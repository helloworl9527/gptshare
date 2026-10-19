package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"
)

var uuid4Pattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type userCenterFixture struct {
	repo *Repository
	db   *sql.DB
	now  time.Time
}

func newUserCenterFixture(t *testing.T) userCenterFixture {
	t.Helper()
	database := openStore(t)
	t.Cleanup(func() { database.Close() })
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 10, 18, 8, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "share-07@pool.example.test", DisplayPassword: "UC_PASSWORD_SENTINEL", DisplayTOTPSecret: "UC_TOTP_SENTINEL",
		AccountExpiry: now.Add(25 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	return userCenterFixture{repo: repo, db: database.DB(), now: now}
}

func (f userCenterFixture) card(t *testing.T, code string) int64 {
	t.Helper()
	id, err := f.repo.CreateCard(context.Background(), CardSeed{CodeHash: cardHashForCode(code), CodeSuffix: code[len(code)-4:], CodePlaintext: code, DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (f userCenterFixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var value int
	if err := f.db.QueryRow(query, args...).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func testBinding(nonce string) *RedemptionBinding {
	return &RedemptionBinding{UserUID: "u_9f3k2m7q", Nonce: nonce, NonceExp: time.Date(2026, 10, 18, 8, 5, 0, 0, time.UTC).Unix(), CodeHashHex: strings.Repeat("ab", 32)}
}

func TestRedeemWithBindingWritesEventAndNonceInSameTransaction(t *testing.T) {
	f := newUserCenterFixture(t)
	cardID := f.card(t, "UCAA-BBCC-DDEE")
	result, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-0000000000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if result.Card.ID != cardID || result.Card.Status != "redeemed" {
		t.Fatalf("unexpected redeem result: %+v", result.Card)
	}
	events, err := f.repo.ListRedemptionEvents(context.Background(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("events=%d want 1", len(events))
	}
	event := events[0]
	if event.Kind != RedemptionKindNew || event.EntitlementRef != CardEntitlementRef(cardID) || event.UserUID != "u_9f3k2m7q" ||
		event.CodeHash != strings.Repeat("ab", 32) || event.SucceededAt != "2026-10-18T08:00:00Z" || !uuid4Pattern.MatchString(event.EventUID) {
		t.Fatalf("unexpected event: %+v", event)
	}
	var detail map[string]string
	if err := json.Unmarshal([]byte(event.Detail), &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail) != 2 || detail["plan"] != "30天" || detail["expire_at"] != "2026-11-17" {
		t.Fatalf("unexpected detail: %s", event.Detail)
	}
	for _, forbidden := range []string{"UC_PASSWORD_SENTINEL", "UC_TOTP_SENTINEL", "UCAA", "share-07", "account"} {
		if strings.Contains(event.Detail, forbidden) {
			t.Fatalf("event detail leaked %q: %s", forbidden, event.Detail)
		}
	}
	if got := f.count(t, `SELECT count(*) FROM used_bind_nonces WHERE nonce='nonce-0000000000000001'`); got != 1 {
		t.Fatalf("nonce rows=%d want 1", got)
	}
}

func TestRedeemWithReplayedNonceIsRejectedAndConsumesNothing(t *testing.T) {
	f := newUserCenterFixture(t)
	f.card(t, "UCAA-BBCC-DDEE")
	secondID := f.card(t, "UCFF-GGHH-JJKK")
	if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-replayed-0000001")); err != nil {
		t.Fatal(err)
	}
	_, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCFF-GGHH-JJKK"), true, testBinding("nonce-replayed-0000001"))
	if !errors.Is(err, ErrBindNonceReused) {
		t.Fatalf("replayed nonce err=%v want ErrBindNonceReused", err)
	}
	card, err := f.repo.CardByID(context.Background(), secondID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != "unused" {
		t.Fatalf("replayed ticket consumed the card: status=%s", card.Status)
	}
	if got := f.count(t, `SELECT count(*) FROM allocations WHERE card_id=?`, secondID); got != 0 {
		t.Fatalf("replayed ticket created allocations: %d", got)
	}
	if got := f.count(t, `SELECT count(*) FROM redemption_events`); got != 1 {
		t.Fatalf("events=%d want 1", got)
	}
}

// 事务里任何一步失败，扣码、分配、nonce、事件都不能留下任何一个。
func TestRedeemWithBindingRollsBackEverythingOnFailure(t *testing.T) {
	for name, trigger := range map[string]string{
		"event insert fails": `CREATE TRIGGER fail_event BEFORE INSERT ON redemption_events BEGIN SELECT RAISE(ABORT, 'injected event failure'); END`,
		"card update fails":  `CREATE TRIGGER fail_card BEFORE UPDATE OF status ON cards WHEN NEW.status='redeemed' BEGIN SELECT RAISE(ABORT, 'injected card failure'); END`,
		"allocation fails":   `CREATE TRIGGER fail_allocation BEFORE INSERT ON allocations BEGIN SELECT RAISE(ABORT, 'injected allocation failure'); END`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newUserCenterFixture(t)
			cardID := f.card(t, "UCAA-BBCC-DDEE")
			if _, err := f.db.Exec(trigger); err != nil {
				t.Fatal(err)
			}
			if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-rollback-0000001")); err == nil {
				t.Fatal("expected injected failure")
			}
			card, err := f.repo.CardByID(context.Background(), cardID)
			if err != nil {
				t.Fatal(err)
			}
			if card.Status != "unused" {
				t.Fatalf("card status=%s want unused", card.Status)
			}
			for table, query := range map[string]string{
				"allocations":       `SELECT count(*) FROM allocations`,
				"used_bind_nonces":  `SELECT count(*) FROM used_bind_nonces`,
				"redemption_events": `SELECT count(*) FROM redemption_events`,
				"occupied capacity": `SELECT coalesce(sum(current_allocations),0) FROM chatgpt_accounts`,
			} {
				if got := f.count(t, query); got != 0 {
					t.Fatalf("%s=%d after rolled back redeem", table, got)
				}
			}
		})
	}
}

func TestRedeemAlreadyRedeemedCardWithBindingWritesNothing(t *testing.T) {
	f := newUserCenterFixture(t)
	cardID := f.card(t, "UCAA-BBCC-DDEE")
	first, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-first-0000000001"))
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-second-000000001"))
	if err != nil {
		t.Fatal(err)
	}
	if again.Card.ID != cardID || again.Allocation.ID != first.Allocation.ID {
		t.Fatalf("existing redeem must return the original result: first=%d again=%d", first.Allocation.ID, again.Allocation.ID)
	}
	if got := f.count(t, `SELECT count(*) FROM redemption_events`); got != 1 {
		t.Fatalf("events=%d want 1", got)
	}
	if got := f.count(t, `SELECT count(*) FROM used_bind_nonces WHERE nonce='nonce-second-000000001'`); got != 0 {
		t.Fatal("querying an already redeemed card must not consume the ticket nonce")
	}
}

func TestAnonymousRedeemWritesNoBindingRows(t *testing.T) {
	f := newUserCenterFixture(t)
	f.card(t, "UCAA-BBCC-DDEE")
	if _, err := f.repo.RedeemCode(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true); err != nil {
		t.Fatal(err)
	}
	if got := f.count(t, `SELECT count(*) FROM redemption_events`) + f.count(t, `SELECT count(*) FROM used_bind_nonces`); got != 0 {
		t.Fatalf("anonymous redeem wrote %d binding rows", got)
	}
}

func TestListRedemptionEventsPaginatesInIDOrder(t *testing.T) {
	f := newUserCenterFixture(t)
	codes := []string{"UCAA-AAAA-AAA2", "UCAA-AAAA-AAA3", "UCAA-AAAA-AAA4", "UCAA-AAAA-AAA5", "UCAA-AAAA-AAA6"}
	for index, code := range codes {
		f.card(t, code)
		if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode(code), true, testBinding("nonce-page-00000000000"+string(rune('a'+index)))); err != nil {
			t.Fatal(err)
		}
	}
	var seen []int64
	after := int64(0)
	for {
		page, err := f.repo.ListRedemptionEvents(context.Background(), after, 2)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			break
		}
		if len(page) > 2 {
			t.Fatalf("page size=%d exceeds limit", len(page))
		}
		for _, event := range page {
			seen = append(seen, event.ID)
			after = event.ID
		}
	}
	if len(seen) != len(codes) {
		t.Fatalf("paged events=%v", seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] <= seen[i-1] {
			t.Fatalf("events not in ascending id order: %v", seen)
		}
	}
}

func TestBoundEntitlementsOnlyExposeBoundCards(t *testing.T) {
	f := newUserCenterFixture(t)
	boundID := f.card(t, "UCAA-BBCC-DDEE")
	anonymousID := f.card(t, "UCFF-GGHH-JJKK")
	if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCAA-BBCC-DDEE"), true, testBinding("nonce-bound-0000000001")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.RedeemCode(context.Background(), cardHashForCode("UCFF-GGHH-JJKK"), true); err != nil {
		t.Fatal(err)
	}
	items, err := f.repo.BoundEntitlements(context.Background(), []int64{boundID, anonymousID, 9999, boundID})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("bound entitlements=%d want 1: %+v", len(items), items)
	}
	item := items[boundID]
	if item.Status != "redeemed" || item.DurationDays != 30 || item.ExpiresAt == nil || item.CurrentUsername != "share-07@pool.example.test" {
		t.Fatalf("unexpected entitlement: %+v", item)
	}
	code, err := f.repo.BoundCardCode(context.Background(), boundID)
	if err != nil || code != "UCAA-BBCC-DDEE" {
		t.Fatalf("bound card code=%q err=%v", code, err)
	}
	if _, err := f.repo.BoundCardCode(context.Background(), anonymousID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("anonymous card code must not be revealed, err=%v", err)
	}
}

func TestPurgeExpiredBindNonces(t *testing.T) {
	f := newUserCenterFixture(t)
	if _, err := f.db.Exec(`INSERT INTO used_bind_nonces(nonce,exp) VALUES ('old-nonce',100),('fresh-nonce',?)`, f.now.Unix()); err != nil {
		t.Fatal(err)
	}
	removed, err := f.repo.PurgeExpiredBindNonces(context.Background(), f.now.Add(-5*time.Minute).Unix())
	if err != nil || removed != 1 {
		t.Fatalf("removed=%d err=%v", removed, err)
	}
	if got := f.count(t, `SELECT count(*) FROM used_bind_nonces WHERE nonce='fresh-nonce'`); got != 1 {
		t.Fatal("fresh nonce must be kept")
	}
}

func TestParseCardEntitlementRef(t *testing.T) {
	if id, ok := ParseCardEntitlementRef("card:12"); !ok || id != 12 {
		t.Fatalf("card:12 => %d %v", id, ok)
	}
	for _, ref := range []string{"", "card:", "card:0", "card:-1", "card:012", "card:1a", "account:12", "card: 12", "card:99999999999999999999"} {
		if _, ok := ParseCardEntitlementRef(ref); ok {
			t.Fatalf("%q must be rejected", ref)
		}
	}
}

func TestBoundRedeemBackfillsLegacyCardCode(t *testing.T) {
	f := newUserCenterFixture(t)
	legacyID := f.card(t, "UCLL-MMNN-PPQQ")
	anonymousLegacyID := f.card(t, "UCRR-SSTT-UUVV")
	if _, err := f.db.Exec(`UPDATE cards SET encrypted_code_key_id=NULL, encrypted_code=NULL WHERE id IN (?,?)`, legacyID, anonymousLegacyID); err != nil {
		t.Fatal(err)
	}
	binding := testBinding("nonce-legacy-000000001")
	binding.CodePlaintext = "UCLL-MMNN-PPQQ"
	if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCLL-MMNN-PPQQ"), true, binding); err != nil {
		t.Fatal(err)
	}
	code, err := f.repo.BoundCardCode(context.Background(), legacyID)
	if err != nil || code != "UCLL-MMNN-PPQQ" {
		t.Fatalf("legacy bound card code=%q err=%v", code, err)
	}
	// 匿名兑换不补存
	if _, err := f.repo.RedeemCode(context.Background(), cardHashForCode("UCRR-SSTT-UUVV"), true); err != nil {
		t.Fatal(err)
	}
	var stored int
	if err := f.db.QueryRow(`SELECT count(*) FROM cards WHERE id=? AND encrypted_code IS NOT NULL`, anonymousLegacyID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("anonymous legacy card must stay unarchived, stored=%d err=%v", stored, err)
	}
}

// 老卡补挂：匿名兑换过、仍有效、没有归属事件的卡，登录用户再输入时归到他名下（claimed、不计返利），
// 别人再输入是 already_bound；不写第二条事件、不消耗票据。老批次卡顺手补存加密卡密。
func TestClaimOldAnonymousCardOnce(t *testing.T) {
	f := newUserCenterFixture(t)
	cardID := f.card(t, "UCMM-NNPP-QQRR")
	if _, err := f.db.Exec(`UPDATE cards SET encrypted_code_key_id=NULL, encrypted_code=NULL WHERE id=?`, cardID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.RedeemCode(context.Background(), cardHashForCode("UCMM-NNPP-QQRR"), true); err != nil {
		t.Fatal(err)
	}
	alice := testBinding("nonce-claim-alice-0001")
	alice.CodePlaintext = "UCMM-NNPP-QQRR"
	got, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCMM-NNPP-QQRR"), true, alice)
	if err != nil || got.Bind != "claimed" || got.Card.ID != cardID {
		t.Fatalf("claim result=%+v err=%v", got.Bind, err)
	}
	events, err := f.repo.ListRedemptionEvents(context.Background(), 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	if events[0].Kind != "new" || !strings.Contains(events[0].Detail, `"claimed":true`) || !strings.Contains(events[0].Detail, `"rebate_eligible":false`) {
		t.Fatalf("claim event=%+v", events[0])
	}
	if code, err := f.repo.BoundCardCode(context.Background(), cardID); err != nil || code != "UCMM-NNPP-QQRR" {
		t.Fatalf("bound card code=%q err=%v", code, err)
	}
	bob := testBinding("nonce-claim-bob-000001")
	bob.UserUID = "u_bob00001"
	again, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCMM-NNPP-QQRR"), true, bob)
	if err != nil || again.Bind != "already_bound" {
		t.Fatalf("second claim=%q err=%v", again.Bind, err)
	}
	var nonces int
	if err := f.db.QueryRow(`SELECT count(*) FROM used_bind_nonces WHERE nonce=?`, bob.Nonce).Scan(&nonces); err != nil || nonces != 0 {
		t.Fatalf("already_bound must not consume nonce: %d %v", nonces, err)
	}
	if events, _ := f.repo.ListRedemptionEvents(context.Background(), 0, 10); len(events) != 1 {
		t.Fatalf("already_bound must not write events: %d", len(events))
	}
}

func TestClaimRejectsExpiredCardAndReplayedNonce(t *testing.T) {
	f := newUserCenterFixture(t)
	expiredID := f.card(t, "UCSS-TTUU-VVWW")
	f.card(t, "UCXX-YYZZ-2345")
	for _, code := range []string{"UCSS-TTUU-VVWW", "UCXX-YYZZ-2345"} {
		if _, err := f.repo.RedeemCode(context.Background(), cardHashForCode(code), true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.Exec(`UPDATE cards SET expires_at=? WHERE id=?`, "2020-01-01T00:00:00Z", expiredID); err != nil {
		t.Fatal(err)
	}
	got, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCSS-TTUU-VVWW"), true, testBinding("nonce-claim-expired-01"))
	if err != nil || got.Bind != "not_claimable" {
		t.Fatalf("expired claim=%q err=%v", got.Bind, err)
	}
	if _, err := f.db.Exec(`INSERT INTO used_bind_nonces(nonce,exp) VALUES ('nonce-claim-replay-001', 9999999999)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.repo.RedeemCodeWithBinding(context.Background(), cardHashForCode("UCXX-YYZZ-2345"), true, testBinding("nonce-claim-replay-001")); !errors.Is(err, ErrBindNonceReused) {
		t.Fatalf("replayed nonce err=%v", err)
	}
	if events, _ := f.repo.ListRedemptionEvents(context.Background(), 0, 10); len(events) != 0 {
		t.Fatalf("no events expected, got %d", len(events))
	}
}

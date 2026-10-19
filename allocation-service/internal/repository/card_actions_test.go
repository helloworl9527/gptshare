package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func actionFixture(t *testing.T) (*Repository, *sql.DB, *time.Time) {
	t.Helper()
	db := openStore(t)
	t.Cleanup(func() { db.Close() })
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	return repo, db.DB(), &now
}

func actionCardSeed(t *testing.T, repo *Repository, seed, days int) int64 {
	t.Helper()
	id, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(seed), CodeSuffix: suffixFor(seed), DurationDays: days})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func actionAccountSeed(t *testing.T, repo *Repository, now time.Time, name string) int64 {
	t.Helper()
	id, err := repo.CreateAccount(context.Background(), AccountSeed{DisplayUsername: name, DisplayPassword: "test-only-password", DisplayTOTPSecret: "test-only-totp", AccountExpiry: now.AddDate(0, 0, 30), MaxConcurrentUsers: 5, MonitorStatus: "alive", Status: "available"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func actionSQLCount(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestCardActionUnusedMergePreviewReplayAndNoActivation(t *testing.T) {
	repo, db, _ := actionFixture(t)
	target := actionCardSeed(t, repo, 500, 90)
	source := actionCardSeed(t, repo, 501, 30)
	preview, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(500), hashFor(501), true, true)
	if err != nil || preview.Mode != "unused" || preview.DurationDays != 120 || preview.ExpiresAt != nil {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	for _, q := range []string{"SELECT count(*) FROM allocations", "SELECT count(*) FROM card_operations", "SELECT count(*) FROM card_groups"} {
		if actionSQLCount(t, db, q) != 0 {
			t.Fatalf("preview changed %s", q)
		}
	}
	before, _ := repo.CardByID(context.Background(), target)
	if before.DurationDays != 90 {
		t.Fatal("preview persisted duration")
	}
	result, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(500), hashFor(501), false, true)
	if err != nil || result.DurationDays != 120 || result.Status != "unused" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	consumed, _ := repo.CardByID(context.Background(), source)
	if consumed.Status != "consumed" || consumed.RedeemedAt != nil {
		t.Fatalf("source=%+v", consumed)
	}
	if _, err := repo.RedeemCard(context.Background(), source); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("consumed redeem=%v", err)
	}
	if _, err := repo.ExtendCard(context.Background(), source, 1); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("consumed extension=%v", err)
	}
	if _, err := repo.RevokeCard(context.Background(), source); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("consumed revocation=%v", err)
	}
	replay, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(500), hashFor(501), false, true)
	if err != nil || !replay.Replayed || replay.DurationDays != 120 {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	third := actionCardSeed(t, repo, 502, 7)
	if _, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(502), hashFor(501), false, true); !errors.Is(err, ErrCardActionConflict) {
		t.Fatalf("other target=%d error=%v", third, err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM card_operations") != 1 || actionSQLCount(t, db, "SELECT count(*) FROM allocations") != 0 {
		t.Fatal("merge or replay created extra effects")
	}
	history, err := repo.CardOperations(context.Background(), source)
	if err != nil || len(history) != 1 || history[0].TargetCardID != target || history[0].AddedDays != 30 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestCardActionRenewConcurrentAddsExactlyOnce(t *testing.T) {
	repo, db, now := actionFixture(t)
	account := actionAccountSeed(t, repo, *now, "original@example.test")
	target := actionCardSeed(t, repo, 510, 7)
	actionCardSeed(t, repo, 511, 90)
	allocation, err := repo.RedeemCard(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan CardActionResult, 20)
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := repo.ApplyCardAction(context.Background(), "renew", hashFor(510), hashFor(511), false, true)
			results <- r
			errs <- e
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	committed := 0
	for result := range results {
		if !result.Replayed {
			committed++
		}
		if result.AccountChanged {
			t.Fatal("usable original changed")
		}
	}
	card, _ := repo.CardByID(context.Background(), target)
	if committed != 1 || card.DurationDays != 97 || card.ExpiresAt == nil || !card.ExpiresAt.Equal(now.AddDate(0, 0, 97)) {
		t.Fatalf("commits=%d card=%+v", committed, card)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM card_operations") != 1 || actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE active=1") != 1 {
		t.Fatal("extra effects")
	}
	var until string
	if err := db.QueryRow("SELECT valid_until FROM allocations WHERE id=? AND account_id=?", allocation.ID, account).Scan(&until); err != nil || until != formatTime(*card.ExpiresAt) {
		t.Fatalf("allocation expiry=%s err=%v", until, err)
	}
}

func TestCardActionExpiredRenewPrefersOriginalAndRollsBackNoCapacity(t *testing.T) {
	for _, available := range []bool{true, false} {
		t.Run(fmt.Sprint(available), func(t *testing.T) {
			repo, db, now := actionFixture(t)
			original := actionAccountSeed(t, repo, *now, "previous@example.test")
			target := actionCardSeed(t, repo, 520, 1)
			source := actionCardSeed(t, repo, 521, 7)
			if _, err := repo.RedeemCard(context.Background(), target); err != nil {
				t.Fatal(err)
			}
			*now = now.AddDate(0, 0, 2)
			if !available {
				if _, err := db.Exec("UPDATE chatgpt_accounts SET status='disabled' WHERE id=?", original); err != nil {
					t.Fatal(err)
				}
			}
			result, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(520), hashFor(521), false, true)
			if !available {
				if !errors.Is(err, ErrNoAccountCapacity) {
					t.Fatalf("error=%v", err)
				}
				card, _ := repo.CardByID(context.Background(), target)
				newCard, _ := repo.CardByID(context.Background(), source)
				if card.DurationDays != 1 || newCard.Status != "unused" || actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE active=1") != 1 || actionSQLCount(t, db, "SELECT count(*) FROM card_operations") != 0 {
					t.Fatal("failed renewal changed state")
				}
				actionAccountSeed(t, repo, *now, "replacement@example.test")
				result, err = repo.ApplyCardAction(context.Background(), "renew", hashFor(520), hashFor(521), false, true)
				if err != nil || !result.AccountChanged {
					t.Fatalf("fallback=%+v err=%v", result, err)
				}
			} else if err != nil || result.AccountChanged {
				t.Fatalf("retained original=%+v err=%v", result, err)
			}
			if result.Mode != "reactivated" || result.ExpiresAt == nil || !result.ExpiresAt.Equal(now.AddDate(0, 0, 7)) {
				t.Fatalf("result=%+v", result)
			}
			if actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE card_id=? AND active=1", target) != 1 {
				t.Fatal("duplicate allocation after reactivation")
			}
		})
	}
}

func TestCardActionDifferentPhysicalAccountGroupAndFutureRetirement(t *testing.T) {
	repo, db, now := actionFixture(t)
	first := actionAccountSeed(t, repo, *now, "Same@Example.test")
	target := actionCardSeed(t, repo, 530, 7)
	if _, err := repo.RedeemCard(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	// Distinct database row, same login: must not be treated as another account.
	actionAccountSeed(t, repo, *now, " same@example.test ")
	second := actionAccountSeed(t, repo, *now, "different@example.test")
	source := actionCardSeed(t, repo, 531, 7)
	preview, err := repo.ApplyCardAction(context.Background(), "different", hashFor(530), hashFor(531), true, true)
	if err != nil || preview.CardID != source {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM card_groups") != 0 || actionSQLCount(t, db, "SELECT count(*) FROM cards WHERE group_id IS NOT NULL") != 0 {
		t.Fatal("preview persisted grouping")
	}
	result, err := repo.ApplyCardAction(context.Background(), "different", hashFor(530), hashFor(531), false, true)
	if err != nil || result.CardID != source {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE card_id=? AND account_id=? AND active=1", source, second) != 1 {
		t.Fatal("different did not exclude normalized physical login")
	}
	third := actionCardSeed(t, repo, 532, 7)
	if _, err := repo.ApplyCardAction(context.Background(), "different", hashFor(531), hashFor(532), false, true); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("third without capacity=%v", err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM cards WHERE id=? AND group_id IS NULL AND status='unused'", third) != 1 {
		t.Fatal("failed grouping persisted")
	}
	fourthAccount := actionAccountSeed(t, repo, *now, "third@example.test")
	if _, err := repo.ApplyCardAction(context.Background(), "different", hashFor(531), hashFor(532), false, true); err != nil {
		t.Fatal(err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM cards WHERE group_id=(SELECT group_id FROM cards WHERE id=?)", target) != 3 {
		t.Fatal("did not reuse group")
	}
	// Retirement must not collapse the group into another member's account.
	retired, err := repo.RetireAccount(context.Background(), first)
	if !errors.Is(err, ErrAccountReplacementUnavailable) {
		t.Fatalf("retire result=%+v error=%v", retired, err)
	}
	replacement := actionAccountSeed(t, repo, *now, "fourth@example.test")
	retired, err = repo.RetireAccount(context.Background(), first)
	if err != nil || !retired.Archived {
		t.Fatalf("retire=%+v err=%v", retired, err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary' AND account_id=?", target, replacement) != 1 {
		t.Fatal("retirement violated grouping or reused old physical login")
	}
	_ = fourthAccount
}

func TestCardActionDifferentFailureRollsBackAndRedeemRace(t *testing.T) {
	repo, db, now := actionFixture(t)
	actionAccountSeed(t, repo, *now, "only@example.test")
	target := actionCardSeed(t, repo, 540, 7)
	source := actionCardSeed(t, repo, 541, 7)
	if _, err := repo.RedeemCard(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyCardAction(context.Background(), "different", hashFor(540), hashFor(541), false, true); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("no different capacity=%v", err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM card_groups") != 0 || actionSQLCount(t, db, "SELECT count(*) FROM card_operations") != 0 {
		t.Fatal("failed action persisted group or ledger")
	}
	if _, err := repo.RedeemCard(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(540), hashFor(541), false, true); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("used source=%v", err)
	}
	card, _ := repo.CardByID(context.Background(), target)
	if card.DurationDays != 7 {
		t.Fatal("used source extended target")
	}
}

func TestCardTimeAfterDaysRejectsOverflow(t *testing.T) {
	now := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	for _, days := range []int{0, -1, int(^uint(0) >> 1)} {
		if _, err := cardTimeAfterDays(now, days); !errors.Is(err, ErrCardDurationLimit) {
			t.Fatalf("days=%d err=%v", days, err)
		}
	}
	if result, err := cardTimeAfterDays(now, 365); err != nil || !result.Equal(now.AddDate(0, 0, 365)) {
		t.Fatalf("result=%s err=%v", result, err)
	}
}

func TestCardActionGroupExcludesGraceAndAutomaticReplacement(t *testing.T) {
	repo, db, now := actionFixture(t)
	original := actionAccountSeed(t, repo, *now, "grace-original@example.test")
	target := actionCardSeed(t, repo, 550, 7)
	if _, err := repo.RedeemCard(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	occupied := actionAccountSeed(t, repo, *now, "group-primary@example.test")
	actionCardSeed(t, repo, 551, 7)
	if _, err := repo.ApplyCardAction(context.Background(), "different", hashFor(550), hashFor(551), false, true); err != nil {
		t.Fatal(err)
	}
	replacement := actionAccountSeed(t, repo, *now, "replacement-primary@example.test")
	if _, err := db.Exec("UPDATE chatgpt_accounts SET account_expiry=? WHERE id=?", formatTime(now.Add(12*time.Hour)), original); err != nil {
		t.Fatal(err)
	}
	if run, err := repo.ProcessReplacements(context.Background(), *now); err != nil || len(run.Replaced) != 1 {
		t.Fatalf("replacement=%+v err=%v", run, err)
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary' AND account_id=?", target, replacement) != 1 {
		t.Fatal("automatic replacement reused group primary")
	}
	if actionSQLCount(t, db, "SELECT count(*) FROM allocations WHERE card_id=? AND active=1 AND allocation_state='grace' AND account_id=?", target, original) != 1 {
		t.Fatal("expected grace missing")
	}
	actionCardSeed(t, repo, 552, 7)
	if _, err := repo.ApplyCardAction(context.Background(), "different", hashFor(551), hashFor(552), false, true); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("grace not excluded: %v", err)
	}
	// Renewing the target must never prolong an existing grace allocation.
	actionCardSeed(t, repo, 553, 7)
	var graceBefore, validBefore string
	if err := db.QueryRow("SELECT grace_until,valid_until FROM allocations WHERE card_id=? AND active=1 AND allocation_state='grace'", target).Scan(&graceBefore, &validBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ApplyCardAction(context.Background(), "renew", hashFor(550), hashFor(553), false, true); err != nil {
		t.Fatal(err)
	}
	var graceAfter, validAfter string
	if err := db.QueryRow("SELECT grace_until,valid_until FROM allocations WHERE card_id=? AND active=1 AND allocation_state='grace'", target).Scan(&graceAfter, &validAfter); err != nil {
		t.Fatal(err)
	}
	if graceAfter != graceBefore || validAfter != validBefore {
		t.Fatal("renewal prolonged grace")
	}
	_ = occupied
}

func TestCardActionDifferentConcurrentClaimsOneDistinctSeat(t *testing.T) {
	repo, db, now := actionFixture(t)
	actionAccountSeed(t, repo, *now, "first-concurrent@example.test")
	target := actionCardSeed(t, repo, 560, 7)
	if _, err := repo.RedeemCard(context.Background(), target); err != nil {
		t.Fatal(err)
	}
	second := actionAccountSeed(t, repo, *now, "second-concurrent@example.test")
	if _, err := db.Exec("UPDATE chatgpt_accounts SET max_concurrent_users=1 WHERE id=?", second); err != nil {
		t.Fatal(err)
	}
	for i := 561; i < 571; i++ {
		actionCardSeed(t, repo, i, 7)
	}
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := 561; i < 571; i++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			_, err := repo.ApplyCardAction(context.Background(), "different", hashFor(560), hashFor(seed), false, true)
			results <- err
		}(i)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrNoAccountCapacity) {
			t.Fatal(err)
		}
	}
	if successes != 1 || actionSQLCount(t, db, "SELECT count(*) FROM card_operations") != 1 || actionSQLCount(t, db, "SELECT count(*) FROM cards WHERE status='unused'") != 9 {
		t.Fatalf("successes=%d", successes)
	}
	if actionSQLCount(t, db, "SELECT current_allocations FROM chatgpt_accounts WHERE id=?", second) != 1 {
		t.Fatal("seat oversold")
	}
}

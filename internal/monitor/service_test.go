package monitor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/store"
)

type fakeClient struct {
	mu              sync.Mutex
	errors          map[string]*chatgpt.TypedError
	statuses        map[string]chatgpt.StatusResult
	attempts        map[string]int
	failures        int
	delay           time.Duration
	block           <-chan struct{}
	active          atomic.Int32
	max             atomic.Int32
	exchanges       atomic.Int32
	exchangeSecrets []string
	exchangeExpiry  *time.Time
	exchangeIDToken string
}

func (f *fakeClient) ExchangeCredential(_ context.Context, kind chatgpt.CredentialKind, secret string) (chatgpt.TokenSet, error) {
	f.exchanges.Add(1)
	if kind == chatgpt.CredentialRefresh {
		f.mu.Lock()
		f.exchangeSecrets = append(f.exchangeSecrets, secret)
		expiry, idToken := f.exchangeExpiry, f.exchangeIDToken
		f.mu.Unlock()
		return chatgpt.TokenSet{AccessToken: jwtFor("acct-refresh", time.Now().Add(time.Hour)), RefreshToken: secret + "-rotated", IDToken: idToken, AccessExpiresAt: expiry}, nil
	}
	return chatgpt.TokenSet{}, errors.New("unexpected exchange")
}

func (f *fakeClient) FetchStatus(ctx context.Context, access string) (chatgpt.StatusResult, error) {
	pid := tokenPID(access)
	f.mu.Lock()
	if f.attempts == nil {
		f.attempts = make(map[string]int)
	}
	f.attempts[pid]++
	attempt := f.attempts[pid]
	typed := f.errors[pid]
	status := f.statuses[pid]
	f.mu.Unlock()
	active := f.active.Add(1)
	for {
		maximum := f.max.Load()
		if active <= maximum || f.max.CompareAndSwap(maximum, active) {
			break
		}
	}
	defer f.active.Add(-1)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return chatgpt.StatusResult{}, ctx.Err()
		}
	}
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return chatgpt.StatusResult{}, ctx.Err()
		}
	}
	if attempt <= f.failures {
		return chatgpt.StatusResult{}, &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_5xx", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, Retryable: true, PreserveBusinessState: true}
	}
	if typed != nil {
		copy := *typed
		return chatgpt.StatusResult{}, &copy
	}
	if status.ProviderAccountID == "" {
		expiry := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
		status = chatgpt.StatusResult{ProviderAccountID: pid, RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified}
	}
	return status, nil
}

func TestStableBanCandidatesAutomaticallyFinalizeAndDeduplicate(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors = map[string]*chatgpt.TypedError{"acct-a": candidate("credential_revoked")}
	id := seedAccount(t, db, keyring, "acct-a", now.Add(10*24*time.Hour), now.Add(-2*24*time.Hour))
	run, done, err := s.RefreshNow(context.Background(), id)
	if err != nil || !done || run.State != "completed" {
		t.Fatalf("refresh run=%+v done=%v err=%v", run, done, err)
	}
	assertAccountState(t, db, id, StateDeadBanned, CheckOK, false)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 1)
	var days float64
	if err := db.QueryRow("SELECT banned_survival_days FROM accounts WHERE id=?", id).Scan(&days); err != nil || days != 2 {
		t.Fatalf("survival=%v err=%v", days, err)
	}

	second := seedAccount(t, db, keyring, "acct-b", now.Add(10*24*time.Hour), now.Add(-time.Hour))
	client.errors["acct-b"] = candidate("credential_revoked")
	if _, done, err := s.RefreshNow(context.Background(), second); err != nil || !done {
		t.Fatalf("second refresh done=%v err=%v", done, err)
	}
	assertAccountState(t, db, second, StateDeadBanned, CheckOK, false)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 2)
	third := seedAccount(t, db, keyring, "acct-c", now.Add(10*24*time.Hour), now.Add(-time.Hour))
	client.errors["acct-c"] = candidate("account_disabled")
	if _, done, err := s.RefreshNow(context.Background(), third); err != nil || !done {
		t.Fatalf("third refresh done=%v err=%v", done, err)
	}
	assertAccountState(t, db, third, StateDeadBanned, CheckOK, false)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 3)

	for _, code := range []string{"account_deactivated", "token_revoked", "refresh_token_reused"} {
		accountID := seedAccount(t, db, keyring, "acct-"+code, now.Add(10*24*time.Hour), now.Add(-time.Hour))
		client.errors["acct-"+code] = candidate(code)
		if _, done, err := s.RefreshNow(context.Background(), accountID); err != nil || !done {
			t.Fatalf("%s refresh done=%v err=%v", code, done, err)
		}
		assertAccountState(t, db, accountID, StateDeadBanned, CheckOK, false)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 6)
}

func TestRejectAndUnverifiedRemainFailClosed(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors = map[string]*chatgpt.TypedError{
		"acct-reject":  candidate("account_disabled"),
		"acct-unknown": {Kind: chatgpt.ErrorContractChanged, EvidenceCode: "unexpected_http_418", EvidenceLevel: chatgpt.EvidenceUnverified, PreserveBusinessState: true},
	}
	rejected := seedAccount(t, db, keyring, "acct-reject", now.Add(24*time.Hour), now.Add(-time.Hour))
	signature := EvidenceSignature("account_disabled", "status-v1")
	if _, err := db.Exec(`UPDATE accounts SET
		last_check_state='verification_required',last_check_error_code='account_disabled',
		polling_paused=1,pause_reason='evidence_review_required',
		pending_evidence_signature=?,pending_detected_at=?
		WHERE id=?`, signature, formatTime(now), rejected); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReviewEvidence(context.Background(), ReviewRequest{Signature: signature, Decision: ReviewReject, Reason: "ground truth did not confirm terminal status", Operator: "service-user"}); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, rejected, StateAlive, CheckContractChanged, true)
	rejectedAgain := seedAccount(t, db, keyring, "acct-reject-again", now.Add(24*time.Hour), now.Add(-time.Hour))
	client.errors["acct-reject-again"] = candidate("account_disabled")
	if _, _, err := s.RefreshNow(context.Background(), rejectedAgain); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, rejectedAgain, StateAlive, CheckContractChanged, true)
	unknown := seedAccount(t, db, keyring, "acct-unknown", now.Add(24*time.Hour), now.Add(-time.Hour))
	if _, _, err := s.RefreshNow(context.Background(), unknown); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, unknown, StateAlive, CheckContractChanged, true)
	positiveUnknown := seedAccount(t, db, keyring, "acct-positive-unknown", now.Add(24*time.Hour), now.Add(-time.Hour))
	freeExpiry := now.Add(48 * time.Hour)
	client.statuses["acct-positive-unknown"] = chatgpt.StatusResult{ProviderAccountID: "acct-positive-unknown", RawPlan: "chatgptfreeplan", Plan: chatgpt.PlanFree, SubscriptionExpiry: &freeExpiry, AccountState: chatgpt.StateActive, EvidenceCode: "future_success_shape", EvidenceLevel: chatgpt.EvidenceUnverified}
	if _, _, err := s.RefreshNow(context.Background(), positiveUnknown); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, positiveUnknown, StateAlive, CheckContractChanged, true)
	var preservedPlan string
	if err := db.QueryRow("SELECT plan FROM accounts WHERE id=?", positiveUnknown).Scan(&preservedPlan); err != nil || preservedPlan != "plus" {
		t.Fatalf("preserved plan=%s err=%v", preservedPlan, err)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
}

func TestRecoverInterruptedFinalizesLegacyPendingBan(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-legacy-pending", now.Add(24*time.Hour), now.Add(-2*24*time.Hour))
	signature := EvidenceSignature("account_deactivated", "status-v1")
	detected := now.Add(-time.Hour)
	if _, err := db.Exec(`UPDATE accounts SET
		last_check_state='verification_required',last_check_error_code='account_deactivated',
		polling_paused=1,pause_reason='evidence_review_required',
		pending_evidence_signature=?,pending_detected_at=?
		WHERE id=?`, signature, formatTime(detected), id); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, id, StateDeadBanned, CheckOK, false)
	assertCount(t, db, "SELECT count(*) FROM alert_events WHERE account_id="+strconv.FormatInt(id, 10), 1)
	var deadAt, deathType string
	if err := db.QueryRow("SELECT dead_at,death_type FROM accounts WHERE id=?", id).Scan(&deadAt, &deathType); err != nil {
		t.Fatal(err)
	}
	if deadAt != formatTime(detected) || deathType != "abnormal_ban" {
		t.Fatalf("dead_at=%s death_type=%s", deadAt, deathType)
	}
}

func TestRecoverInterruptedSkipsUnknownAndRejectedLegacyEvidence(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	rejected := seedAccount(t, db, keyring, "acct-legacy-rejected", now.Add(24*time.Hour), now.Add(-2*24*time.Hour))
	unknown := seedAccount(t, db, keyring, "acct-legacy-unknown-code", now.Add(24*time.Hour), now.Add(-2*24*time.Hour))
	detected := formatTime(now.Add(-time.Hour))
	rejectedSignature := EvidenceSignature("account_disabled", "status-v1")
	for id, values := range map[int64][2]string{
		rejected: {rejectedSignature, "account_disabled"},
		unknown:  {EvidenceSignature("http_403", "status-v1"), "http_403"},
	} {
		if _, err := db.Exec(`UPDATE accounts SET
			last_check_state='verification_required',last_check_error_code=?,
			polling_paused=1,pause_reason='evidence_review_required',
			pending_evidence_signature=?,pending_detected_at=?
			WHERE id=?`, values[1], values[0], detected, id); err != nil {
			t.Fatal(err)
		}
	}
	rejectedEntry := `{"level":"unverified","decision":"rejected"}`
	if _, err := db.Exec(`INSERT INTO settings(key,value,is_secret,updated_at) VALUES (?, ?,0,?)`,
		"internal.evidence."+rejectedSignature, []byte(rejectedEntry), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, rejected, StateAlive, CheckVerificationRequired, true)
	assertAccountState(t, db, unknown, StateAlive, CheckVerificationRequired, true)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
}

func TestNormalExpiryWinsWithoutUpstreamAndKeepsAccountVisible(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-expired", now, now.Add(-30*24*time.Hour))
	if _, done, err := s.RefreshNow(context.Background(), id); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	assertAccountState(t, db, id, StateDeadNormal, CheckOK, false)
	accountService, err := account.NewService(db, client, keyring)
	if err != nil {
		t.Fatal(err)
	}
	visible, err := accountService.Get(context.Background(), id)
	if err != nil || visible.Status != StateDeadNormal {
		t.Fatalf("visible=%+v err=%v", visible, err)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
	client.mu.Lock()
	attempts := client.attempts["acct-expired"]
	client.mu.Unlock()
	if attempts != 0 {
		t.Fatalf("expired account made %d upstream calls", attempts)
	}
	var survival sql.NullFloat64
	if err := db.QueryRow("SELECT banned_survival_days FROM accounts WHERE id=?", id).Scan(&survival); err != nil || survival.Valid {
		t.Fatalf("normal expiry survival=%v err=%v", survival, err)
	}
}

func TestOrdinaryErrorsRetryAndNeverBan(t *testing.T) {
	tests := []struct {
		name   string
		typed  *chatgpt.TypedError
		paused bool
	}{
		{"permission", &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, EvidenceCode: "http_403", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}, false},
		{"rate", &chatgpt.TypedError{Kind: chatgpt.ErrorRateLimited, EvidenceCode: "http_429", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, Retryable: true, RetryAfter: time.Millisecond, PreserveBusinessState: true}, false},
		{"server", &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_5xx", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, Retryable: true, PreserveBusinessState: true}, false},
		{"timeout", &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_timeout", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, Retryable: true, PreserveBusinessState: true}, false},
		{"html", &chatgpt.TypedError{Kind: chatgpt.ErrorContractChanged, EvidenceCode: "unexpected_non_json", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s, db, keyring, client, now := newTestService(t)
			s.sleep = func(context.Context, time.Duration) error { return nil }
			client.errors = map[string]*chatgpt.TypedError{"acct-" + test.name: test.typed}
			id := seedAccount(t, db, keyring, "acct-"+test.name, now.Add(time.Hour), now.Add(-time.Hour))
			if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			var state string
			var paused bool
			if err := db.QueryRow("SELECT status,polling_paused FROM accounts WHERE id=?", id).Scan(&state, &paused); err != nil {
				t.Fatal(err)
			}
			if state != StateAlive || paused != test.paused {
				t.Fatalf("state=%s paused=%v", state, paused)
			}
			assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
		})
	}
}

func TestRetryWorkerLimitRoundOverlapAndRecovery(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	s.sleep = func(context.Context, time.Duration) error { return nil }
	client.failures = 2
	client.delay = 10 * time.Millisecond
	for i := 0; i < 7; i++ {
		seedAccount(t, db, keyring, "acct-worker-"+string(rune('a'+i)), now.Add(time.Hour), now)
	}
	run, err := s.RunScheduled(context.Background())
	if err != nil || run.AccountsOK != 7 || run.AccountsFailed != 0 {
		t.Fatalf("run=%+v err=%v", run, err)
	}
	if client.max.Load() > 3 {
		t.Fatalf("max concurrency=%d", client.max.Load())
	}

	block := make(chan struct{})
	client.block = block
	for i := 0; i < 7; i++ {
		_, _ = db.Exec("UPDATE accounts SET next_retry_at=NULL WHERE provider_account_id=?", "acct-worker-"+string(rune('a'+i)))
	}
	done := make(chan Run, 1)
	go func() { result, _ := s.RunScheduled(context.Background()); done <- result }()
	time.Sleep(30 * time.Millisecond)
	skipped, err := s.RunScheduled(context.Background())
	if err != nil || skipped.State != "skipped" {
		t.Fatalf("skipped=%+v err=%v", skipped, err)
	}
	close(block)
	<-done
	_, err = db.Exec(`INSERT INTO poll_runs(id,started_at,state,accounts_total,trigger_type,error_counts_json) VALUES ('orphan','2026-07-22T00:00:00Z','running',0,'scheduled','{}')`)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.GetRun(context.Background(), "orphan")
	if err != nil || recovered.State != "interrupted" {
		t.Fatalf("recovered=%+v err=%v", recovered, err)
	}
}

func TestRefreshRotatesCredentialWithoutChangingAuthExpiry(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	authExpiry := now.Add(10 * 24 * time.Hour)
	id := seedAccountWithRefresh(t, db, keyring, "acct-refresh", authExpiry, now)
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var storedExpiry string
	var envelope []byte
	if err := db.QueryRow("SELECT auth_expiry,enc_credentials FROM accounts WHERE id=?", id).Scan(&storedExpiry, &envelope); err != nil {
		t.Fatal(err)
	}
	if storedExpiry != formatTime(authExpiry) {
		t.Fatalf("auth expiry changed to %s", storedExpiry)
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(id, "refresh"))
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	if string(plaintext) == "" || !json.Valid(plaintext) {
		t.Fatal("rotated credential is not encrypted JSON")
	}
	client.mu.Lock()
	attempts := client.attempts["acct-refresh"]
	client.mu.Unlock()
	if attempts != 1 {
		t.Fatalf("refreshed status calls=%d", attempts)
	}
	if client.exchanges.Load() != 1 {
		t.Fatalf("refresh exchanges=%d", client.exchanges.Load())
	}
}

func TestTransientStatusAfterRefreshDoesNotReuseRotatedRefreshToken(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	s.sleep = func(context.Context, time.Duration) error { return nil }
	client.failures = 1
	id := seedAccountWithRefresh(t, db, keyring, "acct-refresh", now.Add(24*time.Hour), now)
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if client.exchanges.Load() != 1 {
		t.Fatalf("rotating refresh credential exchanged %d times", client.exchanges.Load())
	}
	client.mu.Lock()
	attempts := client.attempts["acct-refresh"]
	client.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("status attempts=%d", attempts)
	}
}

func TestGenericAccountCheckDenialAfterRefreshPersistsCredentialsAndSucceeds(t *testing.T) {
	for _, test := range []struct {
		name   string
		code   string
		status int
	}{
		{name: "unauthorized", code: "http_401", status: 401},
		{name: "forbidden", code: "http_403", status: 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			s, db, keyring, client, initialNow := newTestService(t)
			pollNow := initialNow
			s.now = func() time.Time { return pollNow }
			accessExpiry := initialNow.Add(time.Minute)
			client.exchangeExpiry = &accessExpiry
			client.exchangeIDToken = "rotated-id-token"
			client.errors["acct-refresh"] = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: test.status, EvidenceCode: test.code, EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
			id := seedAccountWithRefresh(t, db, keyring, "acct-refresh", initialNow.Add(24*time.Hour), initialNow)
			if _, err := db.Exec("UPDATE accounts SET last_check_state='error',last_check_error_code=?,plan='plus',raw_plan='chatgptplusplan' WHERE id=?", test.code, id); err != nil {
				t.Fatal(err)
			}
			run, _, err := s.RefreshNow(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if run.AccountsOK != 1 || run.AccountsFailed != 0 || len(run.ErrorCounts) != 0 {
				t.Fatalf("run=%+v", run)
			}
			assertAccountState(t, db, id, StateAlive, CheckOK, false)
			var plan, rawPlan string
			var checkError sql.NullString
			var envelope []byte
			if err := db.QueryRow("SELECT plan,raw_plan,last_check_error_code,enc_credentials FROM accounts WHERE id=?", id).Scan(&plan, &rawPlan, &checkError, &envelope); err != nil {
				t.Fatal(err)
			}
			if plan != "plus" || rawPlan != "chatgptplusplan" || checkError.Valid {
				t.Fatalf("business snapshot changed: plan=%q raw=%q check_error=%v", plan, rawPlan, checkError)
			}
			plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(id, "refresh"))
			if err != nil {
				t.Fatal(err)
			}
			var credentials credentialPayload
			if err := json.Unmarshal(plaintext, &credentials); err != nil {
				t.Fatal(err)
			}
			zero(plaintext)
			if credentials.Refresh != "sanitized-refresh-fixture-rotated" || credentials.IDToken != "rotated-id-token" || credentials.OAuthSource != "refresh" || credentials.AccessExpiresAt == nil || !credentials.AccessExpiresAt.Equal(accessExpiry) {
				t.Fatalf("persisted credentials=%+v", credentials)
			}

			pollNow = initialNow.Add(2 * time.Minute)
			if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			client.mu.Lock()
			secrets := append([]string(nil), client.exchangeSecrets...)
			client.mu.Unlock()
			if len(secrets) != 2 || secrets[1] != "sanitized-refresh-fixture-rotated" {
				t.Fatalf("refresh exchange secrets=%v", secrets)
			}
		})
	}
}

func TestStructuredDenialAfterRefreshStillUsesBanEvidence(t *testing.T) {
	for _, code := range []string{"token_revoked", "account_disabled"} {
		t.Run(code, func(t *testing.T) {
			s, db, keyring, client, now := newTestService(t)
			client.errors["acct-refresh"] = candidate(code)
			id := seedAccountWithRefresh(t, db, keyring, "acct-refresh", now.Add(24*time.Hour), now)
			run, _, err := s.RefreshNow(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if run.AccountsFailed != 1 || run.ErrorCounts[code] != 1 {
				t.Fatalf("run=%+v", run)
			}
			assertAccountState(t, db, id, StateDeadBanned, CheckOK, false)
		})
	}
}

func TestFieldLevelLogsOnlyActualChangesAndAuthSnapshotIsImmutable(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	authExpiry := now.Add(10 * 24 * time.Hour)
	id := seedAccount(t, db, keyring, "acct-fields", authExpiry, now)
	currentExpiry := now.Add(20 * 24 * time.Hour)
	client.statuses["acct-fields"] = chatgpt.StatusResult{ProviderAccountID: "acct-fields", RawPlan: "chatgptfreeplan", Plan: chatgpt.PlanFree, SubscriptionExpiry: &currentExpiry, AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified}
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE field IN ('plan','current_expiry')", 2)
	if _, err := db.Exec("UPDATE accounts SET next_retry_at=NULL WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE field IN ('plan','current_expiry')", 2)
	var storedAuth, plan, current string
	if err := db.QueryRow("SELECT auth_expiry,plan,current_expiry FROM accounts WHERE id=?", id).Scan(&storedAuth, &plan, &current); err != nil {
		t.Fatal(err)
	}
	if storedAuth != formatTime(authExpiry) || plan != "free" || current != formatTime(currentExpiry) {
		t.Fatalf("auth=%s plan=%s current=%s", storedAuth, plan, current)
	}
}

func TestSuccessfulRefreshBackfillsEmailAndDefaultLabelOnly(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-backfill", now.Add(24*time.Hour), now)
	expiry := now.Add(48 * time.Hour)
	client.statuses["acct-backfill"] = chatgpt.StatusResult{ProviderAccountID: "acct-backfill", Email: "backfill@example.test", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified}
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	var email, label string
	if err := db.QueryRow("SELECT email,label FROM accounts WHERE id=?", id).Scan(&email, &label); err != nil {
		t.Fatal(err)
	}
	if email != "backfill@example.test" || label != "backfill@example.test" {
		t.Fatalf("email=%q label=%q", email, label)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE from_value LIKE '%@example.test%' OR to_value LIKE '%@example.test%'", 0)

	if _, err := db.Exec("UPDATE accounts SET next_retry_at=NULL WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE from_value LIKE '%@example.test%' OR to_value LIKE '%@example.test%'", 0)

	custom := seedAccount(t, db, keyring, "acct-custom-email", now.Add(24*time.Hour), now)
	if _, err := db.Exec("UPDATE accounts SET label='Custom Label' WHERE id=?", custom); err != nil {
		t.Fatal(err)
	}
	client.statuses["acct-custom-email"] = chatgpt.StatusResult{ProviderAccountID: "acct-custom-email", Email: "custom@example.test", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified}
	if _, _, err := s.RefreshNow(context.Background(), custom); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT email,label FROM accounts WHERE id=?", custom).Scan(&email, &label); err != nil {
		t.Fatal(err)
	}
	if email != "custom@example.test" || label != "Custom Label" {
		t.Fatalf("custom email=%q label=%q", email, label)
	}
}

func TestConfiguredIntervalAppliesToNextRoundWithBoundedJitter(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	if _, err := db.Exec("UPDATE settings SET value='1800' WHERE key='poll_interval'"); err != nil {
		t.Fatal(err)
	}
	id := seedAccount(t, db, keyring, "acct-interval", now.Add(24*time.Hour), now)
	if _, err := s.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	var dueText string
	if err := db.QueryRow("SELECT next_retry_at FROM accounts WHERE id=?", id).Scan(&dueText); err != nil {
		t.Fatal(err)
	}
	due, err := parseTime(dueText)
	if err != nil {
		t.Fatal(err)
	}
	if due.Before(now.Add(30*time.Minute)) || due.After(now.Add(33*time.Minute)) {
		t.Fatalf("next due=%s", due)
	}
	if _, err := db.Exec("UPDATE settings SET value='3600' WHERE key='poll_interval'"); err != nil {
		t.Fatal(err)
	}
	second := seedAccount(t, db, keyring, "acct-interval-next", now.Add(24*time.Hour), now)
	if _, err := s.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT next_retry_at FROM accounts WHERE id=?", second).Scan(&dueText); err != nil {
		t.Fatal(err)
	}
	due, err = parseTime(dueText)
	if err != nil {
		t.Fatal(err)
	}
	if due.Before(now.Add(time.Hour)) || due.After(now.Add(66*time.Minute)) {
		t.Fatalf("next-round due=%s", due)
	}
}

func TestManualRefreshTimeoutConflictAndPausedGuard(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	s.cfg.ManualWait = 20 * time.Millisecond
	block := make(chan struct{})
	client.block = block
	id := seedAccount(t, db, keyring, "acct-manual", now.Add(time.Hour), now)
	run, completed, err := s.RefreshNow(context.Background(), id)
	if err != nil || completed || run.State != "running" {
		t.Fatalf("run=%+v completed=%v err=%v", run, completed, err)
	}
	_, _, err = s.RefreshNow(context.Background(), id)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.RunID != run.ID {
		t.Fatalf("conflict=%v", err)
	}
	close(block)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		current, getErr := s.GetRun(context.Background(), run.ID)
		if getErr == nil && current.State == "completed" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := db.Exec("UPDATE accounts SET polling_paused=1,pause_reason='evidence_review_required' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.RefreshNow(context.Background(), id)
	var paused *PausedError
	if !errors.As(err, &paused) {
		t.Fatalf("paused error=%v", err)
	}
}

func TestServiceLockAndPermissionChecks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "lock.db")
	if err := os.WriteFile(dbPath, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDB(dbPath); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireServiceLock(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireServiceLock(dbPath); err == nil {
		t.Fatal("second service lock unexpectedly succeeded")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidatePrivateDB(dbPath); err == nil {
		t.Fatal("insecure database mode was accepted")
	}
	envPath := filepath.Join(dir, "service.env")
	if err := os.WriteFile(envPath, []byte("SANITIZED=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvironmentFile(envPath); err == nil {
		t.Fatal("insecure environment file mode was accepted")
	}
	if err := os.Chmod(envPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateEnvironmentFile(envPath); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceSignatureBindsEndpointCodeAndParserVersion(t *testing.T) {
	base := EvidenceSignatureFor("accounts_check", "credential_revoked", "status-v1")
	for _, changed := range []string{
		EvidenceSignatureFor("oauth_token", "credential_revoked", "status-v1"),
		EvidenceSignatureFor("accounts_check", "account_disabled", "status-v1"),
		EvidenceSignatureFor("accounts_check", "credential_revoked", "status-v2"),
	} {
		if changed == base {
			t.Fatal("evidence signature did not bind all contract dimensions")
		}
	}
}

func candidate(code string) *chatgpt.TypedError {
	kind := chatgpt.ErrorCredentialRevoked
	if code == "account_disabled" || code == "account_deactivated" {
		kind = chatgpt.ErrorAccountDisabled
	}
	return &chatgpt.TypedError{Kind: kind, EvidenceCode: code, EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, BannedCandidate: true, PreserveBusinessState: true}
}

func newTestService(t *testing.T) (*Service, *sql.DB, *credentialcrypto.Keyring, *fakeClient, time.Time) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))
	database, err := store.Open(context.Background(), filepath.Join(dir, "test.db"), os.DirFS(root))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"test": make([]byte, 32)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{errors: make(map[string]*chatgpt.TypedError), statuses: make(map[string]chatgpt.StatusResult), attempts: make(map[string]int)}
	cfg := DefaultConfig()
	cfg.ManualWait = 100 * time.Millisecond
	service, err := New(database.DB(), client, keyring, cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	service.SetBaseContext(context.Background())
	return service, database.DB(), keyring, client, now
}

func seedAccount(t *testing.T, db *sql.DB, keyring *credentialcrypto.Keyring, pid string, authExpiry, imported time.Time) int64 {
	return seedAccountPayload(t, db, keyring, pid, "access", credentialPayload{Access: jwtFor(pid, authExpiry.Add(time.Hour))}, authExpiry, imported)
}

func seedAccountWithRefresh(t *testing.T, db *sql.DB, keyring *credentialcrypto.Keyring, pid string, authExpiry, imported time.Time) int64 {
	return seedAccountPayload(t, db, keyring, pid, "refresh", credentialPayload{Access: jwtFor(pid, imported.Add(-time.Hour)), Refresh: "sanitized-refresh-fixture"}, authExpiry, imported)
}

func seedAccountPayload(t *testing.T, db *sql.DB, keyring *credentialcrypto.Keyring, pid, tokenType string, payload credentialPayload, authExpiry, imported time.Time) int64 {
	t.Helper()
	stamp := formatTime(imported)
	expiry := formatTime(authExpiry)
	result, err := db.Exec(`INSERT INTO accounts(provider_account_id,label,token_type,enc_credentials,credential_key_id,plan,raw_plan,current_expiry,auth_expiry,status,last_alive_at,import_time,last_check_state,updated_at)
		VALUES (?,?,?,x'','test','plus','chatgptplusplan',?,?,'alive',?,?,'ok',?)`, pid, pid, tokenType, expiry, expiry, stamp, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	plaintext, _ := json.Marshal(payload)
	envelope, err := keyring.Seal(plaintext, credentialcrypto.CredentialAAD(id, tokenType))
	zero(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE accounts SET enc_credentials=? WHERE id=?", envelope, id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", id, stamp, expiry); err != nil {
		t.Fatal(err)
	}
	return id
}

func jwtFor(pid string, expiry time.Time) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix(), "pid": pid})
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
}

func tokenPID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var claims struct {
		PID string `json:"pid"`
	}
	_ = json.Unmarshal(payload, &claims)
	return claims.PID
}

func assertAccountState(t *testing.T, db *sql.DB, id int64, status, check string, paused bool) {
	t.Helper()
	var gotStatus, gotCheck string
	var gotPaused bool
	if err := db.QueryRow("SELECT status,last_check_state,polling_paused FROM accounts WHERE id=?", id).Scan(&gotStatus, &gotCheck, &gotPaused); err != nil {
		t.Fatal(err)
	}
	if gotStatus != status || gotCheck != check || gotPaused != paused {
		t.Fatalf("state=(%s,%s,%v), want=(%s,%s,%v)", gotStatus, gotCheck, gotPaused, status, check, paused)
	}
}

func assertCount(t *testing.T, db *sql.DB, query string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(query).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("count=%d want=%d", got, want)
	}
}

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
	exchangeErr     *chatgpt.TypedError
	exchangeHook    func(chatgpt.CredentialKind, string) (chatgpt.TokenSet, error)
}

func (f *fakeClient) ExchangeCredential(_ context.Context, kind chatgpt.CredentialKind, secret string) (chatgpt.TokenSet, error) {
	f.exchanges.Add(1)
	f.mu.Lock()
	hook, configuredErr := f.exchangeHook, f.exchangeErr
	f.mu.Unlock()
	if hook != nil {
		return hook(kind, secret)
	}
	if configuredErr != nil {
		copy := *configuredErr
		return chatgpt.TokenSet{}, &copy
	}
	if kind == chatgpt.CredentialRefresh {
		f.mu.Lock()
		f.exchangeSecrets = append(f.exchangeSecrets, secret)
		expiry, idToken := f.exchangeExpiry, f.exchangeIDToken
		f.mu.Unlock()
		return chatgpt.TokenSet{AccessToken: jwtFor("acct-refresh", time.Now().Add(time.Hour)), RefreshToken: secret + "-rotated", IDToken: idToken, AccessExpiresAt: expiry}, nil
	}
	return chatgpt.TokenSet{}, errors.New("unexpected exchange")
}

func TestOAuthRefreshFailureRequiresReauthorizationWithoutChangingBusinessState(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccountPayload(t, db, keyring, "acct-refresh", "refresh", credentialPayload{
		Access: jwtFor("acct-refresh", now.Add(-time.Hour)), Refresh: "sanitized-refresh-fixture", OAuthSource: "refresh",
	}, now.Add(24*time.Hour), now.Add(-48*time.Hour))
	client.exchangeErr = authorizationRequired("oauth_refresh_token_reused")

	run, done, err := s.RefreshNow(context.Background(), id)
	if err != nil || !done || run.AccountsFailed != 1 || run.ErrorCounts["oauth_refresh_token_reused"] != 1 {
		t.Fatalf("run=%+v done=%v err=%v", run, done, err)
	}
	assertAccountState(t, db, id, StateAlive, CheckReauthorizationRequired, true)
	var code, pause string
	var nextRetry, endedAt sql.NullString
	if err := db.QueryRow(`SELECT last_check_error_code,pause_reason,next_retry_at,
		(SELECT ended_at FROM authorization_epochs WHERE account_id=accounts.id ORDER BY id DESC LIMIT 1)
		FROM accounts WHERE id=?`, id).Scan(&code, &pause, &nextRetry, &endedAt); err != nil {
		t.Fatal(err)
	}
	if code != "oauth_refresh_token_reused" || pause != "reauthorization_required" || nextRetry.Valid || endedAt.Valid {
		t.Fatalf("code=%q pause=%q retry=%v ended=%v", code, pause, nextRetry, endedAt)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
	if _, _, err := s.RefreshNow(context.Background(), id); err == nil {
		t.Fatal("manual refresh of authorization warning unexpectedly started")
	} else {
		var required *ReauthorizationRequiredError
		if !errors.As(err, &required) {
			t.Fatalf("manual refresh error=%T %v", err, err)
		}
	}
}

func TestLegacyGenericRefreshDenialIsNormalizedToStableOAuthCode(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccountPayload(t, db, keyring, "acct-refresh", "refresh", credentialPayload{
		Access: jwtFor("acct-refresh", now.Add(-time.Hour)), Refresh: "sanitized-refresh-fixture", OAuthSource: "refresh",
	}, now.Add(24*time.Hour), now)
	client.exchangeErr = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: 401, EvidenceCode: "http_401",
		EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	if _, done, err := s.RefreshNow(context.Background(), id); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	assertAccountState(t, db, id, StateAlive, CheckReauthorizationRequired, true)
	var code string
	if err := db.QueryRow("SELECT last_check_error_code FROM accounts WHERE id=?", id).Scan(&code); err != nil || code != "oauth_refresh_unauthorized" {
		t.Fatalf("code=%q err=%v", code, err)
	}
}

func TestConcurrentRefreshCASUsesWinningRotatedCredential(t *testing.T) {
	s1, db, keyring, client, now := newTestService(t)
	s2, err := New(db, client, keyring, s1.cfg)
	if err != nil {
		t.Fatal(err)
	}
	s2.now = s1.now
	s2.SetBaseContext(context.Background())
	id := seedAccountPayload(t, db, keyring, "acct-refresh", "refresh", credentialPayload{
		Access: jwtFor("acct-refresh", now.Add(-time.Hour)), Refresh: "sanitized-refresh-fixture", OAuthSource: "refresh",
	}, now.Add(24*time.Hour), now)

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	var sequence atomic.Int32
	client.exchangeHook = func(kind chatgpt.CredentialKind, secret string) (chatgpt.TokenSet, error) {
		if kind != chatgpt.CredentialRefresh || secret != "sanitized-refresh-fixture" {
			t.Fatalf("unexpected exchange kind=%s secret=%q", kind, secret)
		}
		n := sequence.Add(1)
		arrived <- struct{}{}
		<-release
		access := jwtFor("acct-refresh", now.Add(time.Hour))
		return chatgpt.TokenSet{AccessToken: access, RefreshToken: "winner-candidate-" + strconv.Itoa(int(n))}, nil
	}
	type refreshResult struct {
		run Run
		err error
	}
	results := make(chan refreshResult, 2)
	for _, service := range []*Service{s1, s2} {
		go func(service *Service) {
			run, _, refreshErr := service.RefreshNow(context.Background(), id)
			results <- refreshResult{run: run, err: refreshErr}
		}(service)
	}
	<-arrived
	<-arrived
	close(release)
	for range 2 {
		result := <-results
		if result.err != nil || result.run.AccountsSkipped != 0 {
			t.Fatalf("refresh run=%+v err=%v", result.run, result.err)
		}
	}
	var envelope []byte
	var generation int64
	if err := db.QueryRow("SELECT enc_credentials,credential_generation FROM accounts WHERE id=?", id).Scan(&envelope, &generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("credential_generation=%d want=2", generation)
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(id, "refresh"))
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	var stored credentialPayload
	if json.Unmarshal(plaintext, &stored) != nil || (stored.Refresh != "winner-candidate-1" && stored.Refresh != "winner-candidate-2") {
		t.Fatalf("stored refresh token did not come from CAS winner")
	}
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

func TestSuccessfulStatusCheckClearsHistoricalHTTP401(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-recovered", now.Add(24*time.Hour), now)
	if _, err := db.Exec(`UPDATE accounts SET last_check_state='error',last_check_error_code='http_401',next_retry_at=? WHERE id=?`, formatTime(now), id); err != nil {
		t.Fatal(err)
	}
	run, done, err := s.RefreshNow(context.Background(), id)
	if err != nil || !done || run.AccountsOK != 1 || run.AccountsFailed != 0 {
		t.Fatalf("run=%+v done=%v err=%v", run, done, err)
	}
	assertAccountState(t, db, id, StateAlive, CheckOK, false)
	var checkError sql.NullString
	if err := db.QueryRow("SELECT last_check_error_code FROM accounts WHERE id=?", id).Scan(&checkError); err != nil || checkError.Valid {
		t.Fatalf("last_check_error_code=%v err=%v", checkError, err)
	}
}

func TestExplicitAccountDisabledSignalsAutomaticallyFinalizeAndDeduplicate(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors = map[string]*chatgpt.TypedError{"acct-a": candidate("account_disabled")}
	id := seedAccount(t, db, keyring, "acct-a", now.Add(10*24*time.Hour), now.Add(-2*24*time.Hour))
	run, done, err := s.RefreshNow(context.Background(), id)
	if err != nil || !done || run.State != "completed" {
		t.Fatalf("refresh run=%+v done=%v err=%v", run, done, err)
	}
	assertAccountState(t, db, id, StateDeadBanned, CheckOK, true)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 1)
	var days float64
	if err := db.QueryRow("SELECT banned_survival_days FROM accounts WHERE id=?", id).Scan(&days); err != nil || days != 2 {
		t.Fatalf("survival=%v err=%v", days, err)
	}

	second := seedAccount(t, db, keyring, "acct-b", now.Add(10*24*time.Hour), now.Add(-time.Hour))
	client.errors["acct-b"] = candidate("account_deactivated")
	if _, done, err := s.RefreshNow(context.Background(), second); err != nil || !done {
		t.Fatalf("second refresh done=%v err=%v", done, err)
	}
	assertAccountState(t, db, second, StateDeadBanned, CheckOK, true)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 2)
	third := seedAccount(t, db, keyring, "acct-c", now.Add(10*24*time.Hour), now.Add(-time.Hour))
	client.errors["acct-c"] = candidate("account_disabled")
	if _, done, err := s.RefreshNow(context.Background(), third); err != nil || !done {
		t.Fatalf("third refresh done=%v err=%v", done, err)
	}
	assertAccountState(t, db, third, StateDeadBanned, CheckOK, true)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 3)
}

func TestCredentialRevocationFromStatusDoesNotBan(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	for _, code := range []string{"credential_revoked", "token_revoked", "refresh_token_reused"} {
		id := seedAccount(t, db, keyring, "acct-"+code, now.Add(24*time.Hour), now)
		client.errors["acct-"+code] = candidate(code)
		if _, done, err := s.RefreshNow(context.Background(), id); err != nil || !done {
			t.Fatalf("%s refresh done=%v err=%v", code, done, err)
		}
		assertAccountState(t, db, id, StateAlive, CheckError, false)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
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
	assertAccountState(t, db, id, StateDeadBanned, CheckOK, true)
	assertCount(t, db, "SELECT count(*) FROM alert_events WHERE account_id="+strconv.FormatInt(id, 10), 1)
	var deadAt, deathType string
	if err := db.QueryRow("SELECT dead_at,death_type FROM accounts WHERE id=?", id).Scan(&deadAt, &deathType); err != nil {
		t.Fatal(err)
	}
	if deadAt != formatTime(detected) || deathType != "abnormal_ban" {
		t.Fatalf("dead_at=%s death_type=%s", deadAt, deathType)
	}
}

func TestRecoverInterruptedConvertsLegacyCredentialCandidateToAuthorizationWarning(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-legacy-refresh", now.Add(24*time.Hour), now.Add(-2*24*time.Hour))
	oauthDisabledID := seedAccount(t, db, keyring, "acct-legacy-oauth-disabled", now.Add(24*time.Hour), now.Add(-2*24*time.Hour))
	if _, err := db.Exec(`UPDATE accounts SET
		last_check_state='verification_required',last_check_error_code='refresh_token_reused',
		polling_paused=1,pause_reason='evidence_review_required',
		pending_evidence_signature=?,pending_detected_at=? WHERE id=?`,
		EvidenceSignatureFor("oauth_token", "refresh_token_reused", "status-v1"), formatTime(now.Add(-time.Hour)), id); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE accounts SET
		last_check_state='verification_required',last_check_error_code='account_disabled',
		polling_paused=1,pause_reason='evidence_review_required',
		pending_evidence_signature=?,pending_detected_at=? WHERE id=?`,
		EvidenceSignatureFor("oauth_token", "account_disabled", "status-v1"), formatTime(now.Add(-time.Hour)), oauthDisabledID); err != nil {
		t.Fatal(err)
	}
	if err := s.RecoverInterrupted(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertAccountState(t, db, id, StateAlive, CheckReauthorizationRequired, true)
	var code, pause string
	if err := db.QueryRow("SELECT last_check_error_code,pause_reason FROM accounts WHERE id=?", id).Scan(&code, &pause); err != nil {
		t.Fatal(err)
	}
	if code != "oauth_refresh_token_reused" || pause != "reauthorization_required" {
		t.Fatalf("code=%q pause=%q", code, pause)
	}
	assertAccountState(t, db, oauthDisabledID, StateAlive, CheckReauthorizationRequired, true)
	if err := db.QueryRow("SELECT last_check_error_code,pause_reason FROM accounts WHERE id=?", oauthDisabledID).Scan(&code, &pause); err != nil {
		t.Fatal(err)
	}
	if code != "oauth_refresh_token_invalid" || pause != "reauthorization_required" {
		t.Fatalf("oauth token endpoint code=%q pause=%q", code, pause)
	}
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)
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

func TestRecoverMisclassifiedSupplementalDenialsIsExactAuditedAndIdempotent(t *testing.T) {
	s, db, keyring, _, now := newTestService(t)
	signature := EvidenceSignatureFor("accounts_check", "unexpected_non_json", "status-v1")
	otherSignature := EvidenceSignatureFor("accounts_check", "unexpected_non_json", "status-v2")
	nextRetry := formatTime(now.Add(time.Hour))

	seedCandidate := func(pid, tokenType string) int64 {
		t.Helper()
		payload := credentialPayload{Access: jwtFor(pid, now.Add(time.Hour)), OAuthSource: tokenType}
		if tokenType == "refresh" {
			payload.Refresh = "recovery-refresh-fixture"
		}
		id := seedAccountPayload(t, db, keyring, pid, tokenType, payload, now.Add(24*time.Hour), now.Add(-time.Hour))
		if _, err := db.Exec(`UPDATE accounts SET
			last_check_state='contract_changed',last_check_error_code='unexpected_non_json',next_retry_at=?,
			polling_paused=1,pause_reason='contract_changed',pending_evidence_signature=?,pending_detected_at=?
			WHERE id=?`, nextRetry, signature, formatTime(now.Add(-time.Minute)), id); err != nil {
			t.Fatal(err)
		}
		var epochID int64
		if err := db.QueryRow("SELECT id FROM authorization_epochs WHERE account_id=? AND ended_at IS NULL", id).Scan(&epochID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO status_change_log(
			account_id,epoch_id,at,field,from_value,to_value,evidence_code,evidence_level,evidence_signature)
			VALUES (?,?,?,?,?,?,?,?,?)`, id, epochID, formatTime(now.Add(-time.Minute)), "last_check_state", CheckOK,
			CheckContractChanged, "unexpected_non_json", string(chatgpt.EvidenceContractVerifiedLivePending), signature); err != nil {
			t.Fatal(err)
		}
		return id
	}

	targets := []int64{
		seedCandidate("acct-recovery-refresh", "refresh"),
		seedCandidate("acct-recovery-device", "device"),
	}
	nonTargets := []int64{
		seedCandidate("acct-recovery-access", "access"),
		seedCandidate("acct-recovery-dead", "refresh"),
		seedCandidate("acct-recovery-check", "refresh"),
		seedCandidate("acct-recovery-code", "refresh"),
		seedCandidate("acct-recovery-unpaused", "refresh"),
		seedCandidate("acct-recovery-signature", "refresh"),
	}
	mutations := []struct {
		id    int64
		query string
		args  []any
	}{
		{nonTargets[1], "UPDATE accounts SET status='dead_normal' WHERE id=?", nil},
		{nonTargets[2], "UPDATE accounts SET last_check_state='error' WHERE id=?", nil},
		{nonTargets[3], "UPDATE accounts SET last_check_error_code='other_error' WHERE id=?", nil},
		{nonTargets[4], "UPDATE accounts SET polling_paused=0 WHERE id=?", nil},
		{nonTargets[5], "UPDATE accounts SET pending_evidence_signature=? WHERE id=?", []any{otherSignature}},
	}
	for _, mutation := range mutations {
		args := append(mutation.args, mutation.id)
		if _, err := db.Exec(mutation.query, args...); err != nil {
			t.Fatal(err)
		}
	}
	fingerprint := func(id int64) string {
		t.Helper()
		var value string
		if err := db.QueryRow(`SELECT status||'|'||token_type||'|'||last_check_state||'|'||
			COALESCE(last_check_error_code,'')||'|'||polling_paused||'|'||COALESCE(pause_reason,'')||'|'||
			COALESCE(pending_evidence_signature,'')||'|'||COALESCE(pending_detected_at,'')||'|'||COALESCE(next_retry_at,'')
			FROM accounts WHERE id=?`, id).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	beforeNonTargets := make(map[int64]string, len(nonTargets))
	for _, id := range nonTargets {
		beforeNonTargets[id] = fingerprint(id)
	}
	var immutableBefore string
	if err := db.QueryRow(`SELECT plan||'|'||raw_plan||'|'||COALESCE(current_expiry,'')||'|'||auth_expiry||'|'||hex(enc_credentials)||'|'||credential_key_id
		FROM accounts WHERE id=?`, targets[0]).Scan(&immutableBefore); err != nil {
		t.Fatal(err)
	}

	recovered, err := s.RecoverMisclassifiedSupplementalDenials(context.Background())
	if err != nil || recovered != len(targets) {
		t.Fatalf("recovered=%d err=%v", recovered, err)
	}
	for _, id := range targets {
		assertAccountState(t, db, id, StateAlive, CheckOK, false)
		var errorCode, retryAt, pauseReason, pendingSignature, pendingDetected sql.NullString
		if err := db.QueryRow(`SELECT last_check_error_code,next_retry_at,pause_reason,pending_evidence_signature,pending_detected_at
			FROM accounts WHERE id=?`, id).Scan(&errorCode, &retryAt, &pauseReason, &pendingSignature, &pendingDetected); err != nil {
			t.Fatal(err)
		}
		if errorCode.Valid || retryAt.Valid || pauseReason.Valid || pendingSignature.Valid || pendingDetected.Valid {
			t.Fatalf("recovery fields were not cleared for account %d", id)
		}
	}
	for _, id := range nonTargets {
		if after := fingerprint(id); after != beforeNonTargets[id] {
			t.Fatalf("non-target account %d changed: before=%q after=%q", id, beforeNonTargets[id], after)
		}
	}
	var immutableAfter string
	if err := db.QueryRow(`SELECT plan||'|'||raw_plan||'|'||COALESCE(current_expiry,'')||'|'||auth_expiry||'|'||hex(enc_credentials)||'|'||credential_key_id
		FROM accounts WHERE id=?`, targets[0]).Scan(&immutableAfter); err != nil {
		t.Fatal(err)
	}
	if immutableAfter != immutableBefore {
		t.Fatalf("business or credential fields changed: before=%q after=%q", immutableBefore, immutableAfter)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE evidence_code='supplemental_denial_reclassified'", len(targets))
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE evidence_code='unexpected_non_json' AND review_decision='rejected' AND account_id IN ("+strconv.FormatInt(targets[0], 10)+","+strconv.FormatInt(targets[1], 10)+")", len(targets))
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE evidence_code='supplemental_denial_reclassified' AND evidence_level='contract_verified_live_pending' AND review_decision IS NULL", len(targets))
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE evidence_code='unexpected_non_json' AND review_decision='rejected' AND reviewed_at IS NOT NULL AND review_operator='startup_recovery'", len(targets))

	recovered, err = s.RecoverMisclassifiedSupplementalDenials(context.Background())
	if err != nil || recovered != 0 {
		t.Fatalf("second recovery=%d err=%v", recovered, err)
	}
	assertCount(t, db, "SELECT count(*) FROM status_change_log WHERE evidence_code='supplemental_denial_reclassified'", len(targets))
}

func TestNormalExpiryWinsWithoutUpstreamAndKeepsAccountVisible(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccount(t, db, keyring, "acct-expired", now, now.Add(-30*24*time.Hour))
	if _, done, err := s.RefreshNow(context.Background(), id); err != nil || !done {
		t.Fatalf("done=%v err=%v", done, err)
	}
	assertAccountState(t, db, id, StateDeadNormal, CheckOK, true)
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

func TestGenericAccountCheckDenialAfterRefreshPersistsCredentialsAndRecordsError(t *testing.T) {
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
			if run.AccountsOK != 0 || run.AccountsFailed != 1 || run.ErrorCounts[test.code] != 1 {
				t.Fatalf("run=%+v", run)
			}
			assertAccountState(t, db, id, StateAlive, CheckError, false)
			var plan, rawPlan string
			var checkError sql.NullString
			var envelope []byte
			if err := db.QueryRow("SELECT plan,raw_plan,last_check_error_code,enc_credentials FROM accounts WHERE id=?", id).Scan(&plan, &rawPlan, &checkError, &envelope); err != nil {
				t.Fatal(err)
			}
			if plan != "plus" || rawPlan != "chatgptplusplan" || !checkError.Valid || checkError.String != test.code {
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

func TestLegacyRefreshCredentialRecordsGenericDenial(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors["acct-refresh"] = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: 401, EvidenceCode: "http_401", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	id := seedAccountPayload(t, db, keyring, "acct-refresh", "refresh", credentialPayload{
		Access: jwtFor("acct-refresh", now.Add(time.Hour)), Refresh: "legacy-refresh-fixture",
	}, now.Add(24*time.Hour), now)
	if _, err := db.Exec("UPDATE accounts SET last_check_state='error',last_check_error_code='http_401' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	run, _, err := s.RefreshNow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.AccountsOK != 0 || run.AccountsFailed != 1 || run.ErrorCounts["http_401"] != 1 || client.exchanges.Load() != 0 {
		t.Fatalf("run=%+v exchanges=%d", run, client.exchanges.Load())
	}
	assertAccountState(t, db, id, StateAlive, CheckError, false)
	var envelope []byte
	if err := db.QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", id).Scan(&envelope); err != nil {
		t.Fatal(err)
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
	if credentials.OAuthSource != "refresh" {
		t.Fatalf("legacy credential source=%q", credentials.OAuthSource)
	}
}

func TestDeviceCredentialRotationSurvivesGenericDenialAndRecordsError(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	accessExpiry := now.Add(time.Hour)
	client.exchangeExpiry = &accessExpiry
	client.exchangeIDToken = "device-rotated-id-token"
	client.errors["acct-refresh"] = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: 403, EvidenceCode: "http_403", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	id := seedAccountPayload(t, db, keyring, "acct-refresh", "device", credentialPayload{
		Access: jwtFor("acct-refresh", now.Add(-time.Hour)), Refresh: "device-refresh-fixture", OAuthSource: "device",
	}, now.Add(24*time.Hour), now)
	run, _, err := s.RefreshNow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.AccountsOK != 0 || run.AccountsFailed != 1 || run.ErrorCounts["http_403"] != 1 {
		t.Fatalf("run=%+v", run)
	}
	assertAccountState(t, db, id, StateAlive, CheckError, false)
	var envelope []byte
	if err := db.QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", id).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(id, "device"))
	if err != nil {
		t.Fatal(err)
	}
	var credentials credentialPayload
	if err := json.Unmarshal(plaintext, &credentials); err != nil {
		t.Fatal(err)
	}
	zero(plaintext)
	if credentials.Refresh != "device-refresh-fixture-rotated" || credentials.IDToken != "device-rotated-id-token" ||
		credentials.OAuthSource != "device" || credentials.AccessExpiresAt == nil || !credentials.AccessExpiresAt.Equal(accessExpiry) {
		t.Fatalf("persisted device credentials=%+v", credentials)
	}
}

func TestUnknownCredentialSourceCannotBypassStatusValidation(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors["acct-untrusted-source"] = &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: 401, EvidenceCode: "http_401", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	id := seedAccountPayload(t, db, keyring, "acct-untrusted-source", "access", credentialPayload{
		Access: jwtFor("acct-untrusted-source", now.Add(time.Hour)), OAuthSource: "untrusted",
	}, now.Add(24*time.Hour), now)
	run, _, err := s.RefreshNow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if run.AccountsOK != 0 || run.AccountsFailed != 1 || run.ErrorCounts["http_401"] != 1 {
		t.Fatalf("run=%+v", run)
	}
	assertAccountState(t, db, id, StateAlive, CheckError, false)
}

func TestOnlyExplicitAccountDisabledAfterRefreshUsesBanEvidence(t *testing.T) {
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
			if code == "account_disabled" {
				assertAccountState(t, db, id, StateDeadBanned, CheckOK, true)
			} else {
				assertAccountState(t, db, id, StateAlive, CheckError, false)
			}
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

// 已封禁和订阅正常到期的账号必须彻底退出轮询队列。
func TestTerminalAccountsAreNotPolledAgain(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	expired := seedAccount(t, db, keyring, "acct-expired", now, now.Add(-30*24*time.Hour))
	banned := seedAccount(t, db, keyring, "acct-banned", now.Add(10*24*time.Hour), now.Add(-2*24*time.Hour))
	client.errors["acct-banned"] = candidate("account_disabled")

	if _, done, err := s.RefreshNow(context.Background(), expired); err != nil || !done {
		t.Fatalf("expired refresh done=%v err=%v", done, err)
	}
	if _, done, err := s.RefreshNow(context.Background(), banned); err != nil || !done {
		t.Fatalf("banned refresh done=%v err=%v", done, err)
	}
	assertAccountState(t, db, expired, StateDeadNormal, CheckOK, true)
	assertAccountState(t, db, banned, StateDeadBanned, CheckOK, true)

	// 两道防线：授权 epoch 已关闭，且 polling_paused 已置位。
	for _, id := range []int64{expired, banned} {
		var openEpochs int
		if err := db.QueryRow("SELECT count(*) FROM authorization_epochs WHERE account_id=? AND ended_at IS NULL", id).Scan(&openEpochs); err != nil {
			t.Fatal(err)
		}
		if openEpochs != 0 {
			t.Fatalf("account %d still has an open authorization epoch", id)
		}
	}

	run, err := s.RunScheduled(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "" || run.AccountsTotal != 0 {
		t.Fatalf("terminal accounts must not be scheduled again: %+v", run)
	}
	before := pollRunCount(t, db)
	if _, err := s.RunScheduled(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := pollRunCount(t, db); after != before {
		t.Fatalf("scheduled run count moved from %d to %d with only terminal accounts", before, after)
	}
}

func pollRunCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM poll_runs WHERE trigger_type='scheduled'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func denied(code string, status int) *chatgpt.TypedError {
	return &chatgpt.TypedError{Kind: chatgpt.ErrorPermissionDenied, StatusCode: status, EvidenceCode: code,
		EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
}

func denialState(t *testing.T, db *sql.DB, id int64) (int, bool) {
	t.Helper()
	var streak int
	var suspectedAt sql.NullString
	if err := db.QueryRow("SELECT denial_streak,suspected_banned_at FROM accounts WHERE id=?", id).Scan(&streak, &suspectedAt); err != nil {
		t.Fatal(err)
	}
	return streak, suspectedAt.Valid
}

// 连续三次账号级拒绝后标记疑似封禁；账号状态保持 alive，不做任何终态处理。
func TestConsecutiveAccountDenialsMarkSuspectedWithoutBanning(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors["acct-denied"] = denied("http_401", 401)
	id := seedAccount(t, db, keyring, "acct-denied", now.Add(24*time.Hour), now)

	for attempt := 1; attempt <= 3; attempt++ {
		if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
			t.Fatalf("attempt %d: %v", attempt, err)
		}
		streak, suspected := denialState(t, db, id)
		if streak != attempt {
			t.Fatalf("attempt %d streak=%d", attempt, streak)
		}
		if suspected != (attempt >= 3) {
			t.Fatalf("attempt %d suspected=%v", attempt, suspected)
		}
	}
	// 疑似阶段绝不动账号状态：既不封禁也不停轮询。
	assertAccountState(t, db, id, StateAlive, CheckError, false)
	assertCount(t, db, "SELECT count(*) FROM alert_events", 0)

	// 疑似标记要推给分配域，且事件载荷里 suspected 为真。
	var payload string
	if err := db.QueryRow("SELECT payload_json FROM allocation_account_outbox WHERE account_id=? ORDER BY account_version DESC LIMIT 1", id).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, `"suspected":true`) {
		t.Fatalf("outbox payload missing suspected flag: %s", payload)
	}
}

// 一次成功轮询即清零计数并解除疑似标记。
func TestSuccessfulPollClearsDenialStreakAndSuspicion(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors["acct-recovers"] = denied("http_403", 403)
	id := seedAccount(t, db, keyring, "acct-recovers", now.Add(24*time.Hour), now)
	for i := 0; i < 3; i++ {
		if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if streak, suspected := denialState(t, db, id); streak != 3 || !suspected {
		t.Fatalf("streak=%d suspected=%v", streak, suspected)
	}
	delete(client.errors, "acct-recovers")
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if streak, suspected := denialState(t, db, id); streak != 0 || suspected {
		t.Fatalf("after recovery streak=%d suspected=%v", streak, suspected)
	}
	assertAccountState(t, db, id, StateAlive, CheckOK, false)
}

// 需要重新授权（凭据过期/失效）既不计入疑似计数，也不清零已有证据。
func TestReauthorizationRequiredNeitherCountsNorClearsDenialStreak(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	client.errors["acct-reauth"] = denied("http_401", 401)
	// 用纯访问令牌账号：401 来自 accounts/check，与线上被封账号的形态一致
	// （带 refresh 凭据时 401 会先在换取令牌阶段转成 reauthorization_required 并暂停轮询）。
	id := seedAccount(t, db, keyring, "acct-reauth", now.Add(24*time.Hour), now)
	for i := 0; i < 2; i++ {
		if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}
	if streak, suspected := denialState(t, db, id); streak != 2 || suspected {
		t.Fatalf("streak=%d suspected=%v", streak, suspected)
	}

	client.errors["acct-reauth"] = &chatgpt.TypedError{Kind: chatgpt.ErrorAuthorizationRequired, StatusCode: 400,
		EvidenceCode: "oauth_refresh_token_invalid", EvidenceLevel: chatgpt.EvidenceContractVerifiedLivePending, PreserveBusinessState: true}
	if _, _, err := s.RefreshNow(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	// 计数停在 2：不因重新授权而 +1，也不被它清零。
	if streak, suspected := denialState(t, db, id); streak != 2 || suspected {
		t.Fatalf("after reauthorization streak=%d suspected=%v", streak, suspected)
	}
	var status string
	if err := db.QueryRow("SELECT status FROM accounts WHERE id=?", id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != StateAlive {
		t.Fatalf("reauthorization must never imply a ban, status=%s", status)
	}
}

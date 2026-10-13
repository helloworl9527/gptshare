package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	credentialcrypto "chatgpt-monitor/internal/crypto"
)

// storedCredential decrypts the persisted credential payload for assertions.
func storedCredential(t *testing.T, db *sql.DB, keyring *credentialcrypto.Keyring, id int64, tokenType string) credentialPayload {
	t.Helper()
	var envelope []byte
	var keyID string
	if err := db.QueryRow("SELECT enc_credentials,credential_key_id FROM accounts WHERE id=?", id).Scan(&envelope, &keyID); err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(id, tokenType))
	if err != nil {
		t.Fatal(err)
	}
	defer zero(plaintext)
	var payload credentialPayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestRefreshSchedulerRotatesCredentialApproachingLead(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	// Access token expires 2h from now, which is inside the 24h RefreshLead,
	// so the sweep must rotate the refresh token.
	id := seedAccountPayload(t, db, keyring, "acct-lead", "refresh", credentialPayload{
		Access: jwtFor("acct-lead", now.Add(2*time.Hour)), Refresh: "refresh-a", OAuthSource: "refresh",
	}, now.Add(30*24*time.Hour), now)

	client.exchanges.Store(0)
	s.runRefreshSweep(context.Background())

	if client.exchanges.Load() == 0 {
		t.Fatal("expected the sweep to exchange the refresh token")
	}
	payload := storedCredential(t, db, keyring, id, "refresh")
	if payload.Refresh == "refresh-a" {
		t.Fatalf("refresh token was not rotated, still %q", payload.Refresh)
	}
	if payload.Access == "" || payload.AccessExpiresAt == nil {
		t.Fatalf("rotated access metadata missing: %+v", payload)
	}
}

func TestRefreshSchedulerLeavesCredentialBeyondLead(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	// Access token expires 10 days from now, beyond the 24h lead, so no
	// rotation should happen.
	id := seedAccountPayload(t, db, keyring, "acct-fresh", "refresh", credentialPayload{
		Access: jwtFor("acct-fresh", now.Add(10*24*time.Hour)), Refresh: "refresh-fresh", OAuthSource: "refresh",
	}, now.Add(30*24*time.Hour), now)

	client.exchanges.Store(0)
	s.runRefreshSweep(context.Background())

	if client.exchanges.Load() != 0 {
		t.Fatalf("expected no exchange beyond the lead window, got %d", client.exchanges.Load())
	}
	payload := storedCredential(t, db, keyring, id, "refresh")
	if payload.Refresh != "refresh-fresh" {
		t.Fatalf("refresh token changed unexpectedly: %q", payload.Refresh)
	}
}

func TestRefreshSchedulerSkipsActivelyPolledAccount(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccountPayload(t, db, keyring, "acct-busy", "refresh", credentialPayload{
		Access: jwtFor("acct-busy", now.Add(time.Hour)), Refresh: "refresh-busy", OAuthSource: "refresh",
	}, now.Add(30*24*time.Hour), now)

	// Simulate a concurrent liveness poll claiming the account.
	s.mu.Lock()
	s.active[id] = "run-in-flight"
	s.mu.Unlock()

	client.exchanges.Store(0)
	s.runRefreshSweep(context.Background())

	if client.exchanges.Load() != 0 {
		t.Fatalf("expected in-flight account to be skipped, got %d exchanges", client.exchanges.Load())
	}
	payload := storedCredential(t, db, keyring, id, "refresh")
	if payload.Refresh != "refresh-busy" {
		t.Fatalf("refresh token rotated while account was busy: %q", payload.Refresh)
	}
}

func TestRefreshSchedulerLeavesBusinessStateOnAuthorizationFailure(t *testing.T) {
	s, db, keyring, client, now := newTestService(t)
	id := seedAccountPayload(t, db, keyring, "acct-revoked", "refresh", credentialPayload{
		Access: jwtFor("acct-revoked", now.Add(time.Hour)), Refresh: "refresh-revoked", OAuthSource: "refresh",
	}, now.Add(30*24*time.Hour), now)

	client.exchangeErr = authorizationRequired("oauth_refresh_token_reused")

	s.runRefreshSweep(context.Background())

	// The sweep must not mark the account dead/paused; that classification is
	// owned by the liveness poller via applyResult.
	assertAccountState(t, db, id, StateAlive, CheckOK, false)
	payload := storedCredential(t, db, keyring, id, "refresh")
	if payload.Refresh != "refresh-revoked" {
		t.Fatalf("refresh token changed on failed rotation: %q", payload.Refresh)
	}
}

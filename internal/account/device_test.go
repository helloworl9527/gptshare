package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

func TestDeviceStartEncryptsSessionAndRejectsHighFrequencyPolls(t *testing.T) {
	service, database, keyring, closeDB := testService(t, "acct-device")
	defer closeDB()
	now := time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{
		DeviceAuthID: "synthetic-device-code-plaintext", UserCode: "ABCD-EFGH", VerifyURL: "https://auth.example/device",
		Interval: 5 * time.Second, ExpiresAt: now.Add(15 * time.Minute),
	}
	client.devicePolls = []chatgpt.DevicePollResult{
		{State: chatgpt.DevicePollPending, RetryAfter: 5 * time.Second},
		{State: chatgpt.DevicePollSlowDown, RetryAfter: 10 * time.Second},
		{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "device-access-plaintext", RefreshToken: "device-refresh-plaintext", IDToken: "device-id-plaintext"}},
	}
	started, err := service.StartDeviceImport(context.Background(), "Device account")
	if err != nil {
		t.Fatal(err)
	}
	if len(started.SessionID) < 32 || started.UserCode != "ABCD-EFGH" || started.State != "pending" || started.IntervalSeconds != 5 {
		t.Fatalf("start=%+v", started)
	}
	var envelope []byte
	var keyID string
	if err := database.DB().QueryRow("SELECT enc_device_code,credential_key_id FROM device_auth_sessions WHERE id=?", started.SessionID).Scan(&envelope, &keyID); err != nil {
		t.Fatal(err)
	}
	if keyID != "active" || bytes.Contains(envelope, []byte("synthetic-device-code-plaintext")) || bytes.Contains(envelope, []byte("ABCD-EFGH")) {
		t.Fatal("device session was not safely encrypted")
	}
	plaintext, err := keyring.Open(envelope, []byte("wrong-aad"))
	if err == nil || plaintext != nil {
		t.Fatal("device envelope accepted wrong AAD")
	}

	poll, err := service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "pending" || client.pollCalls != 0 {
		t.Fatalf("early poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	now = now.Add(5 * time.Second)
	poll, err = service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "pending" || client.pollCalls != 1 {
		t.Fatalf("pending poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	poll, err = service.PollDevice(context.Background(), started.SessionID)
	if err != nil || client.pollCalls != 1 {
		t.Fatalf("high-frequency poll reached upstream: calls=%d err=%v", client.pollCalls, err)
	}
	now = now.Add(5 * time.Second)
	poll, err = service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "slow_down" || poll.RetryAfter != 10 || client.pollCalls != 2 {
		t.Fatalf("slow poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}

	restarted, err := NewService(database.DB(), client, keyring)
	if err != nil {
		t.Fatal(err)
	}
	restarted.now = func() time.Time { return now }
	poll, err = restarted.PollDevice(context.Background(), started.SessionID)
	if err != nil || client.pollCalls != 2 {
		t.Fatalf("restart high-frequency poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	now = now.Add(10 * time.Second)
	poll, err = restarted.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "authorized" || poll.Account == nil || poll.Account.Credential.Type != "device" || client.pollCalls != 3 {
		t.Fatalf("authorized poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	var syncEvents int
	if err := database.DB().QueryRow("SELECT count(*) FROM allocation_account_outbox WHERE account_id=?", poll.Account.ID).Scan(&syncEvents); err != nil || syncEvents != 1 {
		t.Fatalf("device allocation sync events=%d err=%v", syncEvents, err)
	}
	accountID := poll.Account.ID
	var credentialEnvelope []byte
	if err := database.DB().QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", accountID).Scan(&credentialEnvelope); err != nil {
		t.Fatal(err)
	}
	credentialPlaintext, err := keyring.Open(credentialEnvelope, credentialcrypto.CredentialAAD(accountID, "device"))
	if err != nil {
		t.Fatal(err)
	}
	var credential credentialPayload
	if err := json.Unmarshal(credentialPlaintext, &credential); err != nil {
		t.Fatal(err)
	}
	zero(credentialPlaintext)
	if credential.IDToken != "device-id-plaintext" || credential.OAuthSource != "device" {
		t.Fatalf("device credential metadata=%+v", credential)
	}
	poll, err = restarted.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.Account == nil || poll.Account.ID != accountID || client.pollCalls != 3 {
		t.Fatalf("replay=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	var accounts, epochs, sessionPlaintext int
	database.DB().QueryRow("SELECT count(*) FROM accounts WHERE deleted_at IS NULL").Scan(&accounts)
	database.DB().QueryRow("SELECT count(*) FROM authorization_epochs WHERE account_id=?", accountID).Scan(&epochs)
	database.DB().QueryRow("SELECT length(enc_device_code) FROM device_auth_sessions WHERE id=?", started.SessionID).Scan(&sessionPlaintext)
	if accounts != 1 || epochs != 1 || sessionPlaintext != 0 {
		t.Fatalf("accounts=%d epochs=%d device_envelope_length=%d", accounts, epochs, sessionPlaintext)
	}
	if _, err := database.DB().Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{databasePath(t, database), databasePath(t, database) + "-wal", databasePath(t, database) + "-shm"} {
		contents, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, secret := range [][]byte{[]byte("synthetic-device-code-plaintext"), []byte("ABCD-EFGH"), []byte("device-access-plaintext"), []byte("device-refresh-plaintext")} {
			if bytes.Contains(contents, secret) {
				t.Fatal("device plaintext found in SQLite files")
			}
		}
	}
}

func TestDeviceExpiryClearsSecretAndIsRecoverable(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-expired")
	defer closeDB()
	now := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "expires-device", UserCode: "EXPI-RED1", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(2 * time.Second)}
	started, err := service.StartDeviceImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	poll, err := service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "expired" || !poll.RestartRequired || client.pollCalls != 0 {
		t.Fatalf("expired=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
	poll, err = service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "expired" || !poll.RestartRequired {
		t.Fatalf("expired replay=%+v err=%v", poll, err)
	}
	var length int
	database.DB().QueryRow("SELECT length(enc_device_code) FROM device_auth_sessions WHERE id=?", started.SessionID).Scan(&length)
	if length != 0 {
		t.Fatalf("expired secret length=%d", length)
	}
}

func TestDeviceDuplicateAndFailedReauthorizationAreAtomic(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-existing")
	defer closeDB()
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	existing, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "initial"})
	if err != nil {
		t.Fatal(err)
	}
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "duplicate-device", UserCode: "DUPL-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(time.Minute)}
	client.devicePolls = []chatgpt.DevicePollResult{{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "duplicate-access"}}}
	started, err := service.StartDeviceImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	_, err = service.PollDevice(context.Background(), started.SessionID)
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorDuplicate {
		t.Fatalf("duplicate err=%v", err)
	}
	var accounts int
	database.DB().QueryRow("SELECT count(*) FROM accounts WHERE deleted_at IS NULL").Scan(&accounts)
	if accounts != 1 {
		t.Fatalf("duplicate created accounts=%d", accounts)
	}

	client.status.ProviderAccountID = "different-provider"
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "reauth-device", UserCode: "REAU-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(time.Minute)}
	client.devicePolls = []chatgpt.DevicePollResult{{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "reauth-access"}}}
	reauth, err := service.StartDeviceReauthorization(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	_, err = service.PollDevice(context.Background(), reauth.SessionID)
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorInvalid {
		t.Fatalf("reauthorization err=%v", err)
	}
	var epochs, activeEpochs int
	database.DB().QueryRow("SELECT count(*),sum(ended_at IS NULL) FROM authorization_epochs WHERE account_id=?", existing.ID).Scan(&epochs, &activeEpochs)
	if epochs != 1 || activeEpochs != 1 {
		t.Fatalf("failed reauthorization epochs=%d active=%d", epochs, activeEpochs)
	}
}

func TestDeviceReauthorizationSuccessPreservesOldEpoch(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-device-reauth")
	defer closeDB()
	now := time.Date(2026, 7, 22, 13, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	existing, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "initial-reauth"})
	if err != nil {
		t.Fatal(err)
	}
	client := service.client.(*fakeClient)
	newExpiry := client.status.SubscriptionExpiry.Add(24 * time.Hour)
	client.status.SubscriptionExpiry = &newExpiry
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "success-device", UserCode: "SUCC-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(time.Minute)}
	client.devicePolls = []chatgpt.DevicePollResult{{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "success-access", RefreshToken: "success-refresh"}}}
	started, err := service.StartDeviceReauthorization(context.Background(), existing.ID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	poll, err := service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.Account == nil || poll.Account.ID != existing.ID || poll.Account.Credential.Type != "device" || !poll.Account.AuthExpiry.Equal(newExpiry) {
		t.Fatalf("poll=%+v err=%v", poll, err)
	}
	var epochs, ended int
	database.DB().QueryRow("SELECT count(*),sum(ended_at IS NOT NULL) FROM authorization_epochs WHERE account_id=?", existing.ID).Scan(&epochs, &ended)
	if epochs != 2 || ended != 1 {
		t.Fatalf("epochs=%d ended=%d", epochs, ended)
	}
}

func TestDeviceMaximumPollCountExpiresWithoutUpstreamCall(t *testing.T) {
	service, database, keyring, closeDB := testService(t, "acct-max-polls")
	defer closeDB()
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "max-device", UserCode: "MAXP-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(time.Hour)}
	started, err := service.StartDeviceImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	var envelope []byte
	if err := database.DB().QueryRow("SELECT enc_device_code FROM device_auth_sessions WHERE id=?", started.SessionID).Scan(&envelope); err != nil {
		t.Fatal(err)
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.DeviceSessionAAD(started.SessionID))
	if err != nil {
		t.Fatal(err)
	}
	var payload devicePayload
	if err := json.Unmarshal(plaintext, &payload); err != nil {
		t.Fatal(err)
	}
	zero(plaintext)
	payload.PollCount = maxDevicePolls
	plaintext, _ = json.Marshal(payload)
	envelope, err = keyring.Seal(plaintext, credentialcrypto.DeviceSessionAAD(started.SessionID))
	zero(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.DB().Exec("UPDATE device_auth_sessions SET enc_device_code=? WHERE id=?", envelope, started.SessionID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	poll, err := service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "expired" || !poll.RestartRequired || client.pollCalls != 0 {
		t.Fatalf("poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
}

func TestDeviceAuthorizedTokensSurviveTransientStatusFailure(t *testing.T) {
	service, _, _, closeDB := testService(t, "acct-status-retry")
	defer closeDB()
	now := time.Date(2026, 7, 22, 15, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return now }
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "retry-device", UserCode: "RETR-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: now.Add(time.Minute)}
	client.devicePolls = []chatgpt.DevicePollResult{{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "cached-access", RefreshToken: "cached-refresh"}}}
	client.statusErr = &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_timeout", Retryable: true}
	started, err := service.StartDeviceImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	_, err = service.PollDevice(context.Background(), started.SessionID)
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorUnavailable || client.pollCalls != 1 {
		t.Fatalf("first err=%v poll_calls=%d", err, client.pollCalls)
	}
	client.statusErr = nil
	now = now.Add(time.Second)
	poll, err := service.PollDevice(context.Background(), started.SessionID)
	if err != nil || poll.State != "authorized" || poll.Account == nil || client.pollCalls != 1 {
		t.Fatalf("retry poll=%+v calls=%d err=%v", poll, client.pollCalls, err)
	}
}

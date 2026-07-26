package account

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/store"
)

type fakeClient struct {
	tokens      chatgpt.TokenSet
	status      chatgpt.StatusResult
	exchangeErr error
	statusErr   error
	lastKind    chatgpt.CredentialKind
	lastSecret  string
	deviceAuth  chatgpt.DeviceAuthorization
	devicePolls []chatgpt.DevicePollResult
	deviceErr   error
	startCalls  int
	pollCalls   int
}

func (f *fakeClient) StartDeviceAuthorization(context.Context) (chatgpt.DeviceAuthorization, error) {
	f.startCalls++
	return f.deviceAuth, f.deviceErr
}

func (f *fakeClient) PollDeviceAuthorizationResult(context.Context, chatgpt.DeviceAuthorization) (chatgpt.DevicePollResult, error) {
	f.pollCalls++
	if f.deviceErr != nil {
		return chatgpt.DevicePollResult{}, f.deviceErr
	}
	if len(f.devicePolls) == 0 {
		return chatgpt.DevicePollResult{State: chatgpt.DevicePollPending, RetryAfter: 5 * time.Second}, nil
	}
	result := f.devicePolls[0]
	f.devicePolls = f.devicePolls[1:]
	return result, nil
}

func (f *fakeClient) ExchangeCredential(_ context.Context, kind chatgpt.CredentialKind, secret string) (chatgpt.TokenSet, error) {
	f.lastKind, f.lastSecret = kind, secret
	return f.tokens, f.exchangeErr
}

func (f *fakeClient) FetchStatus(context.Context, string) (chatgpt.StatusResult, error) {
	return f.status, f.statusErr
}

func TestImportEncryptsSnapshotsAndRejectsDuplicate(t *testing.T) {
	service, database, keyring, closeDB := testService(t, "acct-import")
	defer closeDB()
	secret := "synthetic-access-token-never-store-plaintext"
	refresh := "synthetic-refresh-token-never-store-plaintext"
	service.client.(*fakeClient).tokens.AccessToken = secret
	input := &TokenInput{Label: "Primary", AccessToken: secret, RefreshToken: refresh}
	account, err := service.ImportByToken(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if input.AccessToken != "" || input.RefreshToken != "" {
		t.Fatal("caller credential references were not released")
	}
	if account.Plan != "plus" || account.Status != "alive" || !account.Credential.Configured || account.Credential.Type != "access" {
		t.Fatalf("account=%+v", account)
	}
	var envelope []byte
	var keyID, authExpiry string
	if err := database.DB().QueryRow("SELECT enc_credentials,credential_key_id,auth_expiry FROM accounts WHERE id=?", account.ID).Scan(&envelope, &keyID, &authExpiry); err != nil {
		t.Fatal(err)
	}
	if keyID != "active" || bytes.Contains(envelope, []byte(secret)) || authExpiry != account.AuthExpiry.Format(time.RFC3339Nano) {
		t.Fatal("credential envelope or auth snapshot mismatch")
	}
	plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(account.ID, "access"))
	if err != nil || !bytes.Contains(plaintext, []byte(secret)) || !bytes.Contains(plaintext, []byte(refresh)) {
		t.Fatalf("decrypt failed err=%v", err)
	}
	zero(plaintext)

	duplicate := &TokenInput{AccessToken: "different-token"}
	_, err = service.ImportByToken(context.Background(), duplicate)
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorDuplicate {
		t.Fatalf("duplicate error=%v", err)
	}
	var count int
	database.DB().QueryRow("SELECT count(*) FROM accounts WHERE deleted_at IS NULL").Scan(&count)
	if count != 1 {
		t.Fatalf("active accounts=%d", count)
	}
}

func TestImportAcceptsEachDirectCredentialPath(t *testing.T) {
	tests := []struct {
		name  string
		kind  chatgpt.CredentialKind
		input TokenInput
	}{
		{name: "access", kind: chatgpt.CredentialAccess, input: TokenInput{AccessToken: "access-only-secret"}},
		{name: "refresh", kind: chatgpt.CredentialRefresh, input: TokenInput{RefreshToken: "refresh-only-secret"}},
		{name: "session", kind: chatgpt.CredentialSession, input: TokenInput{SessionToken: "session-only-secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service, database, keyring, closeDB := testService(t, "acct-"+test.name)
			defer closeDB()
			account, err := service.ImportByToken(context.Background(), &test.input)
			if err != nil {
				t.Fatal(err)
			}
			client := service.client.(*fakeClient)
			if client.lastKind != test.kind || client.lastSecret == "" || account.Credential.Type != string(test.kind) {
				t.Fatalf("kind=%q exchanged=%q account=%+v", test.kind, client.lastKind, account)
			}
			var envelope []byte
			if err := database.DB().QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", account.ID).Scan(&envelope); err != nil {
				t.Fatal(err)
			}
			plaintext, err := keyring.Open(envelope, credentialcrypto.CredentialAAD(account.ID, string(test.kind)))
			if err != nil || len(plaintext) == 0 {
				t.Fatalf("decrypt err=%v", err)
			}
			zero(plaintext)
		})
	}
}

func TestImportEmailDefaultLabelAndReauthorizeFillOnly(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-email")
	defer closeDB()
	client := service.client.(*fakeClient)
	client.status.Email = "owner@example.test"
	account, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "access"})
	if err != nil {
		t.Fatal(err)
	}
	if account.Email == nil || *account.Email != "owner@example.test" || account.Label != "owner@example.test" {
		t.Fatalf("email default account=%+v", account)
	}

	client.status.Email = "other@example.test"
	reauthorized, err := service.ReauthorizeByToken(context.Background(), account.ID, &TokenInput{AccessToken: "access-2"})
	if err != nil {
		t.Fatal(err)
	}
	if reauthorized.Email == nil || *reauthorized.Email != "owner@example.test" || reauthorized.Label != "owner@example.test" {
		t.Fatalf("conflicting reauth overwrote email/label: %+v", reauthorized)
	}

	client.status.ProviderAccountID = "acct-custom"
	client.status.Email = "custom@example.test"
	custom, err := service.ImportByToken(context.Background(), &TokenInput{Label: "Custom Label", AccessToken: "custom-access"})
	if err != nil {
		t.Fatal(err)
	}
	if custom.Label != "Custom Label" || custom.Email == nil || *custom.Email != "custom@example.test" {
		t.Fatalf("custom account=%+v", custom)
	}
	client.status.Email = "new-custom@example.test"
	updated, err := service.ReauthorizeByToken(context.Background(), custom.ID, &TokenInput{AccessToken: "custom-reauth"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != "Custom Label" || *updated.Email != "custom@example.test" {
		t.Fatalf("custom label/email overwritten: %+v", updated)
	}

	var logs int
	database.DB().QueryRow("SELECT count(*) FROM status_change_log WHERE from_value LIKE '%@example.test%' OR to_value LIKE '%@example.test%'").Scan(&logs)
	if logs != 0 {
		t.Fatalf("email leaked to status_change_log rows=%d", logs)
	}
}

func TestInvalidAndTransientImportAreAtomicAndSanitized(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-errors")
	defer closeDB()
	secret := "credential-value-must-not-appear-in-error"
	service.client = &fakeClient{exchangeErr: &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_timeout", Retryable: true}}
	_, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: secret})
	var serviceErr *ServiceError
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorUnavailable || strings.Contains(err.Error(), secret) {
		t.Fatalf("transient error=%v", err)
	}
	service.client = &fakeClient{tokens: chatgpt.TokenSet{AccessToken: "access"}, status: chatgpt.StatusResult{ProviderAccountID: "acct", Plan: chatgpt.PlanUnknown, AccountState: chatgpt.StateActive}}
	_, err = service.ImportByToken(context.Background(), &TokenInput{AccessToken: secret})
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorInvalid {
		t.Fatalf("invalid error=%v", err)
	}
	expiry := time.Date(2026, 8, 19, 0, 0, 0, 0, time.UTC)
	service.client = &fakeClient{tokens: chatgpt.TokenSet{AccessToken: "access"}, status: chatgpt.StatusResult{
		ProviderAccountID: "acct", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry,
		AccountState: chatgpt.StateActive, EvidenceLevel: chatgpt.EvidenceUnverified,
	}}
	_, err = service.ImportByToken(context.Background(), &TokenInput{AccessToken: secret})
	if !errors.As(err, &serviceErr) || serviceErr.Kind != ErrorInvalid {
		t.Fatalf("unverified evidence error=%v", err)
	}
	var count int
	database.DB().QueryRow("SELECT count(*) FROM accounts").Scan(&count)
	if count != 0 {
		t.Fatalf("failed import wrote %d accounts", count)
	}
}

func TestAuthExpiryStableReauthorizePreservesEpochAndFailureRollsBack(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-reauth")
	defer closeDB()
	account, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "first-access"})
	if err != nil {
		t.Fatal(err)
	}
	originalExpiry := account.AuthExpiry
	if _, err := database.DB().Exec("UPDATE accounts SET current_expiry='2027-01-01T00:00:00Z' WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	account, err = service.Get(context.Background(), account.ID)
	if err != nil || !account.AuthExpiry.Equal(originalExpiry) {
		t.Fatalf("auth_expiry changed during current expiry update: %+v err=%v", account, err)
	}
	if _, err := database.DB().Exec(`INSERT INTO status_change_log(account_id,at,field,evidence_code,evidence_level,evidence_signature)
		VALUES (?,?,'plan','test','live_verified','test-signature')`, account.ID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	newExpiry := originalExpiry.Add(30 * 24 * time.Hour)
	service.client.(*fakeClient).status.SubscriptionExpiry = &newExpiry
	service.now = func() time.Time { return time.Date(2026, 7, 23, 0, 0, 0, 0, time.UTC) }
	account, err = service.ReauthorizeByToken(context.Background(), account.ID, &TokenInput{AccessToken: "second-access"})
	if err != nil || !account.AuthExpiry.Equal(newExpiry) {
		t.Fatalf("reauthorize account=%+v err=%v", account, err)
	}
	var epochs, ended, logs int
	database.DB().QueryRow("SELECT count(*),sum(ended_at IS NOT NULL) FROM authorization_epochs WHERE account_id=?", account.ID).Scan(&epochs, &ended)
	database.DB().QueryRow("SELECT count(*) FROM status_change_log WHERE account_id=?", account.ID).Scan(&logs)
	if epochs != 2 || ended != 1 || logs != 1 {
		t.Fatalf("epochs=%d ended=%d logs=%d", epochs, ended, logs)
	}

	service.client.(*fakeClient).status.ProviderAccountID = "different-account"
	before := account.AuthExpiry
	_, err = service.ReauthorizeByToken(context.Background(), account.ID, &TokenInput{AccessToken: "mismatch-access"})
	if !errors.As(err, new(*ServiceError)) {
		t.Fatalf("mismatch error=%v", err)
	}
	after, _ := service.Get(context.Background(), account.ID)
	if !after.AuthExpiry.Equal(before) {
		t.Fatal("failed reauthorization changed account")
	}
}

func TestDeleteIsIdempotentScrubsAndAllowsReimport(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-delete")
	defer closeDB()
	account, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "delete-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), account.ID); err != nil {
		t.Fatal(err)
	}
	if err := service.Delete(context.Background(), account.ID); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
	if _, err := service.Get(context.Background(), account.ID); err == nil {
		t.Fatal("deleted account remained visible")
	}
	var credentialLength int
	var keyID string
	database.DB().QueryRow("SELECT length(enc_credentials),credential_key_id FROM accounts WHERE id=?", account.ID).Scan(&credentialLength, &keyID)
	if credentialLength != 0 || keyID != "" {
		t.Fatalf("credential length=%d key=%q", credentialLength, keyID)
	}
	reimported, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "new-secret"})
	if err != nil || reimported.ID == account.ID {
		t.Fatalf("reimport=%+v err=%v", reimported, err)
	}
	var oldEpochs int
	database.DB().QueryRow("SELECT count(*) FROM authorization_epochs WHERE account_id=?", account.ID).Scan(&oldEpochs)
	if oldEpochs != 1 {
		t.Fatalf("old epochs=%d", oldEpochs)
	}
}

func TestDatabasePlaintextScanAndBulkKeyRotation(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-rotate")
	defer closeDB()
	secret := "unique-plaintext-scan-credential-4f36f6"
	service.client.(*fakeClient).tokens.AccessToken = secret
	account, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: secret})
	if err != nil {
		t.Fatal(err)
	}
	client := service.client.(*fakeClient)
	client.deviceAuth = chatgpt.DeviceAuthorization{DeviceAuthID: "rotation-device-secret", UserCode: "ROTA-0001", VerifyURL: "https://auth.example/device", Interval: time.Second, ExpiresAt: service.now().Add(time.Minute)}
	device, err := service.StartDeviceImport(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	rotating, err := credentialcrypto.NewKeyring(map[string][]byte{"active": bytes.Repeat([]byte{7}, 32), "new": bytes.Repeat([]byte{8}, 32)}, "new")
	if err != nil {
		t.Fatal(err)
	}
	count, err := rotating.ReencryptAccounts(context.Background(), database.DB())
	if err != nil || count != 2 {
		t.Fatalf("rotation count=%d err=%v", count, err)
	}
	var envelope []byte
	var keyID string
	database.DB().QueryRow("SELECT enc_credentials,credential_key_id FROM accounts WHERE id=?", account.ID).Scan(&envelope, &keyID)
	if keyID != "new" {
		t.Fatalf("key id=%q", keyID)
	}
	plaintext, err := rotating.Open(envelope, credentialcrypto.CredentialAAD(account.ID, "access"))
	if err != nil || !bytes.Contains(plaintext, []byte(secret)) {
		t.Fatalf("rotated plaintext err=%v", err)
	}
	zero(plaintext)
	var deviceEnvelope []byte
	var deviceKeyID string
	if err := database.DB().QueryRow("SELECT enc_device_code,credential_key_id FROM device_auth_sessions WHERE id=?", device.SessionID).Scan(&deviceEnvelope, &deviceKeyID); err != nil {
		t.Fatal(err)
	}
	devicePlaintext, err := rotating.Open(deviceEnvelope, credentialcrypto.DeviceSessionAAD(device.SessionID))
	if err != nil || deviceKeyID != "new" || !bytes.Contains(devicePlaintext, []byte("rotation-device-secret")) {
		t.Fatalf("rotated device session key=%q err=%v", deviceKeyID, err)
	}
	zero(devicePlaintext)
	if _, err := database.DB().Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{databasePath(t, database), databasePath(t, database) + "-wal", databasePath(t, database) + "-shm"} {
		contents, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(secret)) || bytes.Contains(contents, []byte("rotation-device-secret")) || bytes.Contains(contents, []byte("ROTA-0001")) {
			t.Fatalf("plaintext found in %s", filepath.Base(path))
		}
	}
}

func testService(t *testing.T, providerID string) (*Service, *store.Store, *credentialcrypto.Keyring, func()) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	migrations, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))
	database, err := store.Open(context.Background(), filepath.Join(dir, "accounts.db"), os.DirFS(migrations))
	if err != nil {
		t.Fatal(err)
	}
	keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"active": bytes.Repeat([]byte{7}, 32)}, "active")
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Date(2026, 8, 19, 18, 28, 13, 0, time.UTC)
	client := &fakeClient{tokens: chatgpt.TokenSet{AccessToken: "resolved-access"}, status: chatgpt.StatusResult{
		ProviderAccountID: providerID, RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry,
		AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified,
	}}
	service, err := NewService(database.DB(), client, keyring)
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 7, 22, 9, 30, 0, 0, time.UTC) }
	return service, database, keyring, func() { database.Close() }
}

func databasePath(t *testing.T, database *store.Store) string {
	t.Helper()
	var path string
	if err := database.DB().QueryRow("PRAGMA database_list").Scan(new(int), new(string), &path); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNearExpiryUsesInjectedUTCClockAndAliveBoundary(t *testing.T) {
	service, database, _, closeDB := testService(t, "acct-near")
	defer closeDB()
	account, err := service.ImportByToken(context.Background(), &TokenInput{AccessToken: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	now := service.now().UTC()
	boundary := now.Add(3 * 24 * time.Hour)
	if _, err := database.DB().Exec("UPDATE accounts SET auth_expiry=? WHERE id=?", boundary.Format(time.RFC3339Nano), account.ID); err != nil {
		t.Fatal(err)
	}
	got, err := service.Get(context.Background(), account.ID)
	if err != nil || !got.NearExpiry {
		t.Fatalf("account=%+v err=%v", got, err)
	}
	if _, err := database.DB().Exec("UPDATE settings SET value='1' WHERE key='near_expiry_days'"); err != nil {
		t.Fatal(err)
	}
	got, err = service.Get(context.Background(), account.ID)
	if err != nil || got.NearExpiry {
		t.Fatalf("configured near-expiry threshold was ignored: near_expiry=%v err=%v", got.NearExpiry, err)
	}
	if _, err := database.DB().Exec("UPDATE accounts SET status='dead_normal' WHERE id=?", account.ID); err != nil {
		t.Fatal(err)
	}
	got, err = service.Get(context.Background(), account.ID)
	if err != nil || got.NearExpiry {
		t.Fatalf("dead account near_expiry=%v err=%v", got.NearExpiry, err)
	}
}

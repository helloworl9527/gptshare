package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
)

// TestLiveAccountImport is opt-in. It consumes a real credential only from a
// 0600 regular file and never prints the credential, request body, ciphertext,
// or provider response. Normal test runs skip it.
func TestLiveAccountImport(t *testing.T) {
	credentialPath := os.Getenv("CHATGPT_LIVE_CREDENTIAL_FILE")
	if credentialPath == "" {
		t.Skip("CHATGPT_LIVE_CREDENTIAL_FILE is not set")
	}
	info, err := os.Lstat(credentialPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatal("live credential must be a regular 0600 file")
	}
	credential, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal("read live credential file")
	}
	defer zeroLive(credential)
	credential = bytes.TrimSpace(credential)
	if len(credential) == 0 || len(credential) > 16*1024 {
		t.Fatal("live credential length is invalid")
	}
	secret := string(credential)

	var database *sql.DB
	h := newHarness(t, func(db *sql.DB) httpapi.AccountService {
		database = db
		keyring, keyErr := credentialcrypto.NewKeyring(map[string][]byte{"live-test": bytes.Repeat([]byte{0xa7}, 32)}, "live-test")
		if keyErr != nil {
			t.Fatal(keyErr)
		}
		service, serviceErr := account.NewService(db, chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified}), keyring)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return service
	})
	defer h.close()
	csrf := login(t, h)

	created, importHash := liveWrite(t, h, http.MethodPost, "/api/accounts/import/token", csrf, map[string]string{"label": "STEP-04 live verification", "access_token": secret}, http.StatusCreated, secret)
	if created.Plan != "plus" || created.Status != "alive" || created.CurrentExpiry == nil || created.CurrentExpiry.UTC().Format("2006-01-02") != "2026-08-19" || created.AuthExpiry.UTC().Format("2006-01-02") != "2026-08-19" {
		t.Fatalf("live account status did not match approved truth: plan=%s status=%s current_expiry=%v auth_expiry=%s", created.Plan, created.Status, created.CurrentExpiry, created.AuthExpiry.Format(time.RFC3339))
	}

	response := h.request(t, http.MethodGet, "/api/accounts/"+strconvID(created.ID), nil, "", "")
	assertStatus(t, response, http.StatusOK)
	detailBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || containsLiveSecret(detailBody, secret) {
		t.Fatal("account detail read failed or exposed credential material")
	}
	detailHash := sha256.Sum256(detailBody)

	reauthorized, reauthorizeHash := liveWrite(t, h, http.MethodPost, "/api/accounts/"+strconvID(created.ID)+"/reauthorize/token", csrf, map[string]string{"access_token": secret}, http.StatusOK, secret)
	if reauthorized.ID != created.ID {
		t.Fatal("reauthorization changed the local account identity")
	}
	var epochs, ended int
	if err := database.QueryRow("SELECT count(*),sum(ended_at IS NOT NULL) FROM authorization_epochs WHERE account_id=?", created.ID).Scan(&epochs, &ended); err != nil || epochs != 2 || ended != 1 {
		t.Fatalf("authorization epoch trace is invalid: epochs=%d ended=%d", epochs, ended)
	}

	if containsLiveSecret(h.logs.Bytes(), secret) {
		t.Fatal("access log exposed credential material")
	}
	var dbPath string
	if err := database.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	if _, err := database.ExecContext(context.Background(), "PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		contents, readErr := os.ReadFile(filepath.Clean(path))
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil || containsLiveSecret(contents, secret) {
			t.Fatal("database plaintext scan failed")
		}
	}
	response = h.request(t, http.MethodDelete, "/api/accounts/"+strconvID(created.ID), nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	accountRef := sha256.Sum256([]byte(created.ProviderAccountID))
	t.Logf("live_verified account_ref=%s plan=%s expiry=%s status=%s import_sha256=%s detail_sha256=%s reauthorize_sha256=%s epochs=%d ended=%d plaintext_matches=0",
		hex.EncodeToString(accountRef[:6]), created.Plan, created.AuthExpiry.UTC().Format("2006-01-02"), created.Status,
		hex.EncodeToString(importHash[:]), hex.EncodeToString(detailHash[:]), hex.EncodeToString(reauthorizeHash[:]), epochs, ended)
	secret = ""
}

func liveWrite(t *testing.T, h *harness, method, path, csrf string, body map[string]string, want int, secret string) (account.Account, [32]byte) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal("encode live request")
	}
	defer zeroLive(encoded)
	request, err := http.NewRequest(method, h.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal("create live request")
	}
	request.Header.Set("Origin", h.server.URL)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-CSRF-Token", csrf)
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatal("execute live request")
	}
	assertStatus(t, response, want)
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || containsLiveSecret(contents, secret) {
		t.Fatal("live response read failed or exposed credential material")
	}
	var result account.Account
	if err := json.Unmarshal(contents, &result); err != nil {
		t.Fatal("decode live account response")
	}
	return result, sha256.Sum256(contents)
}

func containsLiveSecret(contents []byte, secret string) bool {
	if secret == "" {
		return false
	}
	if bytes.Contains(contents, []byte(secret)) {
		return true
	}
	trimmed := strings.TrimSpace(secret)
	return len(trimmed) >= 16 && bytes.Contains(contents, []byte(trimmed[:16]))
}

func zeroLive(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

package e2e

import (
	"bytes"
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

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
)

func TestLiveDeviceAuthorization(t *testing.T) {
	mode := os.Getenv("CHATGPT_LIVE_DEVICE_MODE")
	if mode == "" {
		t.Skip("CHATGPT_LIVE_DEVICE_MODE is not set")
	}
	liveDir := filepath.Clean(os.Getenv("CHATGPT_LIVE_DEVICE_DIR"))
	if mode != "start" && mode != "poll" {
		t.Fatal("live device mode must be start or poll")
	}
	if !validLiveDeviceDir(liveDir) {
		t.Fatal("live device directory must be a dedicated 0700 directory under the system temporary directory")
	}
	databasePath := filepath.Join(liveDir, "device.db")
	statePath := filepath.Join(liveDir, "session_id")
	keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"live-device": bytes.Repeat([]byte{0xb3}, 32)}, "live-device")
	if err != nil {
		t.Fatal(err)
	}
	var database *sql.DB
	h := newHarnessAt(t, databasePath, func(db *sql.DB) httpapi.AccountService {
		database = db
		service, serviceErr := account.NewService(db, chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified}), keyring)
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		return service
	})
	if mode == "start" {
		defer h.close()
		liveDeviceStart(t, h, statePath)
		return
	}
	defer func() {
		h.close()
		if validLiveDeviceDir(liveDir) {
			_ = os.RemoveAll(liveDir)
		}
	}()
	liveDevicePoll(t, h, database, keyring, statePath)
}

func liveDeviceStart(t *testing.T, h *harness, statePath string) {
	csrf := login(t, h)
	response := h.request(t, http.MethodPost, "/api/accounts/import/device/start", map[string]string{"label": "STEP-05 live verification"}, csrf, "")
	assertStatus(t, response, http.StatusCreated)
	contents, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal("read live device start response")
	}
	for _, forbidden := range [][]byte{[]byte("device_code"), []byte("device_auth_id"), []byte("access_token"), []byte("refresh_token"), []byte("authorization_code"), []byte("code_verifier")} {
		if bytes.Contains(contents, forbidden) {
			t.Fatal("live device start response exposed forbidden material")
		}
	}
	var started account.DeviceStart
	if err := json.Unmarshal(contents, &started); err != nil || started.SessionID == "" || started.UserCode == "" || started.VerifyURL == "" || started.State != "pending" {
		t.Fatal("live device start response was incomplete")
	}
	if err := os.WriteFile(statePath, []byte(started.SessionID+"\n"), 0o600); err != nil {
		t.Fatal("persist live device session reference")
	}
	startHash := sha256.Sum256(contents)
	t.Logf("ACTION_REQUIRED verify_url=%s user_code=%s expires_at=%s interval_seconds=%d start_sha256=%s", started.VerifyURL, started.UserCode, started.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"), started.IntervalSeconds, hex.EncodeToString(startHash[:]))
}

func liveDevicePoll(t *testing.T, h *harness, database *sql.DB, keyring *credentialcrypto.Keyring, statePath string) {
	info, err := os.Stat(statePath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatal("live device session reference is missing or not 0600")
	}
	sessionBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal("read live device session reference")
	}
	sessionID := strings.TrimSpace(string(sessionBytes))
	zeroLive(sessionBytes)
	var deviceEnvelope []byte
	if err := database.QueryRow("SELECT enc_device_code FROM device_auth_sessions WHERE id=? AND state='pending'", sessionID).Scan(&deviceEnvelope); err != nil {
		t.Fatal("live pending device session not found")
	}
	devicePlaintext, err := keyring.Open(deviceEnvelope, credentialcrypto.DeviceSessionAAD(sessionID))
	if err != nil {
		t.Fatal("decrypt live device session for in-process leak scan")
	}
	var devicePayload struct {
		Authorization struct {
			DeviceAuthID string `json:"device_auth_id"`
			UserCode     string `json:"user_code"`
		} `json:"authorization"`
	}
	if err := json.Unmarshal(devicePlaintext, &devicePayload); err != nil {
		zeroLive(devicePlaintext)
		t.Fatal("decode live device session for in-process leak scan")
	}
	needles := [][]byte{[]byte(devicePayload.Authorization.DeviceAuthID), []byte(devicePayload.Authorization.UserCode)}
	zeroLive(devicePlaintext)
	csrf := login(t, h)
	pollPath := "/api/accounts/import/device/" + sessionID + "/poll"
	response := h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	pollBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal("read live device poll response")
	}
	var poll account.DevicePoll
	if err := json.Unmarshal(pollBody, &poll); err != nil || poll.State != "authorized" || poll.Account == nil {
		t.Fatalf("live device authorization is not complete; state=%s", poll.State)
	}
	var accountEnvelope []byte
	if err := database.QueryRow("SELECT enc_credentials FROM accounts WHERE id=?", poll.Account.ID).Scan(&accountEnvelope); err != nil {
		t.Fatal("read live encrypted account credential")
	}
	accountPlaintext, err := keyring.Open(accountEnvelope, credentialcrypto.CredentialAAD(poll.Account.ID, "device"))
	if err != nil {
		t.Fatal("decrypt live account credential for in-process leak scan")
	}
	var credentials map[string]string
	if err := json.Unmarshal(accountPlaintext, &credentials); err != nil {
		zeroLive(accountPlaintext)
		t.Fatal("decode live account credential for in-process leak scan")
	}
	zeroLive(accountPlaintext)
	for _, value := range credentials {
		if value != "" {
			needles = append(needles, []byte(value))
		}
	}
	credentials = nil
	response = h.request(t, http.MethodGet, "/api/accounts", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	listBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	replayBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if _, err := database.Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, contents := range [][]byte{pollBody, listBody, replayBody, h.logs.Bytes()} {
		if containsAnySecret(contents, needles) {
			t.Fatal("live device material appeared in response or log")
		}
	}
	var dbPath string
	if err := database.QueryRow("PRAGMA database_list").Scan(new(int), new(string), &dbPath); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		contents, readErr := os.ReadFile(path)
		if os.IsNotExist(readErr) {
			continue
		}
		if readErr != nil || containsAnySecret(contents, needles) {
			t.Fatal("live device plaintext scan failed")
		}
	}
	response = h.request(t, http.MethodDelete, "/api/accounts/"+strconvID(poll.Account.ID), nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	for _, needle := range needles {
		zeroLive(needle)
	}
	accountRef := sha256.Sum256([]byte(poll.Account.ProviderAccountID))
	pollHash := sha256.Sum256(pollBody)
	listHash := sha256.Sum256(listBody)
	replayHash := sha256.Sum256(replayBody)
	t.Logf("live_verified account_ref=%s plan=%s expiry=%s status=%s poll_sha256=%s list_sha256=%s replay_sha256=%s plaintext_matches=0 cleanup=deleted",
		hex.EncodeToString(accountRef[:6]), poll.Account.Plan, poll.Account.AuthExpiry.UTC().Format("2006-01-02"), poll.Account.Status,
		hex.EncodeToString(pollHash[:]), hex.EncodeToString(listHash[:]), hex.EncodeToString(replayHash[:]))
}

func containsAnySecret(contents []byte, needles [][]byte) bool {
	for _, needle := range needles {
		if len(needle) > 0 && bytes.Contains(contents, needle) {
			return true
		}
		if len(needle) >= 16 && bytes.Contains(contents, needle[:16]) {
			return true
		}
	}
	return false
}

func validLiveDeviceDir(path string) bool {
	if path == "" || filepath.Dir(path) != filepath.Clean(os.TempDir()) || !strings.HasPrefix(filepath.Base(path), "chatgpt-step05-live.") {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.IsDir() && info.Mode().Perm() == 0o700
}

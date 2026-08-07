package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
	"chatgpt-monitor/internal/monitor"
)

type monitorAPIStub struct {
	run       monitor.Run
	completed bool
	err       error
}

func (stub *monitorAPIStub) RefreshNow(context.Context, int64) (monitor.Run, bool, error) {
	return stub.run, stub.completed, stub.err
}
func (stub *monitorAPIStub) GetRun(context.Context, string) (monitor.Run, error) {
	return stub.run, stub.err
}

type accountClient struct {
	tokens      chatgpt.TokenSet
	status      chatgpt.StatusResult
	exchangeErr error
	statusErr   error
	deviceAuth  chatgpt.DeviceAuthorization
	devicePolls []chatgpt.DevicePollResult
	startCalls  int
	pollCalls   int
	oauthCode   string
}

func (client *accountClient) BuildOAuthAuthorizationURL(state, challenge string) string {
	return "https://auth.example/oauth?state=" + url.QueryEscape(state) + "&code_challenge=" + url.QueryEscape(challenge)
}

func (client *accountClient) ExchangeOAuthCode(_ context.Context, code, _ string) (chatgpt.TokenSet, error) {
	client.oauthCode = code
	return client.tokens, client.exchangeErr
}

func (client *accountClient) StartDeviceAuthorization(context.Context) (chatgpt.DeviceAuthorization, error) {
	client.startCalls++
	return client.deviceAuth, client.exchangeErr
}

func (client *accountClient) PollDeviceAuthorizationResult(context.Context, chatgpt.DeviceAuthorization) (chatgpt.DevicePollResult, error) {
	client.pollCalls++
	if client.exchangeErr != nil {
		return chatgpt.DevicePollResult{}, client.exchangeErr
	}
	if len(client.devicePolls) == 0 {
		return chatgpt.DevicePollResult{State: chatgpt.DevicePollPending, RetryAfter: time.Second}, nil
	}
	result := client.devicePolls[0]
	client.devicePolls = client.devicePolls[1:]
	return result, nil
}

func (client *accountClient) ExchangeCredential(context.Context, chatgpt.CredentialKind, string) (chatgpt.TokenSet, error) {
	return client.tokens, client.exchangeErr
}

func (client *accountClient) FetchStatus(context.Context, string) (chatgpt.StatusResult, error) {
	return client.status, client.statusErr
}

func TestAccountHTTPSImportReauthorizeDeleteContract(t *testing.T) {
	secret := "e2e-access-secret-never-return-90c3d7"
	expiry := time.Date(2026, 8, 19, 18, 28, 13, 0, time.UTC)
	upstream := &accountClient{
		tokens: chatgpt.TokenSet{AccessToken: "resolved-" + secret},
		status: chatgpt.StatusResult{
			ProviderAccountID: "acct-e2e", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus,
			SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive,
			EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified,
		},
	}
	var database *sql.DB
	h := newHarness(t, func(db *sql.DB) httpapi.AccountService {
		database = db
		keyring, err := credentialcrypto.NewKeyring(map[string][]byte{"e2e": bytes.Repeat([]byte{9}, 32)}, "e2e")
		if err != nil {
			t.Fatal(err)
		}
		service, err := account.NewService(db, upstream, keyring)
		if err != nil {
			t.Fatal(err)
		}
		return service
	})
	defer h.close()

	response := h.request(t, http.MethodPost, "/api/accounts/import/token", map[string]string{"access_token": secret}, "valid-looking-but-not-session-bound-csrf", "")
	assertStatus(t, response, http.StatusUnauthorized)
	csrf := login(t, h)

	request, err := http.NewRequest(http.MethodPost, h.server.URL+"/api/accounts/import/token", strings.NewReader(`{"access_token":"ignored"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", h.server.URL)
	request.Header.Set("Content-Type", "text/plain")
	request.Header.Set("X-CSRF-Token", csrf)
	response, err = h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, response, http.StatusUnsupportedMediaType)

	response = h.request(t, http.MethodPost, "/api/accounts/import/token", map[string]string{"access_token": secret, "unknown": "rejected"}, csrf, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)

	response = h.request(t, http.MethodPost, "/api/accounts/import/token", map[string]string{"label": "Primary", "access_token": secret}, csrf, "")
	assertStatus(t, response, http.StatusCreated)
	createdBody, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(createdBody, []byte(secret)) || bytes.Contains(createdBody, []byte("resolved-"+secret)) || bytes.Contains(createdBody, []byte("enc_credentials")) {
		t.Fatal("credential material appeared in import response")
	}
	var created account.Account
	if err := json.Unmarshal(createdBody, &created); err != nil {
		t.Fatal(err)
	}
	if created.Plan != "plus" || created.Status != "alive" || !created.AuthExpiry.Equal(expiry) || created.Credential.Type != "access" || !created.Credential.Configured {
		t.Fatalf("created=%+v", created)
	}

	response = h.request(t, http.MethodPost, "/api/accounts/import/token", map[string]string{"access_token": "different"}, csrf, "")
	assertStatus(t, response, http.StatusConflict)
	response = h.request(t, http.MethodGet, "/api/accounts", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	listBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if bytes.Contains(listBody, []byte(secret)) || !bytes.Contains(listBody, []byte(`"plan":"plus"`)) || !bytes.Contains(listBody, []byte(`"configured":true`)) {
		t.Fatalf("unsafe or incomplete list response: %s", listBody)
	}

	newExpiry := expiry.Add(30 * 24 * time.Hour)
	upstream.status.SubscriptionExpiry = &newExpiry
	response = h.request(t, http.MethodPost, "/api/accounts/"+strconvID(created.ID)+"/reauthorize/token", map[string]string{"session_token": "replacement-session-secret"}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	var epochs, ended int
	if err := database.QueryRow("SELECT count(*),sum(ended_at IS NOT NULL) FROM authorization_epochs WHERE account_id=?", created.ID).Scan(&epochs, &ended); err != nil {
		t.Fatal(err)
	}
	if epochs != 2 || ended != 1 {
		t.Fatalf("epochs=%d ended=%d", epochs, ended)
	}

	response = h.request(t, http.MethodDelete, "/api/accounts/"+strconvID(created.ID), nil, "wrong-session-csrf", "")
	assertStatus(t, response, http.StatusForbidden)
	response = h.request(t, http.MethodDelete, "/api/accounts/"+strconvID(created.ID), nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	response = h.request(t, http.MethodDelete, "/api/accounts/"+strconvID(created.ID), nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	response = h.request(t, http.MethodGet, "/api/accounts/"+strconvID(created.ID), nil, "", "")
	assertStatus(t, response, http.StatusNotFound)
}

func TestAccountHTTPSMapsRetryableProviderFailureTo503(t *testing.T) {
	upstream := &accountClient{exchangeErr: &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: "upstream_timeout", Retryable: true}}
	h := newHarness(t, func(db *sql.DB) httpapi.AccountService {
		keyring, _ := credentialcrypto.NewKeyring(map[string][]byte{"e2e": bytes.Repeat([]byte{4}, 32)}, "e2e")
		service, _ := account.NewService(db, upstream, keyring)
		return service
	})
	defer h.close()
	csrf := login(t, h)
	response := h.request(t, http.MethodPost, "/api/accounts/import/token", map[string]string{"access_token": "not-returned"}, csrf, "")
	assertStatus(t, response, http.StatusServiceUnavailable)
	var result map[string]any
	decode(t, response, &result)
	if result["code"] != "upstream_timeout" {
		t.Fatalf("response=%v", result)
	}
}

func TestAccountHTTPSOAuthManualCallbackSecurityContract(t *testing.T) {
	expiry := time.Date(2026, 8, 19, 18, 28, 13, 0, time.UTC)
	upstream := &accountClient{
		tokens: chatgpt.TokenSet{
			AccessToken:  "oauth-e2e-access-must-not-leak",
			RefreshToken: "oauth-e2e-refresh-must-not-leak",
		},
		status: chatgpt.StatusResult{
			ProviderAccountID: "acct-oauth-e2e", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus,
			SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive,
			EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified,
		},
	}
	h := newHarness(t, func(db *sql.DB) httpapi.AccountService {
		keyring, _ := credentialcrypto.NewKeyring(map[string][]byte{"e2e": bytes.Repeat([]byte{8}, 32)}, "e2e")
		service, _ := account.NewService(db, upstream, keyring)
		return service
	})
	defer h.close()

	response := h.request(t, http.MethodPost, "/api/accounts/import/oauth/start", map[string]string{"label": "OAuth"}, "not-bound", "")
	assertStatus(t, response, http.StatusUnauthorized)
	csrf := login(t, h)
	response = h.request(t, http.MethodPost, "/api/accounts/import/oauth/start", map[string]string{"label": "OAuth"}, "wrong", "")
	assertStatus(t, response, http.StatusForbidden)
	response = h.request(t, http.MethodPost, "/api/accounts/import/oauth/start", map[string]string{"label": "OAuth"}, csrf, "")
	assertStatus(t, response, http.StatusCreated)
	startBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range [][]byte{[]byte("code_verifier"), []byte("oauth-e2e-access"), []byte("oauth-e2e-refresh")} {
		if bytes.Contains(startBody, forbidden) {
			t.Fatalf("OAuth start exposed forbidden material: %s", startBody)
		}
	}
	var started account.OAuthStart
	if err := json.Unmarshal(startBody, &started); err != nil {
		t.Fatal(err)
	}
	authorizationURL, err := url.Parse(started.AuthorizationURL)
	if err != nil || authorizationURL.Query().Get("state") == "" {
		t.Fatalf("authorization URL=%q err=%v", started.AuthorizationURL, err)
	}
	callback := "http://localhost:1455/auth/callback?code=e2e-code&state=" + url.QueryEscape(authorizationURL.Query().Get("state"))
	response = h.request(t, http.MethodPost, "/api/accounts/oauth/"+started.SessionID+"/complete", map[string]string{"callback_url": callback}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	doneBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range [][]byte{[]byte("e2e-code"), []byte("oauth-e2e-access"), []byte("oauth-e2e-refresh"), []byte("callback_url")} {
		if bytes.Contains(doneBody, forbidden) {
			t.Fatalf("OAuth complete exposed forbidden material: %s", doneBody)
		}
	}
	if upstream.oauthCode != "e2e-code" {
		t.Fatalf("authorization code not exchanged: %q", upstream.oauthCode)
	}
	response = h.request(t, http.MethodPost, "/api/accounts/oauth/"+started.SessionID+"/complete", map[string]string{"callback_url": callback}, csrf, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
}

func TestAccountHTTPSDeviceStartPollReplayAndResponseAllowlist(t *testing.T) {
	expiry := time.Date(2026, 8, 19, 18, 28, 13, 0, time.UTC)
	upstream := &accountClient{
		status: chatgpt.StatusResult{
			ProviderAccountID: "acct-device-e2e", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus,
			SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive,
			EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified,
		},
		deviceAuth: chatgpt.DeviceAuthorization{
			DeviceAuthID: "device-code-must-not-leak", UserCode: "LIVE-CODE", VerifyURL: "https://auth.example/device",
			Interval: time.Second, ExpiresAt: time.Now().Add(time.Minute),
		},
		devicePolls: []chatgpt.DevicePollResult{
			{State: chatgpt.DevicePollPending, RetryAfter: time.Second},
			{State: chatgpt.DevicePollAuthorized, Tokens: chatgpt.TokenSet{AccessToken: "device-access-must-not-leak", RefreshToken: "device-refresh-must-not-leak"}},
		},
	}
	h := newHarness(t, func(db *sql.DB) httpapi.AccountService {
		keyring, _ := credentialcrypto.NewKeyring(map[string][]byte{"e2e": bytes.Repeat([]byte{6}, 32)}, "e2e")
		service, _ := account.NewService(db, upstream, keyring)
		return service
	})
	defer h.close()
	csrf := login(t, h)

	response := h.request(t, http.MethodPost, "/api/accounts/import/device/start", map[string]string{"label": "Device"}, "wrong-session-csrf", "")
	assertStatus(t, response, http.StatusForbidden)
	response = h.request(t, http.MethodPost, "/api/accounts/import/device/start", map[string]string{"label": "Device", "unknown": "rejected"}, csrf, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = h.request(t, http.MethodPost, "/api/accounts/import/device/start", map[string]string{"label": "Device"}, csrf, "")
	assertStatus(t, response, http.StatusCreated)
	startBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range [][]byte{[]byte("device-code-must-not-leak"), []byte("access_token"), []byte("refresh_token"), []byte("device_auth_id")} {
		if bytes.Contains(startBody, forbidden) {
			t.Fatalf("start response exposed forbidden material")
		}
	}
	var started account.DeviceStart
	if err := json.Unmarshal(startBody, &started); err != nil || started.UserCode != "LIVE-CODE" || started.VerifyURL == "" || started.State != "pending" {
		t.Fatalf("start=%+v err=%v", started, err)
	}
	pollPath := "/api/accounts/import/device/" + started.SessionID + "/poll"
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	if upstream.pollCalls != 0 {
		t.Fatalf("early poll reached upstream calls=%d", upstream.pollCalls)
	}
	time.Sleep(1100 * time.Millisecond)
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	if upstream.pollCalls != 1 {
		t.Fatalf("pending calls=%d", upstream.pollCalls)
	}
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	if upstream.pollCalls != 1 {
		t.Fatalf("high-frequency poll reached upstream calls=%d", upstream.pollCalls)
	}
	time.Sleep(1100 * time.Millisecond)
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	doneBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, forbidden := range [][]byte{[]byte("device-access-must-not-leak"), []byte("device-refresh-must-not-leak"), []byte("device-code-must-not-leak"), []byte("LIVE-CODE")} {
		if bytes.Contains(doneBody, forbidden) {
			t.Fatal("poll response exposed device material")
		}
	}
	var done account.DevicePoll
	if err := json.Unmarshal(doneBody, &done); err != nil || done.State != "authorized" || done.Account == nil || done.Account.Plan != "plus" {
		t.Fatalf("done=%+v err=%v", done, err)
	}
	response = h.request(t, http.MethodPost, pollPath, map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	if upstream.pollCalls != 2 {
		t.Fatalf("replay reached upstream calls=%d", upstream.pollCalls)
	}
	if bytes.Contains(h.logs.Bytes(), []byte("device-code-must-not-leak")) || bytes.Contains(h.logs.Bytes(), []byte("device-access-must-not-leak")) {
		t.Fatal("device material appeared in access log")
	}
}

func TestMonitorHTTPSRefreshContractCSRFConflictAndAsyncQuery(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	stub := &monitorAPIStub{run: monitor.Run{ID: "monitor-run-id-0123456789", State: "completed", Trigger: "manual", AccountID: pointerInt64(1), StartedAt: now, FinishedAt: &now, AccountsTotal: 1, AccountsOK: 1, ErrorCounts: map[string]int{}}, completed: true}
	h := newHarnessAtWithMonitor(t, filepath.Join(dir, "monitor.db"), nil, func(*sql.DB) httpapi.MonitorService { return stub })
	defer h.close()
	csrf := login(t, h)
	response := h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, "wrong-csrf", "")
	assertStatus(t, response, http.StatusForbidden)
	response.Body.Close()
	response = h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	var completed monitor.Run
	decode(t, response, &completed)
	if completed.ID != stub.run.ID || completed.State != "completed" {
		t.Fatalf("completed=%+v", completed)
	}
	stub.completed = false
	stub.run.State = "running"
	stub.run.FinishedAt = nil
	response = h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusAccepted)
	response.Body.Close()
	response = h.request(t, http.MethodGet, "/api/poll-runs/"+stub.run.ID, nil, "", "")
	assertStatus(t, response, http.StatusOK)
	response.Body.Close()
	stub.err = &monitor.ConflictError{RunID: stub.run.ID}
	response = h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusConflict)
	var conflict map[string]any
	decode(t, response, &conflict)
	if conflict["run_id"] != stub.run.ID {
		t.Fatalf("conflict=%v", conflict)
	}
	stub.err = &monitor.ReauthorizationRequiredError{}
	response = h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusConflict)
	var authorizationWarning map[string]any
	decode(t, response, &authorizationWarning)
	if authorizationWarning["code"] != "reauthorization_required" {
		t.Fatalf("authorization warning=%v", authorizationWarning)
	}
}

func TestMonitorHTTPSRealServiceRefreshPersistsSnapshotWithoutPlaintext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Add(20 * 24 * time.Hour)
	upstream := &accountClient{status: chatgpt.StatusResult{ProviderAccountID: "acct-monitor-live", RawPlan: "chatgptplusplan", Plan: chatgpt.PlanPlus, SubscriptionExpiry: &expiry, AccountState: chatgpt.StateActive, EvidenceCode: "access_claim+accounts_check_2xx", EvidenceLevel: chatgpt.EvidenceLiveVerified}}
	keyring, _ := credentialcrypto.NewKeyring(map[string][]byte{"e2e": bytes.Repeat([]byte{0x42}, 32)}, "e2e")
	secret := monitorJWT("acct-monitor-live", time.Now().Add(time.Hour))
	var database *sql.DB
	h := newHarnessAtWithMonitor(t, filepath.Join(dir, "monitor-real.db"), func(db *sql.DB) httpapi.AccountService {
		service, _ := account.NewService(db, upstream, keyring)
		return service
	}, func(db *sql.DB) httpapi.MonitorService {
		database = db
		now := time.Now().UTC()
		plaintext, _ := json.Marshal(map[string]string{"access": secret})
		result, err := db.Exec(`INSERT INTO accounts(provider_account_id,label,token_type,enc_credentials,credential_key_id,plan,raw_plan,current_expiry,auth_expiry,status,last_alive_at,import_time,last_check_state,updated_at)
			VALUES ('acct-monitor-live','Monitor','access',x'','e2e','plus','chatgptplusplan',?,?, 'alive',?,?,'ok',?)`, expiry.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := result.LastInsertId()
		envelope, err := keyring.Seal(plaintext, credentialcrypto.CredentialAAD(id, "access"))
		for i := range plaintext {
			plaintext[i] = 0
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("UPDATE accounts SET enc_credentials=? WHERE id=?", envelope, id); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", id, now.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
		service, err := monitor.New(db, upstream, keyring, monitor.DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		return service
	})
	defer h.close()
	csrf := login(t, h)
	response := h.request(t, http.MethodPost, "/api/accounts/1/refresh", map[string]string{}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	refreshBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	response = h.request(t, http.MethodGet, "/api/accounts/1", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	accountBody, _ := io.ReadAll(response.Body)
	response.Body.Close()
	for _, contents := range [][]byte{refreshBody, accountBody, h.logs.Bytes()} {
		if bytes.Contains(contents, []byte(secret)) || bytes.Contains(contents, []byte(secret[:16])) {
			t.Fatal("monitor credential appeared in response or log")
		}
	}
	if !bytes.Contains(accountBody, []byte(`"last_check_state":"ok"`)) || !bytes.Contains(accountBody, []byte(`"status":"alive"`)) {
		t.Fatalf("account=%s", accountBody)
	}
	if _, err := database.Exec("PRAGMA wal_checkpoint(FULL)"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "monitor-real.db"), filepath.Join(dir, "monitor-real.db-wal"), filepath.Join(dir, "monitor-real.db-shm")} {
		contents, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(contents, []byte(secret)) || bytes.Contains(contents, []byte(secret[:16])) {
			t.Fatalf("plaintext in %s", filepath.Base(path))
		}
	}
}

func monitorJWT(pid string, expiry time.Time) string {
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix(), "pid": pid})
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".fixture"
}

func pointerInt64(value int64) *int64 { return &value }

func login(t *testing.T, h *harness) string {
	t.Helper()
	csrf := h.csrf(t)
	response := h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": h.password}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	decode(t, response, &challenge)
	response = h.request(t, http.MethodPost, "/api/auth/totp", map[string]string{"challenge": challenge.Challenge, "code": h.currentCode(t)}, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	response.Body.Close()
	return h.csrf(t)
}

func strconvID(id int64) string {
	return strconv.FormatInt(id, 10)
}

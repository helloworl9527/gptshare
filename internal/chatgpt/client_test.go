package chatgpt

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testJWT(t *testing.T, accountID, plan string, expiry any) string {
	return testJWTWithExp(t, accountID, plan, expiry, time.Now().Add(time.Hour))
}

func testJWTWithExp(t *testing.T, accountID, plan string, expiry any, tokenExpiry time.Time) string {
	t.Helper()
	claims := map[string]any{
		"exp": tokenExpiry.Unix(),
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id":                accountID,
			"chatgpt_plan_type":                 plan,
			"chatgpt_subscription_active_until": expiry,
		},
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "e30." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestFetchStatusPlansAndExpiry(t *testing.T) {
	tests := []struct {
		name, rawPlan string
		want          Plan
		expires       any
	}{
		{"free", "chatgptfreeplan", PlanFree, nil},
		{"plus", "chatgptplusplan", PlanPlus, "2026-08-19T00:00:00Z"},
		{"team", "chatgptteamplan", PlanTeam, float64(1787097600)},
		{"unknown", "chatgptfutureplan", PlanUnknown, "1787097600"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" || r.Header.Get("Origin") != "https://chatgpt.com" || r.Header.Get("Referer") != "https://chatgpt.com/" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != statusUserAgent {
					t.Fatalf("status request headers are incompatible: %#v", r.Header)
				}
				if r.Header.Get("X-Authorization") != "" || r.Header.Get("Chatgpt-Account-Id") != "" {
					t.Fatalf("status request sent legacy headers: %#v", r.Header)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"acct-test": map[string]any{
					"account":     map[string]any{"account_id": "acct-test"},
					"entitlement": map[string]any{"subscription_plan": tt.rawPlan, "expires_at": tt.expires},
				}}})
			}))
			defer server.Close()
			client := NewClient(Config{StatusURL: server.URL})
			got, err := client.FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
			if err != nil {
				t.Fatal(err)
			}
			if got.Plan != tt.want || got.RawPlan != tt.rawPlan || got.AccountState != StateActive || got.EvidenceLevel != EvidenceContractVerifiedLivePending {
				t.Fatalf("unexpected result: %+v", got)
			}
			if tt.expires != nil && got.SubscriptionExpiry == nil {
				t.Fatal("expiry was not parsed")
			}
			if got.ResponseHash == "" {
				t.Fatal("response hash missing")
			}
		})
	}
}

func TestExplicitLiveEvidenceLevel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"acct-test": map[string]any{
			"account":     map[string]any{"account_id": "acct-test"},
			"entitlement": map[string]any{"subscription_plan": "chatgptplusplan", "expires_at": "2026-08-19T18:28:13Z"},
		}}})
	}))
	defer server.Close()
	client := NewClient(Config{StatusURL: server.URL, EvidenceLevel: EvidenceLiveVerified})
	result, err := client.FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceLevel != EvidenceLiveVerified || result.AccountState != StateActive {
		t.Fatalf("result=%+v", result)
	}
}

func TestInvalidConfiguredEvidenceLevelFailsClosed(t *testing.T) {
	client := NewClient(Config{EvidenceLevel: EvidenceLevel("future-level")})
	if client.evidenceLevel != EvidenceUnverified {
		t.Fatalf("evidence level=%s", client.evidenceLevel)
	}
}

func TestFetchStatusErrorClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		kind   ErrorKind
		state  AccountState
		retry  bool
		banned bool
	}{
		{"revoked", 401, `{"error":{"code":"token_revoked"}}`, ErrorCredentialRevoked, StateCredentialRevoked, false, true},
		{"disabled", 403, `{"error":{"code":"account_disabled"}}`, ErrorAccountDisabled, StateAccountDisabled, false, true},
		{"unauthorized", 401, `{"error":{"code":"invalid_token"}}`, ErrorPermissionDenied, StatePermissionDenied, false, false},
		{"forbidden", 403, `{"error":{"code":"insufficient_scope"}}`, ErrorPermissionDenied, StatePermissionDenied, false, false},
		{"rate limited", 429, `{"error":{"code":"rate_limit"}}`, ErrorRateLimited, StateRateLimited, true, false},
		{"server error", 503, `{"error":{"code":"unavailable"}}`, ErrorUpstreamTransient, StateUpstreamTransient, true, false},
		{"html challenge", 403, `<html>challenge</html>`, ErrorContractChanged, StateContractChanged, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client := NewClient(Config{StatusURL: server.URL})
			got, err := client.FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind != tt.kind || typed.Retryable != tt.retry || typed.BannedCandidate != tt.banned {
				t.Fatalf("error = %#v, want kind=%s retry=%v", err, tt.kind, tt.retry)
			}
			if typed.EvidenceLevel != EvidenceContractVerifiedLivePending || !typed.PreserveBusinessState {
				t.Fatalf("error must be contract_verified_live_pending and preserve state: %#v", typed)
			}
			if got.AccountState != tt.state {
				t.Fatalf("state=%s want=%s", got.AccountState, tt.state)
			}
		})
	}
}

func TestFetchStatusRateLimitCarriesRetryAfter(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":"rate_limit"}}`))
	}))
	defer server.Close()
	_, err := NewClient(Config{StatusURL: server.URL}).FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
	var typed *TypedError
	if !errors.As(err, &typed) || typed.RetryAfter != 7*time.Second {
		t.Fatalf("error=%#v", err)
	}
}

func TestFetchStatusContractChanges(t *testing.T) {
	tests := []struct{ name, body string }{
		{"missing accounts", `{}`},
		{"missing plan", `{"accounts":{"acct-test":{"account":{"account_id":"acct-test"},"entitlement":{"expires_at":"2026-08-19T00:00:00Z"}}}}`},
		{"missing paid expiry", `{"accounts":{"acct-test":{"account":{"account_id":"acct-test"},"entitlement":{"subscription_plan":"chatgptplusplan","expires_at":null}}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(tt.body)) }))
			defer server.Close()
			_, err := NewClient(Config{StatusURL: server.URL}).FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind != ErrorContractChanged || typed.EvidenceLevel != EvidenceContractVerifiedLivePending || typed.BannedCandidate || !typed.PreserveBusinessState {
				t.Fatalf("error=%#v", err)
			}
		})
	}
}

func TestFetchStatusTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()
	client := NewClient(Config{StatusURL: server.URL, HTTPClient: &http.Client{Timeout: 10 * time.Millisecond}})
	got, err := client.FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != ErrorUpstreamTransient || !typed.Retryable || typed.EvidenceLevel != EvidenceContractVerifiedLivePending || typed.BannedCandidate || !typed.PreserveBusinessState || got.AccountState != StateUpstreamTransient {
		t.Fatalf("result=%+v error=%#v", got, err)
	}
}

func TestExpiredAccessRefreshToActiveChain(t *testing.T) {
	now := time.Date(2026, 7, 22, 6, 0, 0, 0, time.UTC)
	expiredAccess := testJWTWithExp(t, "acct-test", "plus", nil, now.Add(-time.Minute))
	freshAccess := testJWTWithExp(t, "acct-test", "plus", nil, now.Add(time.Hour))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-placeholder" {
				t.Fatal("refresh request contract mismatch")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": freshAccess, "refresh_token": "rotated-placeholder"})
		case "/status":
			if r.Header.Get("Authorization") != "Bearer "+freshAccess {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"invalid_token"}}`))
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": map[string]any{"acct-test": map[string]any{
				"account":     map[string]any{"account_id": "acct-test"},
				"entitlement": map[string]any{"subscription_plan": "chatgptplusplan", "expires_at": "2026-08-19T18:28:13Z"},
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient(Config{TokenURL: server.URL + "/token", StatusURL: server.URL + "/status", Now: func() time.Time { return now }})
	result, tokens, err := client.RefreshExpiredAccess(context.Background(), expiredAccess, "refresh-placeholder")
	if err != nil {
		t.Fatal(err)
	}
	if tokens.AccessToken != freshAccess || result.AccountState != StateAccessExpiredRefreshable || result.Plan != PlanPlus || result.EvidenceLevel != EvidenceContractVerifiedLivePending {
		t.Fatalf("result=%+v tokens=%+v", result, tokens)
	}
}

func TestUnverifiedUnknownErrorFailsClosed(t *testing.T) {
	err := classifyHTTP(http.StatusBadRequest, []byte(`{"error":{"code":"future_unknown_code"}}`))
	var typed *TypedError
	if !errors.As(err, &typed) {
		t.Fatalf("error=%#v", err)
	}
	if typed.Kind != ErrorContractChanged || typed.EvidenceLevel != EvidenceUnverified || typed.BannedCandidate || !typed.PreserveBusinessState || typed.Retryable {
		t.Fatalf("unknown error did not fail closed: %#v", typed)
	}
}

func TestRefreshAndSessionExchange(t *testing.T) {
	access := testJWT(t, "acct-test", "plus", nil)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") == "" {
				t.Fatal("refresh form missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "rotated-placeholder"})
		case "/session":
			if _, err := r.Cookie("__Secure-next-auth.session-token"); err != nil {
				t.Fatal("session cookie missing")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"accessToken": access})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := NewClient(Config{TokenURL: server.URL + "/token", SessionURL: server.URL + "/session"})
	for _, test := range []struct {
		kind   CredentialKind
		secret string
	}{{CredentialRefresh, "refresh-placeholder"}, {CredentialSession, "session-placeholder"}} {
		got, err := client.ExchangeCredential(context.Background(), test.kind, test.secret)
		if err != nil || got.AccessToken != access {
			t.Fatalf("kind=%s token=%+v err=%v", test.kind, got, err)
		}
	}
}

func TestDeviceFlow(t *testing.T) {
	access := testJWT(t, "acct-test", "plus", nil)
	polls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/start":
			_ = json.NewEncoder(w).Encode(map[string]any{"device_auth_id": "device-placeholder", "user_code": "ABCD-EFGH", "interval": 1})
		case "/poll":
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"authorization_code": "auth-placeholder", "code_verifier": "verifier-placeholder"})
		case "/token":
			body, _ := ioReadForm(r)
			if body.Get("grant_type") != "authorization_code" {
				t.Fatal("wrong device grant")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": access, "refresh_token": "refresh-placeholder"})
		}
	}))
	defer server.Close()
	client := NewClient(Config{DeviceStartURL: server.URL + "/start", DevicePollURL: server.URL + "/poll", TokenURL: server.URL + "/token"})
	auth, err := client.StartDeviceAuthorization(context.Background())
	if err != nil || auth.UserCode == "" || auth.DeviceAuthID == "" {
		t.Fatalf("auth=%+v err=%v", auth, err)
	}
	if _, pending, err := client.PollDeviceAuthorization(context.Background(), auth); err != nil || !pending {
		t.Fatalf("pending=%v err=%v", pending, err)
	}
	tokens, pending, err := client.PollDeviceAuthorization(context.Background(), auth)
	if err != nil || pending || tokens.RefreshToken == "" {
		t.Fatalf("tokens=%+v pending=%v err=%v", tokens, pending, err)
	}
}

func ioReadForm(r *http.Request) (url.Values, error) {
	if err := r.ParseForm(); err != nil {
		return nil, err
	}
	return r.Form, nil
}

func TestParseTimeFormats(t *testing.T) {
	for _, raw := range []string{`"2026-08-19T00:00:00Z"`, `"1787097600"`, `1787097600`} {
		if _, err := parseTimeValue(json.RawMessage(raw)); err != nil {
			t.Fatalf("raw=%s: %v", raw, err)
		}
	}
}

func TestDecodeTokenSetCapturesAccessExpiry(t *testing.T) {
	expiry := time.Date(2026, 8, 1, 18, 0, 0, 0, time.UTC)
	access := testJWTWithExp(t, "acct-test", "plus", nil, expiry)
	body, err := json.Marshal(map[string]string{"access_token": access, "refresh_token": "rotated", "id_token": "id"})
	if err != nil {
		t.Fatal(err)
	}
	tokens, err := decodeTokenSet(body, "refresh")
	if err != nil || tokens.AccessExpiresAt == nil || !tokens.AccessExpiresAt.Equal(expiry) {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
}

func TestNoRawJSONInErrors(t *testing.T) {
	secretMarker := "sensitive-marker-must-not-escape"
	err := classifyHTTP(401, []byte(fmt.Sprintf(`{"error":{"code":"invalid_token","message":%q}}`, secretMarker)))
	if strings.Contains(err.Error(), secretMarker) {
		t.Fatal("raw upstream content escaped through error")
	}
}

func TestGenericStatusDenialDebugLogContainsOnlySafeDiagnostics(t *testing.T) {
	secretMarker := "sensitive-response-marker"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"error":{"code":"invalid_token","message":%q}}`, secretMarker)))
	}))
	defer server.Close()
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(previous)
	client := NewClient(Config{StatusURL: server.URL})
	result, err := client.FetchStatus(context.Background(), testJWT(t, "acct-test", "plus", nil))
	if err == nil || result.ResponseHash == "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	logged := logs.String()
	if !strings.Contains(logged, "endpoint=accounts_check") || !strings.Contains(logged, "status_code=401") || !strings.Contains(logged, "response_hash="+result.ResponseHash) {
		t.Fatalf("safe diagnostics missing: %s", logged)
	}
	if strings.Contains(logged, secretMarker) || strings.Contains(logged, "Bearer ") {
		t.Fatalf("sensitive response data reached logs: %s", logged)
	}
}

func TestSanitizedFixtures(t *testing.T) {
	accountBody, err := os.ReadFile("testdata/account_check_plus.json")
	if err != nil {
		t.Fatal(err)
	}
	account, err := parseAccountCheck(accountBody, "acct-placeholder")
	if err != nil || account.RawPlan != "chatgptplusplan" || account.SubscriptionExpiry == nil {
		t.Fatalf("account fixture: result=%+v err=%v", account, err)
	}

	fixtures := []struct {
		name   string
		status int
		kind   ErrorKind
	}{
		{"error_account_disabled.json", 403, ErrorAccountDisabled},
		{"error_permission_denied.json", 403, ErrorPermissionDenied},
		{"error_rate_limited.json", 429, ErrorRateLimited},
		{"error_revoked.json", 401, ErrorCredentialRevoked},
		{"error_transient.json", 503, ErrorUpstreamTransient},
	}
	for _, fixture := range fixtures {
		body, readErr := os.ReadFile("testdata/" + fixture.name)
		if readErr != nil {
			t.Fatal(readErr)
		}
		var typed *Error
		if !errors.As(classifyHTTP(fixture.status, body), &typed) || typed.Kind != fixture.kind || typed.EvidenceLevel != EvidenceContractVerifiedLivePending || !typed.PreserveBusinessState {
			t.Fatalf("fixture=%s error=%#v", fixture.name, typed)
		}
	}
}

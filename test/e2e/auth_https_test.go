package e2e

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/auth"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
	"chatgpt-monitor/internal/notify"
	"chatgpt-monitor/internal/store"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

func TestHTTPSLoginCSRFReplayAndLogout(t *testing.T) {
	h := newHarness(t)
	defer h.close()

	response := h.request(t, http.MethodGet, "/api/me", nil, "", "")
	assertStatus(t, response, http.StatusUnauthorized)

	response = h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": h.password}, "", "")
	assertStatus(t, response, http.StatusForbidden)

	preCSRF := h.csrf(t)
	response = h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": "wrong"}, preCSRF, "")
	assertStatus(t, response, http.StatusUnauthorized)
	response = h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": h.password}, preCSRF, "")
	assertStatus(t, response, http.StatusOK)
	var passwordResult struct {
		Challenge string `json:"challenge"`
	}
	decode(t, response, &passwordResult)

	response = h.request(t, http.MethodPost, "/api/auth/totp", map[string]string{"challenge": passwordResult.Challenge, "code": "000000"}, preCSRF, "")
	assertStatus(t, response, http.StatusUnauthorized)
	code := h.currentCode(t)
	response = h.request(t, http.MethodPost, "/api/auth/totp", map[string]string{"challenge": passwordResult.Challenge, "code": code}, preCSRF, "")
	assertStatus(t, response, http.StatusNoContent)
	if body, _ := io.ReadAll(response.Body); len(body) != 0 || bytes.Contains(body, []byte("eyJ")) {
		t.Fatalf("TOTP response exposed token: %q", body)
	}
	var sessionCookie, rotatedCSRF *http.Cookie
	for _, cookie := range response.Cookies() {
		switch cookie.Name {
		case "__Host-session":
			sessionCookie = cookie
		case "__Host-csrf":
			rotatedCSRF = cookie
		}
	}
	if sessionCookie == nil || !sessionCookie.Secure || !sessionCookie.HttpOnly || sessionCookie.Path != "/" || sessionCookie.Domain != "" || sessionCookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid session cookie: %#v", sessionCookie)
	}
	if rotatedCSRF == nil || rotatedCSRF.Value == preCSRF || !rotatedCSRF.Secure || rotatedCSRF.HttpOnly || rotatedCSRF.SameSite != http.SameSiteStrictMode {
		t.Fatalf("invalid rotated CSRF cookie: %#v", rotatedCSRF)
	}

	response = h.request(t, http.MethodGet, "/api/me", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	var me map[string]string
	decode(t, response, &me)
	if me["username"] != "admin" {
		t.Fatalf("me=%v", me)
	}

	response = h.request(t, http.MethodPost, "/api/auth/totp", map[string]string{"challenge": passwordResult.Challenge, "code": code}, rotatedCSRF.Value, "")
	assertStatus(t, response, http.StatusUnauthorized)
	response = h.request(t, http.MethodPost, "/api/auth/logout", nil, "wrong-csrf-token-value-with-entropy", "")
	assertStatus(t, response, http.StatusForbidden)
	response = h.request(t, http.MethodPost, "/api/auth/logout", nil, rotatedCSRF.Value, "")
	assertStatus(t, response, http.StatusNoContent)

	request, err := http.NewRequest(http.MethodGet, h.server.URL+"/api/me", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.AddCookie(sessionCookie)
	response, err = h.server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	assertStatus(t, response, http.StatusUnauthorized)

	postCSRF := h.csrf(t)
	response = h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": h.password}, postCSRF, "")
	assertStatus(t, response, http.StatusOK)
	decode(t, response, &passwordResult)
	response = h.request(t, http.MethodPost, "/api/auth/totp", map[string]string{"challenge": passwordResult.Challenge, "code": code}, postCSRF, "")
	assertStatus(t, response, http.StatusUnauthorized)
}

func TestHTTPSFifthFailureRateLimitsAndIgnoresForgedForwardedFor(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	csrf := h.csrf(t)
	for attempt := 1; attempt <= 5; attempt++ {
		response := h.request(t, http.MethodPost, "/api/auth/password", map[string]string{"username": "admin", "password": "wrong"}, csrf, "203.0.113."+string(rune('0'+attempt)))
		if attempt < 5 {
			assertStatus(t, response, http.StatusUnauthorized)
		} else {
			assertStatus(t, response, http.StatusTooManyRequests)
			if response.Header.Get("Retry-After") == "" {
				t.Fatal("429 missing Retry-After")
			}
		}
	}
}

type harness struct {
	server     *httptest.Server
	client     *http.Client
	closeStore func()
	password   string
	totpSecret string
	logs       *bytes.Buffer
}

func newHarness(t *testing.T, accountFactory ...func(*sql.DB) httpapi.AccountService) *harness {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return newHarnessAt(t, filepath.Join(dir, "e2e.db"), accountFactory...)
}

func newHarnessAt(t *testing.T, databasePath string, accountFactory ...func(*sql.DB) httpapi.AccountService) *harness {
	return newHarnessAtWithMonitor(t, databasePath, firstAccountFactory(accountFactory), nil)
}

func firstAccountFactory(factories []func(*sql.DB) httpapi.AccountService) func(*sql.DB) httpapi.AccountService {
	if len(factories) == 0 {
		return nil
	}
	return factories[0]
}

func newHarnessAtWithMonitor(t *testing.T, databasePath string, accountFactory func(*sql.DB) httpapi.AccountService, monitorFactory func(*sql.DB) httpapi.MonitorService) *harness {
	t.Helper()
	migrations, _ := filepath.Abs(filepath.Join("..", "..", "migrations"))
	database, err := store.Open(context.Background(), databasePath, os.DirFS(migrations))
	if err != nil {
		t.Fatal(err)
	}
	password := "correct horse battery staple"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		t.Fatal(err)
	}
	secretBytes := []byte(strings.Repeat("t", 20))
	manager, err := auth.New(auth.Config{DB: database.DB(), Username: "admin", PasswordHash: string(hash), TOTPSecret: secretBytes, JWTKey: []byte(strings.Repeat("j", 32)), RateLimitKey: []byte(strings.Repeat("r", 32))})
	if err != nil {
		t.Fatal(err)
	}
	logs := new(bytes.Buffer)
	logger := slog.New(slog.NewJSONHandler(logs, nil))
	server := httptest.NewUnstartedServer(nil)
	origin := "https://" + server.Listener.Addr().String()
	var accountService httpapi.AccountService
	if accountFactory != nil {
		accountService = accountFactory(database.DB())
	}
	var monitorService httpapi.MonitorService
	if monitorFactory != nil {
		monitorService = monitorFactory(database.DB())
	}
	settingsKeyring, err := credentialcrypto.NewKeyring(map[string][]byte{"e2e-settings": bytes.Repeat([]byte{7}, 32)}, "e2e-settings")
	if err != nil {
		t.Fatal(err)
	}
	settingsService, err := notify.NewSettingsService(database.DB(), settingsKeyring)
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = httpapi.NewRouter(database, manager, accountService, httpapi.Config{Origin: origin, TrustLoopbackProxy: false, Monitor: monitorService, Settings: settingsService}, logger)
	server.StartTLS()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar
	return &harness{server: server, client: client, closeStore: func() { database.Close() }, password: password, totpSecret: base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secretBytes), logs: logs}
}

func (h *harness) close() {
	h.server.Close()
	h.closeStore()
}

func (h *harness) csrf(t *testing.T) string {
	t.Helper()
	response := h.request(t, http.MethodGet, "/api/auth/csrf", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	var result struct {
		Token string `json:"csrf_token"`
	}
	decode(t, response, &result)
	if len(result.Token) < 32 {
		t.Fatal("CSRF token lacks entropy")
	}
	return result.Token
}

func (h *harness) currentCode(t *testing.T) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(h.totpSecret, uint64(time.Now().Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func (h *harness) request(t *testing.T, method, path string, body any, csrf, forwardedFor string) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		contents, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(contents)
	}
	request, err := http.NewRequest(method, h.server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", h.server.URL)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	if forwardedFor != "" {
		request.Header.Set("X-Forwarded-For", forwardedFor)
	}
	response, err := h.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decode(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func assertStatus(t *testing.T, response *http.Response, want int) {
	t.Helper()
	if response.StatusCode != want {
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		t.Fatalf("status=%d want=%d body=%s", response.StatusCode, want, body)
	}
}

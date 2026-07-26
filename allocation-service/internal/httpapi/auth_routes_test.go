package httpapi

import (
	"bytes"
	"context"
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

	"allocation-service/internal/auth"
	"allocation-service/internal/store"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

func TestAuthHTTPFlowCookiesCSRFHeadersAndAuthorization(t *testing.T) {
	server := testTLSServer(t)
	defer server.Close()
	client := server.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar

	csrf := getCSRF(t, client, server.URL)
	assertSecurityHeaders(t, getRaw(t, client, server.URL+"/health"))

	unauthorized := getRaw(t, client, server.URL+"/api/admin/ping")
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", unauthorized.StatusCode)
	}

	challenge := postPassword(t, client, server.URL, csrf, "admin", "correct horse battery staple", http.StatusOK)
	code := codeAtHTTP(t, testTOTPSecret(), time.Now())
	totpReq := map[string]string{"challenge": challenge, "code": code}
	totpResp := postJSON(t, client, server.URL+"/api/auth/totp", csrf, totpReq)
	if totpResp.StatusCode != http.StatusNoContent {
		t.Fatalf("totp status=%d body=%s", totpResp.StatusCode, readBody(t, totpResp))
	}
	assertAuthCookies(t, totpResp.Cookies())

	me := getRaw(t, client, server.URL+"/api/me")
	if me.StatusCode != http.StatusOK || !strings.Contains(readBody(t, me), `"username":"admin"`) {
		t.Fatalf("me status=%d", me.StatusCode)
	}
	admin := getRaw(t, client, server.URL+"/api/admin/ping")
	if admin.StatusCode != http.StatusOK {
		t.Fatalf("admin status=%d body=%s", admin.StatusCode, readBody(t, admin))
	}

	forbidden := postJSON(t, client, server.URL+"/api/auth/logout", "wrong-csrf", map[string]string{})
	if forbidden.StatusCode != http.StatusForbidden {
		t.Fatalf("csrf status=%d body=%s", forbidden.StatusCode, readBody(t, forbidden))
	}
	rotated := getCSRF(t, client, server.URL)
	logout := postJSON(t, client, server.URL+"/api/auth/logout", rotated, map[string]string{})
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%d body=%s", logout.StatusCode, readBody(t, logout))
	}
	afterLogout := getRaw(t, client, server.URL+"/api/admin/ping")
	if afterLogout.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout status=%d", afterLogout.StatusCode)
	}
}

func TestAuthFailuresLockoutAndTOTPReplayHTTP(t *testing.T) {
	server := testTLSServer(t)
	defer server.Close()
	client := server.Client()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client.Jar = jar
	csrf := getCSRF(t, client, server.URL)
	postPassword(t, client, server.URL, csrf, "missing", "wrong", http.StatusUnauthorized)
	for i := 2; i <= 5; i++ {
		want := http.StatusUnauthorized
		if i == 5 {
			want = http.StatusTooManyRequests
		}
		postPassword(t, client, server.URL, csrf, "admin", "wrong", want)
	}

	server2 := testTLSServer(t)
	defer server2.Close()
	client2 := server2.Client()
	jar2, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client2.Jar = jar2
	csrf2 := getCSRF(t, client2, server2.URL)
	challenge := postPassword(t, client2, server2.URL, csrf2, "admin", "correct horse battery staple", http.StatusOK)
	code := codeAtHTTP(t, testTOTPSecret(), time.Now())
	first := postJSON(t, client2, server2.URL+"/api/auth/totp", csrf2, map[string]string{"challenge": challenge, "code": code})
	if first.StatusCode != http.StatusNoContent {
		t.Fatalf("first totp status=%d body=%s", first.StatusCode, readBody(t, first))
	}
	rotatedCSRF := getCSRF(t, client2, server2.URL)
	secondChallenge := postPassword(t, client2, server2.URL, rotatedCSRF, "admin", "correct horse battery staple", http.StatusOK)
	replay := postJSON(t, client2, server2.URL+"/api/auth/totp", rotatedCSRF, map[string]string{"challenge": secondChallenge, "code": code})
	if replay.StatusCode != http.StatusUnauthorized {
		t.Fatalf("replay status=%d body=%s", replay.StatusCode, readBody(t, replay))
	}
}

func testTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(dir, "auth.db"))
	if err != nil {
		t.Fatal(err)
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("correct horse battery staple"), 12)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := auth.New(auth.Config{
		DB: database.DB(), Username: "admin", PasswordHash: string(hash),
		TOTPSecret: []byte("synthetic-test-secret"), SessionKey: []byte(strings.Repeat("j", 32)), CSRFSigningKey: []byte(strings.Repeat("r", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	router := NewRouter(database, manager, Config{Origin: "https://127.0.0.1"}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	server := httptest.NewTLSServer(router)
	t.Cleanup(func() {
		server.Close()
		database.Close()
	})
	return server
}

func getCSRF(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	resp := getRaw(t, client, baseURL+"/api/auth/csrf")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("csrf status=%d body=%s", resp.StatusCode, readBody(t, resp))
	}
	var body map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return body["csrf_token"]
}

func postPassword(t *testing.T, client *http.Client, baseURL, csrf, username, password string, wantStatus int) string {
	t.Helper()
	resp := postJSON(t, client, baseURL+"/api/auth/password", csrf, map[string]string{"username": username, "password": password})
	if resp.StatusCode != wantStatus {
		t.Fatalf("password status=%d want=%d body=%s", resp.StatusCode, wantStatus, readBody(t, resp))
	}
	if wantStatus != http.StatusOK {
		return ""
	}
	var body struct {
		Challenge string `json:"challenge"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return body.Challenge
}

func postJSON(t *testing.T, client *http.Client, url, csrf string, payload any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://127.0.0.1")
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func getRaw(t *testing.T, client *http.Client, url string) *http.Response {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func assertSecurityHeaders(t *testing.T, resp *http.Response) {
	t.Helper()
	defer resp.Body.Close()
	for _, header := range []string{"X-Content-Type-Options", "X-Frame-Options", "Referrer-Policy", "Content-Security-Policy", "Permissions-Policy", "Strict-Transport-Security"} {
		if resp.Header.Get(header) == "" {
			t.Fatalf("missing security header %s", header)
		}
	}
}

func assertAuthCookies(t *testing.T, cookies []*http.Cookie) {
	t.Helper()
	var session, csrf bool
	for _, cookie := range cookies {
		if cookie.Name == sessionCookie {
			session = true
			if !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("bad session cookie: %#v", cookie)
			}
		}
		if cookie.Name == csrfCookie {
			csrf = true
			if !cookie.Secure || cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" {
				t.Fatalf("bad csrf cookie: %#v", cookie)
			}
		}
	}
	if !session || !csrf {
		t.Fatalf("missing auth cookies session=%v csrf=%v", session, csrf)
	}
}

func codeAtHTTP(t *testing.T, secret string, at time.Time) string {
	t.Helper()
	code, err := hotp.GenerateCodeCustom(secret, uint64(at.Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func testTOTPSecret() string {
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString([]byte("synthetic-test-secret"))
}

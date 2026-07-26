package vitalsapp_test

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
	"strconv"
	"strings"
	"testing"
	"time"

	allocationmodule "allocation-service/module"
	allocationfacade "allocation-service/monitorfacade"
	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/httpapi"
	"chatgpt-monitor/internal/monitor"
	"chatgpt-monitor/internal/notify"
	"chatgpt-monitor/internal/store"
	"chatgpt-monitor/internal/unifiedui"
	"chatgpt-monitor/internal/vitalsapp"
	"github.com/golang-jwt/jwt/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
	"golang.org/x/crypto/bcrypt"
)

const unifiedOrigin = "https://vitals.test"

func TestUnifiedAdminLoginProtectsBothModulesAndPreservesPublicCardBoundary(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	migrationRoot, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	monitorDB, err := store.Open(ctx, filepath.Join(dir, "monitor.db"), os.DirFS(migrationRoot))
	if err != nil {
		t.Fatal(err)
	}
	defer monitorDB.Close()

	password := "correct horse battery staple"
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		t.Fatal(err)
	}
	totpBytes := []byte(strings.Repeat("s", 20))
	manager, err := auth.New(auth.Config{
		DB: monitorDB.DB(), Username: "vitals-admin", PasswordHash: string(passwordHash),
		TOTPSecret: totpBytes, JWTKey: []byte(strings.Repeat("j", 32)), RateLimitKey: []byte(strings.Repeat("r", 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	allocationPath := filepath.Join(dir, "allocation.db")
	allocation, err := allocationmodule.Open(ctx, allocationPath, map[string][]byte{"allocation-test": []byte(strings.Repeat("a", 32))}, "allocation-test", emptyFacade{}, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer allocation.Close()

	app := vitalsapp.New(monitorDB, allocation, backgroundOK{}, allocation, testLogger())
	httpapi.RegisterUnifiedAdminRoutes(app.Engine(), manager, fakeAccounts{}, httpapi.Config{
		Origin: unifiedOrigin, Monitor: fakeMonitor{}, Settings: fakeSettings{},
	})
	boundary := httpapi.UnifiedAdminBoundary(manager, unifiedOrigin)
	if err := allocation.RegisterAdminRoutes(app.Engine(), allocationmodule.AdminBoundary{
		RequireSession: boundary.RequireSession,
		RequireCSRF:    boundary.RequireCSRF,
		RequireOrigin:  boundary.RequireOrigin,
	}); err != nil {
		t.Fatal(err)
	}
	unifiedui.Register(app.Engine())

	server := httptest.NewTLSServer(app.Handler())
	defer server.Close()
	unauthenticated := server.Client()
	for _, path := range []string{
		"/api/accounts", "/api/poll-runs/test-run", "/api/settings",
		"/api/admin/accounts", "/api/admin/cards", "/api/admin/dashboard", "/api/admin/config/security-boundaries",
	} {
		if got := get(t, unauthenticated, server.URL+path, "").StatusCode; got != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s = %d, want 401", path, got)
		}
	}
	for _, path := range []string{"/api/accounts/1/refresh", "/api/admin/cards/generate"} {
		response := postJSONWithOrigin(t, unauthenticated, server.URL+path, "", "", map[string]any{})
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated POST %s without Origin = %d, want 401", path, response.StatusCode)
		}
		response.Body.Close()
	}

	for _, cookieName := range []string{"__Host-allocation-session", "__Host-session"} {
		oldToken := oldAllocationToken(t)
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/api/admin/accounts", nil)
		request.AddCookie(&http.Cookie{Name: cookieName, Value: oldToken})
		response, requestErr := unauthenticated.Do(request)
		if requestErr != nil {
			t.Fatal(requestErr)
		}
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("legacy cookie %s status=%d, want 401", cookieName, response.StatusCode)
		}
		response.Body.Close()
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := server.Client()
	client.Jar = jar
	csrf := fetchCSRF(t, client, server.URL)
	challengeResponse := postJSON(t, client, server.URL+"/api/auth/password", csrf, map[string]string{
		"username": "vitals-admin", "password": password,
	})
	if challengeResponse.StatusCode != http.StatusOK {
		t.Fatalf("password status=%d body=%s", challengeResponse.StatusCode, readBody(t, challengeResponse))
	}
	var challenge struct {
		Challenge string `json:"challenge"`
	}
	decodeBody(t, challengeResponse, &challenge)
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(totpBytes)
	code, err := hotp.GenerateCodeCustom(secret, uint64(time.Now().Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
	if err != nil {
		t.Fatal(err)
	}
	totpResponse := postJSON(t, client, server.URL+"/api/auth/totp", csrf, map[string]string{"challenge": challenge.Challenge, "code": code})
	if totpResponse.StatusCode != http.StatusNoContent {
		t.Fatalf("totp status=%d body=%s", totpResponse.StatusCode, readBody(t, totpResponse))
	}
	totpResponse.Body.Close()
	csrf = fetchCSRF(t, client, server.URL)

	for _, path := range []string{
		"/api/accounts", "/api/poll-runs/test-run", "/api/settings",
		"/api/admin/accounts", "/api/admin/cards", "/api/admin/dashboard", "/api/admin/config/security-boundaries",
	} {
		response := get(t, client, server.URL+path, "")
		if response.StatusCode != http.StatusOK {
			t.Fatalf("authenticated GET %s = %d body=%s", path, response.StatusCode, readBody(t, response))
		}
		if path == "/api/admin/config/security-boundaries" {
			body := readBody(t, response)
			for _, group := range []string{"unified_admin_auth", "monitor_data_encryption", "allocation_data_encryption", `"key_material_exposed":false`} {
				if !strings.Contains(body, group) {
					t.Fatalf("security boundary response missing %q: %s", group, body)
				}
			}
		} else {
			response.Body.Close()
		}
	}

	missingCSRF := postJSON(t, client, server.URL+"/api/admin/cards/generate", "", map[string]int{"quantity": 1, "duration_days": 30})
	if missingCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status=%d, want 403", missingCSRF.StatusCode)
	}
	missingCSRF.Body.Close()
	wrongCSRF := postJSON(t, client, server.URL+"/api/accounts/1/refresh", "wrong-csrf", map[string]any{})
	if wrongCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("wrong CSRF status=%d, want 403", wrongCSRF.StatusCode)
	}
	wrongCSRF.Body.Close()
	legacyCSRFRequest, _ := http.NewRequest(http.MethodPost, server.URL+"/api/admin/cards/generate", strings.NewReader(`{"quantity":1,"duration_days":30}`))
	legacyCSRFRequest.Header.Set("Content-Type", "application/json")
	legacyCSRFRequest.Header.Set("Origin", unifiedOrigin)
	legacyCSRFRequest.Header.Set("X-CSRF-Token", "legacy-allocation-csrf")
	legacyCSRFRequest.AddCookie(&http.Cookie{Name: "__Host-allocation-csrf", Value: "legacy-allocation-csrf"})
	legacyCSRFResponse, err := client.Do(legacyCSRFRequest)
	if err != nil {
		t.Fatal(err)
	}
	if legacyCSRFResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("legacy allocation CSRF status=%d, want 403", legacyCSRFResponse.StatusCode)
	}
	legacyCSRFResponse.Body.Close()
	untrustedOrigin := postJSONWithOrigin(t, client, server.URL+"/api/accounts/1/refresh", csrf, "https://untrusted.test", map[string]any{})
	if untrustedOrigin.StatusCode != http.StatusForbidden {
		t.Fatalf("untrusted Origin status=%d, want 403", untrustedOrigin.StatusCode)
	}
	untrustedOrigin.Body.Close()

	generatedResponse := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]int{"quantity": 1, "duration_days": 30})
	if generatedResponse.StatusCode != http.StatusCreated {
		t.Fatalf("generate status=%d body=%s", generatedResponse.StatusCode, readBody(t, generatedResponse))
	}
	var generated struct {
		Cards []struct {
			ID   int64  `json:"id"`
			Code string `json:"code"`
		} `json:"cards"`
	}
	decodeBody(t, generatedResponse, &generated)
	if len(generated.Cards) != 1 {
		t.Fatalf("generated cards=%d, want 1", len(generated.Cards))
	}
	card := generated.Cards[0]
	listResponse := get(t, client, server.URL+"/api/admin/cards", "")
	listBody := readBody(t, listResponse)
	if listResponse.StatusCode != http.StatusOK || strings.Contains(listBody, card.Code) || strings.Contains(listBody, `"code":`) {
		t.Fatalf("card list exposed plaintext: status=%d body=%s", listResponse.StatusCode, listBody)
	}
	revealURL := server.URL + "/api/admin/cards/" + strconv.FormatInt(card.ID, 10) + "/reveal"
	withoutRevealCSRF := get(t, client, revealURL, "")
	if withoutRevealCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("reveal without CSRF=%d, want 403", withoutRevealCSRF.StatusCode)
	}
	withoutRevealCSRF.Body.Close()
	revealResponse := get(t, client, revealURL, csrf)
	revealBody := readBody(t, revealResponse)
	if revealResponse.StatusCode != http.StatusOK || !strings.Contains(revealBody, card.Code) {
		t.Fatalf("reveal status=%d body=%s", revealResponse.StatusCode, revealBody)
	}

	auditDB, err := sql.Open("sqlite", "file:"+filepath.ToSlash(allocationPath))
	if err != nil {
		t.Fatal(err)
	}
	defer auditDB.Close()
	var revealAudits int
	if err := auditDB.QueryRowContext(ctx, "SELECT count(*) FROM audit_events WHERE action='cards.reveal' AND target_id=?", card.ID).Scan(&revealAudits); err != nil {
		t.Fatal(err)
	}
	if revealAudits != 1 {
		t.Fatalf("reveal audit count=%d, want 1", revealAudits)
	}

	publicPage := get(t, unauthenticated, server.URL+"/", "")
	if publicPage.StatusCode != http.StatusOK {
		t.Fatalf("public page status=%d, want 200", publicPage.StatusCode)
	}
	publicPage.Body.Close()
	publicQuery := postJSON(t, unauthenticated, server.URL+"/api/cards/query", "", map[string]string{"code": "NOT-A-REAL-CARD"})
	if publicQuery.StatusCode == http.StatusUnauthorized {
		t.Fatal("public card query unexpectedly requires admin session")
	}
	publicQuery.Body.Close()
}

type backgroundOK struct{}

func (backgroundOK) BackgroundStatus() string { return "ok" }

type emptyFacade struct{}

func (emptyFacade) ImportForAllocation(context.Context, allocationfacade.ImportRequest) (allocationfacade.ImportResult, error) {
	return allocationfacade.ImportResult{}, allocationfacade.NewFault(allocationfacade.FaultUnavailable)
}
func (emptyFacade) ListAccounts(context.Context) ([]allocationfacade.StatusResult, error) {
	return nil, nil
}
func (emptyFacade) Status(context.Context, string) (allocationfacade.ImportResult, error) {
	return allocationfacade.ImportResult{}, allocationfacade.NewFault(allocationfacade.FaultNotFound)
}
func (emptyFacade) BatchStatus(context.Context, []string) (map[string]allocationfacade.StatusResult, error) {
	return map[string]allocationfacade.StatusResult{}, nil
}
func (emptyFacade) Available(context.Context) bool { return true }

type fakeAccounts struct{}

func (fakeAccounts) ImportByToken(context.Context, *account.TokenInput) (account.Account, error) {
	return account.Account{}, nil
}
func (fakeAccounts) ReauthorizeByToken(context.Context, int64, *account.TokenInput) (account.Account, error) {
	return account.Account{}, nil
}
func (fakeAccounts) Delete(context.Context, int64) error             { return nil }
func (fakeAccounts) List(context.Context) ([]account.Account, error) { return nil, nil }
func (fakeAccounts) Get(context.Context, int64) (account.Account, error) {
	return account.Account{}, nil
}
func (fakeAccounts) StartDeviceImport(context.Context, string) (account.DeviceStart, error) {
	return account.DeviceStart{}, nil
}
func (fakeAccounts) StartDeviceReauthorization(context.Context, int64) (account.DeviceStart, error) {
	return account.DeviceStart{}, nil
}
func (fakeAccounts) PollDevice(context.Context, string) (account.DevicePoll, error) {
	return account.DevicePoll{}, nil
}

type fakeMonitor struct{}

func (fakeMonitor) RefreshNow(context.Context, int64) (monitor.Run, bool, error) {
	return monitor.Run{}, true, nil
}
func (fakeMonitor) GetRun(context.Context, string) (monitor.Run, error) { return monitor.Run{}, nil }

type fakeSettings struct{}

func (fakeSettings) Get(context.Context) (notify.Settings, error) { return notify.Settings{}, nil }
func (fakeSettings) Update(context.Context, notify.Update, string) (notify.Settings, error) {
	return notify.Settings{}, nil
}
func (fakeSettings) DeleteSecret(context.Context, string, string) error { return nil }

func oldAllocationToken(t *testing.T) string {
	t.Helper()
	now := time.Now().UTC()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.RegisteredClaims{
		Issuer: "allocation-service", Subject: "vitals-admin", Audience: jwt.ClaimStrings{"allocation-service-admin"},
		ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), IssuedAt: jwt.NewNumericDate(now), NotBefore: jwt.NewNumericDate(now), ID: "legacy-allocation-jti",
	})
	signed, err := token.SignedString([]byte(strings.Repeat("o", 32)))
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func fetchCSRF(t *testing.T, client *http.Client, baseURL string) string {
	t.Helper()
	response := get(t, client, baseURL+"/api/auth/csrf", "")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("csrf status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	var body struct {
		Token string `json:"csrf_token"`
	}
	decodeBody(t, response, &body)
	return body.Token
}

func postJSON(t *testing.T, client *http.Client, url, csrf string, payload any) *http.Response {
	return postJSONWithOrigin(t, client, url, csrf, unifiedOrigin, payload)
}

func postJSONWithOrigin(t *testing.T, client *http.Client, url, csrf, origin string, payload any) *http.Response {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func get(t *testing.T, client *http.Client, url, csrf string) *http.Response {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}

func decodeBody(t *testing.T, response *http.Response, target any) {
	t.Helper()
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}

func readBody(t *testing.T, response *http.Response) string {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
}

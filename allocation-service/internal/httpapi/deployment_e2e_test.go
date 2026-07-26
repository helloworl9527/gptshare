package httpapi

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/hotp"
)

func TestDeploymentEndToEndFlow(t *testing.T) {
	var monitorOffline atomic.Bool
	phaseOne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("Authorization") != "Bearer monitor-api-key-sentinel" {
			http.Error(w, `{"code":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		if monitorOffline.Load() {
			http.Error(w, `{"code":"provider_unavailable"}`, http.StatusBadGateway)
			return
		}
		switch r.URL.Path {
		case "/api/v1/monitor/accounts/import-for-allocation":
			_, _ = w.Write([]byte(`{"provider_account_id":"phase-one-e2e","email":"e2e-primary@example.test","status":"alive","auth_expiry":"` + time.Now().UTC().Add(30*time.Hour*24).Format(time.RFC3339Nano) + `"}`))
		case "/api/v1/monitor/accounts/batch-status":
			_, _ = w.Write([]byte(`{"items":[{"provider_account_id":"phase-one-e2e","status":"alive"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer phaseOne.Close()

	server := testAccountsTLSServer(t, phaseOne.URL)
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)

	accountID := createDeploymentAccount(t, client, server.URL, csrf, "e2e-primary", "e2e-password", "JBSWY3DPEHPK3PXP", 1, false)
	generated := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 1, "duration_days": 90})
	generatedBody := readBody(t, generated)
	if generated.StatusCode != http.StatusCreated {
		t.Fatalf("generate card status=%d body=%s", generated.StatusCode, generatedBody)
	}
	cardCode := parseFirstGeneratedCode(t, generatedBody)

	redeem := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": cardCode})
	redeemBody := readBody(t, redeem)
	if redeem.StatusCode != http.StatusOK || !strings.Contains(redeemBody, `"allocation_state":"primary"`) {
		t.Fatalf("redeem status=%d body=%s", redeem.StatusCode, redeemBody)
	}

	query := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": cardCode})
	queryBody := readBody(t, query)
	if query.StatusCode != http.StatusOK || !strings.Contains(queryBody, "e2e-password") || !strings.Contains(queryBody, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("query status=%d body=%s", query.StatusCode, queryBody)
	}
	if code, err := hotp.GenerateCodeCustom("JBSWY3DPEHPK3PXP", uint64(time.Now().Unix()/30), hotp.ValidateOpts{Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1}); err != nil || len(code) != 6 {
		t.Fatalf("totp generation failed code=%q err=%v", code, err)
	}

	monitorOffline.Store(true)
	offline := postJSON(t, client, server.URL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     "offline-password",
		"display_2fa_secret":   "JBSWY3DPEHPK3PXQ",
		"max_concurrent_users": 1,
		"monitor_token":        "e2e-monitor-token",
	})
	offlineBody := readBody(t, offline)
	if offline.StatusCode != http.StatusServiceUnavailable || !strings.Contains(offlineBody, "phase_one_monitor_unavailable") {
		t.Fatalf("offline add status=%d body=%s", offline.StatusCode, offlineBody)
	}

	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	now := time.Now().UTC()
	repo.SetNow(func() time.Time { return now.Add(29 * 24 * time.Hour) })
	if _, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "e2e-replacement", DisplayPassword: "replacement-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXR",
		AccountExpiry: now.Add(59 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	run, err := repo.ProcessReplacements(context.Background(), now.Add(29*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Replaced) != 1 || run.Replaced[0].OldAccountID != accountID || run.Replaced[0].Reason != "account_expiring" || run.Replaced[0].GraceUntil == nil {
		t.Fatalf("replacement run=%+v want one expiring grace replacement for account %d", run, accountID)
	}
	replacementQuery := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": cardCode})
	replacementBody := readBody(t, replacementQuery)
	if replacementQuery.StatusCode != http.StatusOK || !strings.Contains(replacementBody, `"allocation_state":"primary"`) || !strings.Contains(replacementBody, `"allocation_state":"grace"`) {
		t.Fatalf("replacement query status=%d body=%s", replacementQuery.StatusCode, replacementBody)
	}

	export := postJSON(t, client, server.URL+"/api/admin/cards/export", csrf, map[string]any{"quantity": 2, "duration_days": 7, "format": "csv"})
	exportBody := readBody(t, export)
	if export.StatusCode != http.StatusOK || !strings.HasPrefix(export.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("export status=%d content-type=%q body=%s", export.StatusCode, export.Header.Get("Content-Type"), exportBody)
	}
	if records, err := csv.NewReader(strings.NewReader(exportBody)).ReadAll(); err != nil || len(records) != 3 || records[0][0] != "code" {
		t.Fatalf("bad export records=%v err=%v", records, err)
	}

	dbValue, ok := testDatabases.Load(server.URL)
	if !ok {
		t.Fatal("test database not registered")
	}
	if _, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "e2e-disabled", DisplayPassword: "disabled-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXQ",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "unknown_monitor", Status: "disabled",
	}); err != nil {
		t.Fatal(err)
	}
	_ = dbValue.(*sql.DB)
	exhaustedCode := createCardForHTTPTest(t, server, "WXYZ-2345-6789", 7)
	exhaustedRedeem := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": exhaustedCode})
	exhaustedBody := readBody(t, exhaustedRedeem)
	if exhaustedRedeem.StatusCode != http.StatusConflict || !strings.Contains(exhaustedBody, "no_account_capacity") {
		t.Fatalf("exhausted redeem status=%d body=%s", exhaustedRedeem.StatusCode, exhaustedBody)
	}
	dashboard := getRaw(t, client, server.URL+"/api/admin/dashboard")
	dashboardBody := readBody(t, dashboard)
	if dashboard.StatusCode != http.StatusOK || !strings.Contains(dashboardBody, `"available_capacity":0`) || !strings.Contains(dashboardBody, `"warning_level":"exhausted"`) {
		t.Fatalf("dashboard warning status=%d body=%s", dashboard.StatusCode, dashboardBody)
	}
}

func createDeploymentAccount(t *testing.T, client *http.Client, baseURL, csrf, username, password, totpSecret string, max int, syncMonitor bool) int64 {
	t.Helper()
	resp := postJSON(t, client, baseURL+"/api/admin/accounts", csrf, map[string]any{
		"display_password":     password,
		"display_2fa_secret":   totpSecret,
		"max_concurrent_users": max,
		"monitor_token":        "e2e-monitor-token",
	})
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create account status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Account struct {
			ID int64 `json:"id"`
		} `json:"account"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Account.ID
}

func parseFirstGeneratedCode(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Cards []struct {
			Code string `json:"code"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Cards) == 0 || !cardsvc.ValidCode(parsed.Cards[0].Code) {
		t.Fatalf("bad generated card body=%s", body)
	}
	return parsed.Cards[0].Code
}

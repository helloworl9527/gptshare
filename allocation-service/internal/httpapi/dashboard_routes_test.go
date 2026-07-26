package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
)

func TestDashboardMetricsAPIMatchesDBFixture(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	now := time.Now().UTC()
	if _, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "dash-a", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "dash-b", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "unknown",
	}); err != nil {
		t.Fatal(err)
	}
	db := databaseForHTTPTest(t, server.URL)
	if _, err := db.Exec("UPDATE chatgpt_accounts SET current_allocations=7 WHERE display_username='dash-a'"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 14; i++ {
		if _, err := db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
			VALUES (?,?,30,'redeemed',?,?,?,?)`, hashForHTTP(i), suffixForHTTP(i), now.Add(-time.Duration(i%7)*24*time.Hour).Format(time.RFC3339Nano), now.Add(30*24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	resp := getRaw(t, client, server.URL+"/api/admin/dashboard")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status=%d body=%s", resp.StatusCode, body)
	}
	var parsed struct {
		Dashboard struct {
			Capacity              int     `json:"capacity"`
			Used                  int     `json:"used"`
			AvailableCapacity     int     `json:"available_capacity"`
			RedeemedLast7Days     int     `json:"redeemed_last_7_days"`
			DailyRedemptionRate   float64 `json:"daily_redemption_rate"`
			DaysToExhaust         float64 `json:"days_to_exhaust"`
			WarningLevel          string  `json:"warning_level"`
			RecommendedAccountAdd int     `json:"recommended_account_add"`
		} `json:"dashboard"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	got := parsed.Dashboard
	if got.Capacity != 15 || got.Used != 7 || got.AvailableCapacity != 8 || got.RedeemedLast7Days != 14 || got.DailyRedemptionRate != 2 || got.DaysToExhaust != 4 || got.WarningLevel != "urgent" || got.RecommendedAccountAdd != 4 {
		t.Fatalf("dashboard mismatch: %+v body=%s", got, body)
	}
}

func TestExhaustedInventoryRejectsNewRedeemButAllowsExistingQuery(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	accountID := createLocalAccountForHTTP(t, client, server.URL, csrf)
	oldCode := createCardForHTTPTest(t, server, "2345-6789-ABCD", 7)
	redeemCardForHTTPTest(t, server, accountID, cardIDForCode(t, server, oldCode), 7)
	newCode := createCardForHTTPTest(t, server, "EFGH-JKMN-PQRS", 7)
	redeem := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": newCode})
	redeemBody := readBody(t, redeem)
	if redeem.StatusCode != http.StatusConflict || !strings.Contains(redeemBody, "no_account_capacity") {
		t.Fatalf("exhausted redeem status=%d body=%s", redeem.StatusCode, redeemBody)
	}
	query := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": oldCode})
	queryBody := readBody(t, query)
	if query.StatusCode != http.StatusOK || !strings.Contains(queryBody, "secret-password") {
		t.Fatalf("existing query status=%d body=%s", query.StatusCode, queryBody)
	}
	dashboard := getRaw(t, client, server.URL+"/api/admin/dashboard")
	dashboardBody := readBody(t, dashboard)
	if dashboard.StatusCode != http.StatusOK || !strings.Contains(dashboardBody, `"warning_level":"exhausted"`) || !strings.Contains(dashboardBody, `"available_capacity":0`) {
		t.Fatalf("dashboard exhausted status=%d body=%s", dashboard.StatusCode, dashboardBody)
	}
}

func hashForHTTP(i int) []byte {
	return cardsvc.HashCode("2345-6789-" + suffixForHTTP(i))
}

func suffixForHTTP(i int) string {
	return "H" + string(rune('A'+i%26)) + string(rune('A'+(i/26)%26)) + "Z"
}

func databaseForHTTPTest(t *testing.T, serverURL string) *sql.DB {
	t.Helper()
	value, ok := testDatabases.Load(serverURL)
	if !ok {
		t.Fatal("test database not registered")
	}
	return value.(*sql.DB)
}

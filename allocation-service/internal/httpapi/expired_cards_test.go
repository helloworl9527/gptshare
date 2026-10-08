package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"allocation-service/internal/repository"
)

func TestExpiredCardsRequiresAdminAndReturnsOnlyCleanupMetadata(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	path := server.URL + "/api/admin/dashboard/expired-cards"
	unauthorized := getRaw(t, server.Client(), path)
	readBody(t, unauthorized)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", unauthorized.StatusCode)
	}
	client := authedAccountClient(t, server)
	now := time.Now().UTC()
	db := databaseForHTTPTest(t, server.URL)
	insert, err := db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
		VALUES (?, 'XYZ9',7,'redeemed',?,?,?,?)`, hashForHTTP(200), now.Add(-8*24*time.Hour).Format(time.RFC3339Nano), now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	cardID, _ := insert.LastInsertId()
	repoValue, _ := testRepositories.Load(server.URL)
	accountID, err := repoValue.(*repository.Repository).CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "archived-cleanup@example.test", DisplayPassword: "cleanup-password-sentinel", DisplayTOTPSecret: "cleanup-totp-sentinel",
		AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (?,?,?,?,'expired',0,?,?)`, cardID, accountID, now.Add(-8*24*time.Hour).Format(time.RFC3339Nano), now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE chatgpt_accounts SET status='disabled',archived_at=? WHERE id=?`, now.Format(time.RFC3339Nano), accountID); err != nil {
		t.Fatal(err)
	}
	resp := getRaw(t, client, path+"?page_size=1&search=xyz9")
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d body=%s cache=%s", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
	var result repository.ExpiredCardPage
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || result.Page != 1 || result.PageSize != 1 || len(result.Cards) != 1 || result.Cards[0].CodeSuffix != "XYZ9" || len(result.Cards[0].Accounts) != 1 || !result.Cards[0].Accounts[0].Archived || result.Cards[0].Accounts[0].DisplayUsername != "archived-cleanup@example.test" {
		t.Fatalf("unexpected page: %+v", result)
	}
	for _, field := range []string{"password", "secret", "encrypted_code", "code_hash"} {
		if strings.Contains(body, field) {
			t.Fatalf("sensitive field %s in response", field)
		}
	}
	for _, query := range []string{"page=0", "page=-1", "page=oops", "page_size=0", "page_size=101", "page_size=oops"} {
		invalid := getRaw(t, client, path+"?"+query)
		readBody(t, invalid)
		if invalid.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("query %s status=%d", query, invalid.StatusCode)
		}
	}
}

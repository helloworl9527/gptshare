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

func TestPasswordChangeDueListAndClear(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	listPath := server.URL + "/api/admin/dashboard/password-change-due"

	unauthorized := getRaw(t, server.Client(), listPath)
	readBody(t, unauthorized)
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", unauthorized.StatusCode)
	}

	client := authedAccountClient(t, server)
	now := time.Now().UTC()
	db := databaseForHTTPTest(t, server.URL)
	repoValue, _ := testRepositories.Load(server.URL)
	repo := repoValue.(*repository.Repository)

	accountID, err := repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "pw-dash@example.test", DisplayPassword: "cleanup-password-sentinel", DisplayTOTPSecret: "cleanup-totp-sentinel",
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	insert, err := db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
		VALUES (?, 'PW01',7,'redeemed',?,?,?,?)`, hashForHTTP(301), now.Add(-8*24*time.Hour).Format(time.RFC3339Nano), now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	cardID, _ := insert.LastInsertId()
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (?,?,?,?,'primary',1,?,?)`, cardID, accountID, now.Add(-8*24*time.Hour).Format(time.RFC3339Nano), now.Add(-24*time.Hour).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	resp := getRaw(t, client, listPath)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d body=%s cache=%s", resp.StatusCode, body, resp.Header.Get("Cache-Control"))
	}
	var listed struct {
		Accounts []repository.PasswordChangeDueAccount `json:"accounts"`
		Total    int                                    `json:"total"`
	}
	if err := json.Unmarshal([]byte(body), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 || len(listed.Accounts) != 1 || listed.Accounts[0].ID != accountID {
		t.Fatalf("unexpected list: %+v", listed)
	}
	for _, field := range []string{"password", "secret", "token", "encrypted_code", "code_hash"} {
		if strings.Contains(body, field) {
			t.Fatalf("sensitive field %s leaked in response", field)
		}
	}

	// 标记已处理后再查，列表为空。
	csrf := getCSRF(t, client, server.URL)
	clearPath := listPath + "/" + itoa(accountID) + "/clear"
	clearResp := postJSON(t, client, clearPath, csrf, nil)
	clearBody := readBody(t, clearResp)
	if clearResp.StatusCode != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", clearResp.StatusCode, clearBody)
	}
	after := getRaw(t, client, listPath)
	afterBody := readBody(t, after)
	if err := json.Unmarshal([]byte(afterBody), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Total != 0 || len(listed.Accounts) != 0 {
		t.Fatalf("cleared account still listed: %+v", listed)
	}
}

func TestClearPasswordChangeRejectsMissingAccount(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	resp := postJSON(t, client, server.URL+"/api/admin/dashboard/password-change-due/999999/clear", csrf, nil)
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(body, `"code":"not_found"`) {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}
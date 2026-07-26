package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
)

func TestRedeemOfflineWarningNoCandidateAndResponseBudget(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	accountID := createLocalAccountForHTTP(t, client, server.URL, csrf)
	code := createCardForHTTPTest(t, server, "2345-6789-ABCD", 7)
	started := time.Now()
	redeem := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": code})
	elapsed := time.Since(started)
	redeemBody := readBody(t, redeem)
	if redeem.StatusCode != http.StatusOK || !strings.Contains(redeemBody, "monitor_unavailable") || !strings.Contains(redeemBody, `"allocation_state":"primary"`) {
		t.Fatalf("redeem status=%d body=%s", redeem.StatusCode, redeemBody)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("redeem exceeded budget: %s", elapsed)
	}
	var parsed struct {
		Allocation struct {
			AccountID int64 `json:"account_id"`
		} `json:"allocation"`
	}
	if err := json.Unmarshal([]byte(redeemBody), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Allocation.AccountID != accountID {
		t.Fatalf("redeemed account=%d want %d", parsed.Allocation.AccountID, accountID)
	}
	duplicate := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": code})
	duplicateBody := readBody(t, duplicate)
	if duplicate.StatusCode != http.StatusOK || !strings.Contains(duplicateBody, `"account_id":`+strconv.FormatInt(accountID, 10)) {
		t.Fatalf("duplicate idempotent status=%d body=%s", duplicate.StatusCode, duplicateBody)
	}
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	count, err := value.(*repository.Repository).ActiveAllocationCount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate redeem created active allocations count=%d", count)
	}

	noCapacityServer := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer noCapacityServer.Close()
	noCapacityCode := createCardForHTTPTest(t, noCapacityServer, "EFGH-JKMN-PQRS", 7)
	noCandidate := postJSON(t, noCapacityServer.Client(), noCapacityServer.URL+"/api/redeem", "", map[string]string{"code": noCapacityCode})
	noCandidateBody := readBody(t, noCandidate)
	if noCandidate.StatusCode != http.StatusConflict || !strings.Contains(noCandidateBody, "no_account_capacity") {
		t.Fatalf("no candidate status=%d body=%s", noCandidate.StatusCode, noCandidateBody)
	}
}

func createCardForHTTPTest(t *testing.T, server *httptest.Server, code string, durationDays int) string {
	t.Helper()
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	if _, err := repo.CreateCard(context.Background(), repository.CardSeed{CodeHash: cardsvc.HashCode(code), CodeSuffix: code[len(code)-4:], DurationDays: durationDays}); err != nil {
		t.Fatal(err)
	}
	return code
}

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

func TestUserQuerySuccessUniformErrorsCaptchaAndBudget(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	accountID := createAccountForQueryHTTP(t, client, server.URL, csrf)
	code := createCardForHTTPTest(t, server, "JKMN-PQRS-TUVW", 7)
	redeemCardForHTTPTest(t, server, accountID, cardIDForCode(t, server, code), 7)
	started := time.Now()
	query := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	elapsed := time.Since(started)
	queryBody := readBody(t, query)
	if query.StatusCode != http.StatusOK {
		t.Fatalf("query status=%d body=%s", query.StatusCode, queryBody)
	}
	if elapsed >= time.Second {
		t.Fatalf("query exceeded budget: %s", elapsed)
	}
	if !strings.Contains(queryBody, "query-account") || !strings.Contains(queryBody, "query-password-sentinel") || !strings.Contains(queryBody, "JBSWY3DPEHPK3PXP") {
		t.Fatalf("query missing credential fields: %s", queryBody)
	}
	if strings.Contains(query.Request.URL.RawQuery, "JBSWY3DPEHPK3PXP") {
		t.Fatal("secret leaked into query URL")
	}
	missing := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": "2345-6789-ABCD"})
	missingBody := readBody(t, missing)
	unredeemedCode := createCardForHTTPTest(t, server, "WXYZ-2345-6789", 7)
	unredeemed := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": unredeemedCode})
	unredeemedBody := readBody(t, unredeemed)
	if missing.StatusCode != http.StatusNotFound || unredeemed.StatusCode != http.StatusNotFound || errorCode(t, missingBody) != errorCode(t, unredeemedBody) || errorMessage(t, missingBody) != errorMessage(t, unredeemedBody) {
		t.Fatalf("non-enumeration mismatch missing=%d %s unredeemed=%d %s", missing.StatusCode, missingBody, unredeemed.StatusCode, unredeemedBody)
	}
	var captcha struct {
		Captcha struct {
			ID       int64  `json:"id"`
			Question string `json:"question"`
		} `json:"captcha"`
	}
	for i := 0; i < 3; i++ {
		resp := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": "BAD-CARD"})
		body := readBody(t, resp)
		if i < 2 && resp.StatusCode != http.StatusNotFound {
			t.Fatalf("bad query %d status=%d body=%s", i+1, resp.StatusCode, body)
		}
		if i == 2 {
			if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "captcha_required") {
				t.Fatalf("captcha status=%d body=%s", resp.StatusCode, body)
			}
			if err := json.Unmarshal([]byte(body), &captcha); err != nil {
				t.Fatal(err)
			}
		}
	}
	pass := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]any{"code": "BAD-CARD", "captcha_id": captcha.Captcha.ID, "captcha_answer": answerForHTTPQuestion(captcha.Captcha.Question)})
	if pass.StatusCode != http.StatusNotFound {
		t.Fatalf("captcha pass uniform status=%d body=%s", pass.StatusCode, readBody(t, pass))
	}
}

func TestUserPageDoesNotUseSecretSinks(t *testing.T) {
	for _, sink := range []string{"localStorage", "sessionStorage", "console.log", "location.search", "URLSearchParams", "history.pushState"} {
		if strings.Contains(userPageHTML, sink) {
			t.Fatalf("user page contains forbidden secret sink %s", sink)
		}
	}
}

func TestUserQueryReturnsPrimaryAndGraceDuringMigration(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	ctx := context.Background()
	now := time.Now().UTC()
	repo.SetNow(func() time.Time { return now })
	oldAccount, err := repo.CreateAccount(ctx, repository.AccountSeed{
		DisplayUsername: "old-query-account", DisplayPassword: "old-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXP", AccountExpiry: now.Add(23 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	code := "2345-6789-ABCD"
	cardID, err := repo.CreateCard(ctx, repository.CardSeed{CodeHash: cardsvc.HashCode(code), CodeSuffix: "ABCD", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCard(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AccountID != oldAccount {
		t.Fatalf("initial account=%d want old=%d", allocation.AccountID, oldAccount)
	}
	newAccount, err := repo.CreateAccount(ctx, repository.AccountSeed{
		DisplayUsername: "new-query-account", DisplayPassword: "new-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXQ", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.ProcessReplacements(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Replaced) != 1 || run.Replaced[0].NewAccountID != newAccount || run.Replaced[0].GraceUntil == nil {
		t.Fatalf("bad replacement run=%+v new=%d", run, newAccount)
	}
	query := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	body := readBody(t, query)
	if query.StatusCode != http.StatusOK {
		t.Fatalf("query status=%d body=%s", query.StatusCode, body)
	}
	if !strings.Contains(body, `"allocation_state":"primary"`) || !strings.Contains(body, `"allocation_state":"grace"`) || !strings.Contains(body, "old-query-account") || !strings.Contains(body, "new-query-account") {
		t.Fatalf("migration query should include primary and grace accounts: %s", body)
	}
	if _, err := repo.ProcessReplacements(ctx, now.Add(25*time.Hour)); err != nil {
		t.Fatal(err)
	}
	after := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	afterBody := readBody(t, after)
	if after.StatusCode != http.StatusOK {
		t.Fatalf("after query status=%d body=%s", after.StatusCode, afterBody)
	}
	if strings.Contains(afterBody, `"allocation_state":"grace"`) || strings.Contains(afterBody, "old-query-account") {
		t.Fatalf("expired grace should be removed from user query: %s", afterBody)
	}
}

func TestUserPageAutomaticRedeemFlowIsIdempotent(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	accountID := createAccountForQueryHTTP(t, client, server.URL, csrf)
	code := createCardForHTTPTest(t, server, "CDEF-HJKM-NPQR", 14)

	initialQuery := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	initialBody := readBody(t, initialQuery)
	if initialQuery.StatusCode != http.StatusNotFound || errorCode(t, initialBody) != "query_not_available" {
		t.Fatalf("initial unredeemed query status=%d body=%s", initialQuery.StatusCode, initialBody)
	}

	redeem := postJSON(t, server.Client(), server.URL+"/api/redeem", "", map[string]string{"code": code})
	redeemBody := readBody(t, redeem)
	if redeem.StatusCode != http.StatusOK || !strings.Contains(redeemBody, `"allocation_state":"primary"`) {
		t.Fatalf("auto redeem status=%d body=%s", redeem.StatusCode, redeemBody)
	}

	firstDisplay := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	firstBody := readBody(t, firstDisplay)
	if firstDisplay.StatusCode != http.StatusOK || !strings.Contains(firstBody, "query-account") || !strings.Contains(firstBody, "query-password-sentinel") {
		t.Fatalf("post-redeem query status=%d body=%s", firstDisplay.StatusCode, firstBody)
	}

	secondDisplay := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
	secondBody := readBody(t, secondDisplay)
	if secondDisplay.StatusCode != http.StatusOK || !strings.Contains(secondBody, `"account_id":`+strconv.FormatInt(accountID, 10)) {
		t.Fatalf("repeat query status=%d body=%s", secondDisplay.StatusCode, secondBody)
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
		t.Fatalf("repeat user input created duplicate active allocation count=%d", count)
	}
}

func TestUserQueryUniformErrorsForExpiredRevokedAndMissing(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	_ = createAccountForQueryHTTP(t, client, server.URL, csrf)
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	ctx := context.Background()

	revokedCode := createCardForHTTPTest(t, server, "CDEF-HJKM-NPQR", 7)
	redeemed, err := repo.RedeemCode(ctx, cardsvc.HashCode(revokedCode), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RevokeCard(ctx, redeemed.Card.ID); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	expiredCode := createCardForHTTPTest(t, server, "STUV-WXYZ-2345", 7)
	repo.SetNow(func() time.Time { return now.Add(-10 * 24 * time.Hour) })
	if _, err := repo.RedeemCode(ctx, cardsvc.HashCode(expiredCode), true); err != nil {
		t.Fatal(err)
	}
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.ExpireDueCards(ctx, now); err != nil {
		t.Fatal(err)
	}

	var wantCode, wantMessage string
	for _, code := range []string{"2345-6789-ABCD", revokedCode, expiredCode} {
		resp := postJSON(t, server.Client(), server.URL+"/api/cards/query", "", map[string]string{"code": code})
		body := readBody(t, resp)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("code=%s status=%d body=%s", code, resp.StatusCode, body)
		}
		gotCode := errorCode(t, body)
		gotMessage := errorMessage(t, body)
		if wantCode == "" {
			wantCode, wantMessage = gotCode, gotMessage
			continue
		}
		if gotCode != wantCode || gotMessage != wantMessage {
			t.Fatalf("uniform error mismatch code=%s got=%s/%s want=%s/%s", code, gotCode, gotMessage, wantCode, wantMessage)
		}
	}
}

func createAccountForQueryHTTP(t *testing.T, client *http.Client, baseURL, csrf string) int64 {
	t.Helper()
	_ = client
	_ = csrf
	value, ok := testRepositories.Load(baseURL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	id, err := value.(*repository.Repository).CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername:    "query-account",
		DisplayPassword:    "query-password-sentinel",
		DisplayTOTPSecret:  "JBSWY3DPEHPK3PXP",
		AccountExpiry:      time.Now().UTC().Add(24 * time.Hour),
		MaxConcurrentUsers: 1,
		MonitorStatus:      "unknown_monitor",
		Status:             "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func cardIDForCode(t *testing.T, serverURLProvider *httptest.Server, code string) int64 {
	t.Helper()
	value, ok := testRepositories.Load(serverURLProvider.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	card, err := repo.CardByHash(context.Background(), cardsvc.HashCode(code))
	if err != nil {
		t.Fatal(err)
	}
	return card.ID
}

func answerForHTTPQuestion(question string) string {
	parts := strings.Split(question, "+")
	left, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
	right, _ := strconv.Atoi(strings.TrimSpace(parts[1]))
	return strconv.Itoa(left + right)
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Code
}

func errorMessage(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Message
}

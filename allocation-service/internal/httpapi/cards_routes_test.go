package httpapi

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"allocation-service/internal/repository"
	"database/sql"
)

var testRepositories sync.Map
var testDatabases sync.Map

func TestAdminCardsGenerateListExportAndPermission(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	unauthorized := postJSONNoCSRF(t, server.Client(), server.URL+"/api/admin/cards/export", map[string]any{"quantity": 1, "duration_days": 7, "format": "txt"})
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized export status=%d body=%s", unauthorized.StatusCode, readBody(t, unauthorized))
	}
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	generated := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 100, "duration_days": 30})
	generatedBody := readBody(t, generated)
	if generated.StatusCode != http.StatusCreated {
		t.Fatalf("generate status=%d body=%s", generated.StatusCode, generatedBody)
	}
	var response struct {
		Cards []struct {
			ID           int64  `json:"id"`
			Code         string `json:"code"`
			CodeSuffix   string `json:"code_suffix"`
			DurationDays int    `json:"duration_days"`
			Status       string `json:"status"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(generatedBody), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Cards) != 100 {
		t.Fatalf("generated cards=%d want 100", len(response.Cards))
	}
	pattern := regexp.MustCompile(`^[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}$`)
	seen := make(map[string]struct{}, 100)
	for _, card := range response.Cards {
		if card.ID == 0 || card.DurationDays != 30 || card.Status != "unused" || !pattern.MatchString(card.Code) || strings.ContainsAny(card.Code, "0OIL1") {
			t.Fatalf("bad generated card: %+v", card)
		}
		if _, exists := seen[card.Code]; exists {
			t.Fatalf("duplicate generated code: %s", card.Code)
		}
		seen[card.Code] = struct{}{}
		if card.CodeSuffix != card.Code[len(card.Code)-4:] {
			t.Fatalf("suffix mismatch card=%+v", card)
		}
	}
	custom := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 1, "duration_days": 5})
	customBody := readBody(t, custom)
	if custom.StatusCode != http.StatusCreated || !strings.Contains(customBody, `"duration_days":5`) {
		t.Fatalf("custom duration status=%d body=%s", custom.StatusCode, customBody)
	}
	ninety := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 1, "duration_days": 90})
	ninetyBody := readBody(t, ninety)
	if ninety.StatusCode != http.StatusCreated || !strings.Contains(ninetyBody, `"duration_days":90`) {
		t.Fatalf("90-day duration status=%d body=%s", ninety.StatusCode, ninetyBody)
	}
	for _, invalidDays := range []int{0, 31, 89, 91} {
		invalid := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 1, "duration_days": invalidDays})
		if invalid.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("invalid duration=%d status=%d body=%s", invalidDays, invalid.StatusCode, readBody(t, invalid))
		}
	}
	list := getRaw(t, client, server.URL+"/api/admin/cards?status=unused&duration_days=30")
	listBody := readBody(t, list)
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", list.StatusCode, listBody)
	}
	for code := range seen {
		if strings.Contains(listBody, code) {
			t.Fatalf("list leaked plaintext code %s: %s", code, listBody)
		}
	}
	first := response.Cards[0]
	unauthClient := &http.Client{Transport: server.Client().Transport}
	unauthReveal := getRaw(t, unauthClient, server.URL+"/api/admin/cards/"+strconv.FormatInt(first.ID, 10)+"/reveal")
	if unauthReveal.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth reveal status=%d body=%s", unauthReveal.StatusCode, readBody(t, unauthReveal))
	}
	forbiddenReveal := getRaw(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(first.ID, 10)+"/reveal")
	if forbiddenReveal.StatusCode != http.StatusForbidden {
		t.Fatalf("csrf reveal status=%d body=%s", forbiddenReveal.StatusCode, readBody(t, forbiddenReveal))
	}
	reveal := getRawCSRF(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(first.ID, 10)+"/reveal", csrf)
	revealBody := readBody(t, reveal)
	if reveal.StatusCode != http.StatusOK || !strings.Contains(revealBody, first.Code) || !strings.Contains(revealBody, `"plaintext_available":true`) {
		t.Fatalf("reveal status=%d body=%s", reveal.StatusCode, revealBody)
	}
	if countAuditEvents(t, server, "cards.reveal") != 1 {
		t.Fatalf("cards.reveal audit count=%d want 1", countAuditEvents(t, server, "cards.reveal"))
	}
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	legacyID, err := value.(*repository.Repository).CreateCard(context.Background(), repository.CardSeed{CodeHash: []byte(strings.Repeat("x", 32)), CodeSuffix: "OLD1", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	legacyReveal := getRawCSRF(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(legacyID, 10)+"/reveal", csrf)
	legacyBody := readBody(t, legacyReveal)
	if legacyReveal.StatusCode != http.StatusOK || !strings.Contains(legacyBody, "明文不可用(旧批次)") || strings.Contains(legacyBody, "OLD1-") {
		t.Fatalf("legacy reveal status=%d body=%s", legacyReveal.StatusCode, legacyBody)
	}
	csvExport := postJSON(t, client, server.URL+"/api/admin/cards/export", csrf, map[string]any{"quantity": 2, "duration_days": 7, "format": "csv"})
	csvBody := readBody(t, csvExport)
	if csvExport.StatusCode != http.StatusOK || !strings.HasPrefix(csvExport.Header.Get("Content-Type"), "text/csv") {
		t.Fatalf("csv export status=%d content-type=%q body=%s", csvExport.StatusCode, csvExport.Header.Get("Content-Type"), csvBody)
	}
	records, err := csv.NewReader(strings.NewReader(csvBody)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 3 || records[0][0] != "code" || records[0][1] != "duration_days" || records[1][1] != "7" {
		t.Fatalf("bad csv records=%v", records)
	}
	txtExport := postJSON(t, client, server.URL+"/api/admin/cards/export", csrf, map[string]any{"quantity": 2, "duration_days": 14, "format": "txt"})
	txtBody := readBody(t, txtExport)
	lines := strings.Fields(strings.TrimSpace(txtBody))
	if txtExport.StatusCode != http.StatusOK || !strings.HasPrefix(txtExport.Header.Get("Content-Type"), "text/plain") || len(lines) != 2 {
		t.Fatalf("txt export status=%d content-type=%q body=%s", txtExport.StatusCode, txtExport.Header.Get("Content-Type"), txtBody)
	}
	for _, line := range lines {
		if !pattern.MatchString(line) {
			t.Fatalf("bad txt code %s", line)
		}
	}
	if countAuditEvents(t, server, "cards.export") != 2 {
		t.Fatalf("cards.export audit count=%d want 2", countAuditEvents(t, server, "cards.export"))
	}
}

func TestCardRevokeBlocksUserQueryAndExtendUpdatesView(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	accountID := createLocalAccountForHTTP(t, client, server.URL, csrf)
	generated := postJSON(t, client, server.URL+"/api/admin/cards/generate", csrf, map[string]any{"quantity": 1, "duration_days": 7})
	generatedBody := readBody(t, generated)
	if generated.StatusCode != http.StatusCreated {
		t.Fatalf("generate status=%d body=%s", generated.StatusCode, generatedBody)
	}
	var parsed struct {
		Cards []struct {
			ID   int64  `json:"id"`
			Code string `json:"code"`
		} `json:"cards"`
	}
	if err := json.Unmarshal([]byte(generatedBody), &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Cards) != 1 {
		t.Fatalf("generated=%+v", parsed)
	}
	redeemCardForHTTPTest(t, server, accountID, parsed.Cards[0].ID, 7)
	view := getRaw(t, server.Client(), server.URL+"/api/cards/"+parsed.Cards[0].Code+"/status")
	viewBody := readBody(t, view)
	if view.StatusCode != http.StatusOK || !strings.Contains(viewBody, `"status":"redeemed"`) {
		t.Fatalf("view status=%d body=%s", view.StatusCode, viewBody)
	}
	var before struct {
		Card struct {
			ValidUntil string `json:"valid_until"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(viewBody), &before); err != nil {
		t.Fatal(err)
	}
	beforeTime, err := time.Parse(time.RFC3339Nano, before.Card.ValidUntil)
	if err != nil {
		t.Fatal(err)
	}
	extended := postJSON(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(parsed.Cards[0].ID, 10)+"/extend", csrf, map[string]any{"days": 14})
	if extended.StatusCode != http.StatusOK {
		t.Fatalf("extend status=%d body=%s", extended.StatusCode, readBody(t, extended))
	}
	after := getRaw(t, server.Client(), server.URL+"/api/cards/"+parsed.Cards[0].Code+"/status")
	afterBody := readBody(t, after)
	var afterParsed struct {
		Card struct {
			ValidUntil string `json:"valid_until"`
		} `json:"card"`
	}
	if err := json.Unmarshal([]byte(afterBody), &afterParsed); err != nil {
		t.Fatal(err)
	}
	afterTime, err := time.Parse(time.RFC3339Nano, afterParsed.Card.ValidUntil)
	if err != nil {
		t.Fatal(err)
	}
	if !afterTime.Equal(beforeTime.Add(14 * 24 * time.Hour)) {
		t.Fatalf("extend did not update user view before=%s after=%s", beforeTime, afterTime)
	}
	toLimit := postJSON(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(parsed.Cards[0].ID, 10)+"/extend", csrf, map[string]any{"days": 9})
	if toLimit.StatusCode != http.StatusOK {
		t.Fatalf("extend to limit status=%d body=%s", toLimit.StatusCode, readBody(t, toLimit))
	}
	overLimit := postJSON(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(parsed.Cards[0].ID, 10)+"/extend", csrf, map[string]any{"days": 1})
	overLimitBody := readBody(t, overLimit)
	if overLimit.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(overLimitBody, `"code":"card_duration_limit_exceeded"`) {
		t.Fatalf("extend over limit status=%d body=%s", overLimit.StatusCode, overLimitBody)
	}
	revoked := postJSON(t, client, server.URL+"/api/admin/cards/"+strconv.FormatInt(parsed.Cards[0].ID, 10)+"/revoke", csrf, map[string]any{})
	revokedBody := readBody(t, revoked)
	if revoked.StatusCode != http.StatusOK || !strings.Contains(revokedBody, `"status":"revoked"`) {
		t.Fatalf("revoke status=%d body=%s", revoked.StatusCode, revokedBody)
	}
	blocked := getRaw(t, server.Client(), server.URL+"/api/cards/"+parsed.Cards[0].Code+"/status")
	if blocked.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked user query status=%d body=%s", blocked.StatusCode, readBody(t, blocked))
	}
}

func postJSONNoCSRF(t *testing.T, client *http.Client, url string, payload any) *http.Response {
	t.Helper()
	return requestJSON(t, client, http.MethodPost, url, "", payload)
}

func getRawCSRF(t *testing.T, client *http.Client, url, csrf string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(csrfHeader, csrf)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func redeemCardForHTTPTest(t *testing.T, server *httptest.Server, accountID, cardID int64, durationDays int) {
	t.Helper()
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not registered")
	}
	repo := value.(*repository.Repository)
	allocation, err := repo.RedeemCard(context.Background(), cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AccountID != accountID {
		t.Fatalf("allocation account=%d want %d", allocation.AccountID, accountID)
	}
	if allocation.ValidUntil.Before(time.Now().UTC().Add(time.Duration(durationDays-1) * 24 * time.Hour)) {
		t.Fatalf("allocation valid_until too early: %s", allocation.ValidUntil)
	}
}

func countAuditEvents(t *testing.T, server *httptest.Server, action string) int {
	t.Helper()
	value, ok := testDatabases.Load(server.URL)
	if !ok {
		t.Fatal("test database not registered")
	}
	db := value.(*sql.DB)
	var count int
	if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM audit_events WHERE action=?", action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

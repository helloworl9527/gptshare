package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
)

func TestPublicCardActionPreviewRenewReplayAndAdminHistory(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	value, _ := testRepositories.Load(server.URL)
	repo := value.(*repository.Repository)
	codes := []string{"2345-6789-ABCD", "EFGH-JKMN-PQRS"}
	ids := []int64{}
	for _, code := range codes {
		id, err := repo.CreateCard(context.Background(), repository.CardSeed{CodeHash: cardsvc.HashCode(code), CodeSuffix: code[len(code)-4:], DurationDays: 90})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	payload := map[string]any{"action": "renew", "original_code": codes[0], "new_code": codes[1]}
	for _, path := range []string{"/api/cards/action-preview", "/api/cards/renew", "/api/cards/renew"} {
		response := postJSONNoCSRF(t, server.Client(), server.URL+path, payload)
		body := readBody(t, response)
		if response.StatusCode != http.StatusOK || !strings.Contains(body, `"duration_days":180`) {
			t.Fatalf("path=%s status=%d body=%s", path, response.StatusCode, body)
		}
		if response.Header.Get("Cache-Control") != "no-store" {
			t.Fatal("missing no-store")
		}
		for _, secret := range []string{"target_card_id", "source_card_id", "card_id", "group_id", "password", "account_id"} {
			if strings.Contains(body, secret) {
				t.Fatalf("public action leaks %s", secret)
			}
		}
		if path == "/api/cards/action-preview" {
			card, _ := repo.CardByID(context.Background(), ids[0])
			if card.DurationDays != 90 {
				t.Fatal("preview persisted")
			}
		}
	}
	client := authedAccountClient(t, server)
	csrf := getCSRF(t, client, server.URL)
	lookup := postJSON(t, client, server.URL+"/api/admin/cards/lookup", csrf, map[string]any{"code": codes[1]})
	body := readBody(t, lookup)
	if lookup.StatusCode != 200 || !strings.Contains(body, `"operations":[`) || !strings.Contains(body, `"status":"consumed"`) {
		t.Fatalf("admin lookup status=%d body=%s", lookup.StatusCode, body)
	}
}

func TestPublicCardActionUniformErrorsCaptchaAndOrigin(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	payload := map[string]any{"action": "renew", "original_code": "2345-6789-ABCD", "new_code": "EFGH-JKMN-PQRS"}
	for i := 0; i < 3; i++ {
		response := postJSONNoCSRF(t, server.Client(), server.URL+"/api/cards/renew", payload)
		body := readBody(t, response)
		if i < 2 {
			if response.StatusCode != 422 || !strings.Contains(body, "card_action_invalid") {
				t.Fatalf("attempt=%d status=%d body=%s", i, response.StatusCode, body)
			}
		} else {
			var result struct {
				Code    string `json:"code"`
				Captcha struct {
					ID       int64  `json:"id"`
					Question string `json:"question"`
				} `json:"captcha"`
			}
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 403 || result.Code != "captcha_required" || result.Captcha.ID == 0 || result.Captcha.Question == "" {
				t.Fatalf("challenge body=%s", body)
			}
		}
	}
	request, err := http.NewRequest(http.MethodPost, server.URL+"/api/cards/action-preview", strings.NewReader(`{"action":"renew"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Origin", "https://evil.example")
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 403 || !strings.Contains(readBody(t, response), "origin_not_allowed") {
		t.Fatal("cross-origin action accepted")
	}
}

func TestPublicCardActionsCompatibleRollbackSwitch(t *testing.T) {
	t.Setenv("PUBLIC_CARD_ACTIONS_ENABLED", "false")
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	defer server.Close()
	for _, path := range []string{"/api/cards/action-preview", "/api/cards/renew", "/api/redeem-different"} {
		response := postJSONNoCSRF(t, server.Client(), server.URL+path, map[string]any{})
		if response.StatusCode != 503 || !strings.Contains(readBody(t, response), "card_actions_unavailable") {
			t.Fatalf("rollback did not disable %s", path)
		}
	}
	response := getRaw(t, server.Client(), server.URL+"/")
	if response.StatusCode != 200 {
		t.Fatal("rollback disabled ordinary frontend")
	}
	response.Body.Close()
}

func TestCardActionClientIPTrustsOnlyLocalProxy(t *testing.T) {
	for _, test := range []struct{ peer, forwarded, want string }{
		{"127.0.0.1:1234", "203.0.113.8", "203.0.113.8"},
		{"[::1]:1234", "2001:db8::8", "2001:db8::8"},
		{"203.0.113.3:1234", "203.0.113.8", "203.0.113.3"},
		{"127.0.0.1:1234", "not-an-ip", "127.0.0.1"},
	} {
		request, _ := http.NewRequest(http.MethodPost, "https://test.invalid/api/cards/renew", nil)
		request.RemoteAddr = test.peer
		request.Header.Set("X-Real-IP", test.forwarded)
		if got := cardActionClientIP(request); got != test.want {
			t.Fatalf("peer=%s got=%s want=%s", test.peer, got, test.want)
		}
	}
}

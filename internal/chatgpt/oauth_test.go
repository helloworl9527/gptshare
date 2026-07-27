package chatgpt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestOAuthAuthorizationURLAndCodeExchange(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("client_id") != defaultClientID ||
			r.Form.Get("redirect_uri") != oauthRedirectURI ||
			r.Form.Get("code") != "authorization-code" ||
			r.Form.Get("code_verifier") != "verifier" {
			t.Fatalf("OAuth form=%v", r.Form)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  "access",
			"refresh_token": "refresh",
			"id_token":      "id",
		})
	}))
	defer server.Close()
	client := NewClient(Config{AuthorizeURL: "https://auth.example/authorize", TokenURL: server.URL})
	raw := client.BuildOAuthAuthorizationURL("state-value", "challenge-value")
	parsed, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	if query.Get("client_id") != defaultClientID ||
		query.Get("redirect_uri") != oauthRedirectURI ||
		query.Get("scope") != oauthScopes ||
		query.Get("state") != "state-value" ||
		query.Get("code_challenge") != "challenge-value" ||
		query.Get("code_challenge_method") != "S256" ||
		query.Get("codex_cli_simplified_flow") != "true" ||
		query.Get("originator") != oauthOriginator {
		t.Fatalf("authorization query=%v", query)
	}
	tokens, err := client.ExchangeOAuthCode(context.Background(), "authorization-code", "verifier")
	if err != nil || tokens.AccessToken != "access" || tokens.RefreshToken != "refresh" {
		t.Fatalf("tokens=%+v err=%v", tokens, err)
	}
}

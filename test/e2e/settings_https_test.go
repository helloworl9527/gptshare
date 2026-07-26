package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestSettingsHTTPSWriteOnlyCSRFAndNoChannelConnection(t *testing.T) {
	h := newHarness(t)
	defer h.close()
	secret := "e2e-channel-secret-must-never-return"

	response := h.request(t, http.MethodGet, "/api/settings", nil, "", "")
	assertStatus(t, response, http.StatusUnauthorized)
	csrf := login(t, h)
	response = h.request(t, http.MethodPut, "/api/settings", map[string]any{
		"poll_interval":    1800,
		"near_expiry_days": 5,
		"channels":         map[string]any{"telegram": map[string]any{"enabled": false, "secret": secret}},
	}, "wrong-csrf", "")
	assertStatus(t, response, http.StatusForbidden)

	response = h.request(t, http.MethodPut, "/api/settings", map[string]any{
		"poll_interval":    1800,
		"near_expiry_days": 5,
		"channels":         map[string]any{"telegram": map[string]any{"enabled": false, "secret": secret}},
	}, csrf, "")
	assertStatus(t, response, http.StatusOK)
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if bytes.Contains(body, []byte(secret)) || bytes.Contains(body, []byte("key_id")) || bytes.Contains(body, []byte("cipher")) {
		t.Fatalf("settings response leaked protected material: %s", body)
	}
	var result struct {
		PollInterval   int `json:"poll_interval"`
		NearExpiryDays int `json:"near_expiry_days"`
		Channels       map[string]struct {
			Enabled, Configured, Connected bool
		} `json:"channels"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatal(err)
	}
	telegram := result.Channels["telegram"]
	if result.PollInterval != 1800 || result.NearExpiryDays != 5 || !telegram.Configured || telegram.Enabled || telegram.Connected {
		t.Fatalf("settings=%+v", result)
	}

	response = h.request(t, http.MethodPut, "/api/settings", map[string]any{"channels": map[string]any{"wecom": map[string]any{"enabled": true}}}, csrf, "")
	assertStatus(t, response, http.StatusUnprocessableEntity)
	response = h.request(t, http.MethodDelete, "/api/settings/channels/telegram/secret", nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	response = h.request(t, http.MethodDelete, "/api/settings/channels/telegram/secret", nil, csrf, "")
	assertStatus(t, response, http.StatusNoContent)
	response = h.request(t, http.MethodGet, "/api/settings", nil, "", "")
	assertStatus(t, response, http.StatusOK)
	body, _ = io.ReadAll(response.Body)
	response.Body.Close()
	if bytes.Contains(body, []byte(secret)) || !bytes.Contains(body, []byte(`"configured":false`)) {
		t.Fatalf("clear response unsafe or incomplete: %s", body)
	}
	if bytes.Contains(h.logs.Bytes(), []byte(secret)) {
		t.Fatal("channel secret leaked to access log")
	}
}

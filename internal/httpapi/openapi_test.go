package httpapi

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestOpenAPIContract(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("..", "..", "docs", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(contents, &document); err != nil {
		t.Fatalf("parse OpenAPI: %v", err)
	}
	if document["openapi"] != "3.0.3" {
		t.Fatalf("openapi version=%v", document["openapi"])
	}
	paths := requireMap(t, document, "paths")
	health := requireMap(t, paths, "/healthz")
	get := requireMap(t, health, "get")
	responses := requireMap(t, get, "responses")
	for _, status := range []string{"200", "400", "500", "503"} {
		if _, ok := responses[status]; !ok {
			t.Errorf("/healthz missing response %s", status)
		}
	}
	for _, route := range []string{"/api/auth/csrf", "/api/auth/password", "/api/auth/totp", "/api/auth/logout", "/api/me"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing auth route %s", route)
		}
	}
	for _, route := range []string{"/api/accounts", "/api/accounts/{accountId}", "/api/accounts/import/token", "/api/accounts/{accountId}/reauthorize/token"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing account route %s", route)
		}
	}
	for _, route := range []string{"/api/accounts/import/device/start", "/api/accounts/{accountId}/reauthorize/device/start", "/api/accounts/import/device/{deviceSessionId}/poll"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing device route %s", route)
		}
	}
	for _, route := range []string{"/api/accounts/{accountId}/refresh", "/api/poll-runs/{pollRunId}"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing monitor route %s", route)
		}
	}
	for _, route := range []string{"/api/v1/monitor/accounts", "/api/v1/monitor/accounts/import-for-allocation", "/api/v1/monitor/accounts/batch-status", "/api/v1/monitor/accounts/{providerAccountId}/status"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing allocation monitor route %s", route)
		}
	}
	for _, route := range []string{"/api/settings", "/api/settings/channels/{channel}/secret"} {
		if _, ok := paths[route]; !ok {
			t.Errorf("missing settings route %s", route)
		}
	}
	components := requireMap(t, document, "components")
	securitySchemes := requireMap(t, components, "securitySchemes")
	bearer := requireMap(t, securitySchemes, "allocationServiceBearer")
	if bearer["type"] != "http" || bearer["scheme"] != "bearer" {
		t.Errorf("allocation service auth must use bearer scheme, got %#v", bearer)
	}
	schemas := requireMap(t, components, "schemas")
	for _, name := range []string{"HealthResponse", "ErrorResponse", "TokenImportRequest", "AllocationImportRequest", "AllocationBatchStatusRequest", "AllocationAccountStatus", "AllocationAccountListResponse", "AllocationAccountListItem", "AllocationBatchStatusResponse", "AllocationBatchStatusItem", "AllocationItemError", "CredentialSummary", "Account", "DeviceStartRequest", "DeviceStartResponse", "DevicePollResponse", "PollRun", "Settings", "SettingsUpdate", "ChannelState", "ChannelUpdate"} {
		if _, ok := schemas[name]; !ok {
			t.Errorf("missing schema %s", name)
		}
	}
	allocationImport := requireMap(t, schemas, "AllocationImportRequest")
	allocationImportProperties := requireMap(t, allocationImport, "properties")
	if requireMap(t, allocationImportProperties, "token")["writeOnly"] != true {
		t.Error("allocation import token must be writeOnly")
	}
	allocationStatus := requireMap(t, schemas, "AllocationAccountStatus")
	allocationStatusProperties := requireMap(t, allocationStatus, "properties")
	for _, required := range []string{"email", "auth_expiry", "subscription_expiry", "plan", "status"} {
		if _, ok := allocationStatusProperties[required]; !ok {
			t.Errorf("AllocationAccountStatus missing property %s", required)
		}
	}
	for _, forbidden := range []string{"token", "access_token", "refresh_token", "session_token", "enc_credentials", "credential_key_id"} {
		if _, ok := allocationStatusProperties[forbidden]; ok {
			t.Errorf("AllocationAccountStatus exposes forbidden property %s", forbidden)
		}
	}
	allocationListItem := requireMap(t, schemas, "AllocationAccountListItem")
	allocationListProperties := requireMap(t, allocationListItem, "properties")
	for _, required := range []string{"provider_account_id", "email", "auth_expiry", "plan", "status"} {
		if _, ok := allocationListProperties[required]; !ok {
			t.Errorf("AllocationAccountListItem missing property %s", required)
		}
	}
	for _, forbidden := range []string{"token", "access_token", "refresh_token", "session_token", "enc_credentials", "credential_key_id", "password", "totp"} {
		if _, ok := allocationListProperties[forbidden]; ok {
			t.Errorf("AllocationAccountListItem exposes forbidden property %s", forbidden)
		}
	}
	channelUpdate := requireMap(t, schemas, "ChannelUpdate")
	channelUpdateProperties := requireMap(t, channelUpdate, "properties")
	if requireMap(t, channelUpdateProperties, "secret")["writeOnly"] != true {
		t.Error("channel secret must be writeOnly")
	}
	settingsProperties := requireMap(t, requireMap(t, schemas, "Settings"), "properties")
	for _, forbidden := range []string{"secret", "key_id", "ciphertext", "internal"} {
		if _, ok := settingsProperties[forbidden]; ok {
			t.Errorf("Settings exposes forbidden property %s", forbidden)
		}
	}
	for _, name := range []string{"DeviceStartResponse", "DevicePollResponse"} {
		properties := requireMap(t, requireMap(t, schemas, name), "properties")
		for _, forbidden := range []string{"device_code", "device_auth_id", "access_token", "refresh_token", "code_verifier", "authorization_code"} {
			if _, ok := properties[forbidden]; ok {
				t.Errorf("%s exposes forbidden property %s", name, forbidden)
			}
		}
	}
	request := requireMap(t, schemas, "TokenImportRequest")
	properties := requireMap(t, request, "properties")
	for _, field := range []string{"access_token", "refresh_token", "session_token"} {
		property := requireMap(t, properties, field)
		if property["writeOnly"] != true {
			t.Errorf("%s must be writeOnly", field)
		}
	}
	accountSchema := requireMap(t, schemas, "Account")
	accountProperties := requireMap(t, accountSchema, "properties")
	for _, required := range []string{"last_check_state", "near_expiry", "polling_paused"} {
		if _, ok := accountProperties[required]; !ok {
			t.Errorf("Account missing monitoring property %s", required)
		}
	}
	email := requireMap(t, accountProperties, "email")
	if email["format"] != "email" || email["nullable"] != true || email["readOnly"] != true {
		t.Errorf("Account email must be readOnly nullable email, got %#v", email)
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "session_token", "enc_credentials", "credential_key_id"} {
		if _, ok := accountProperties[forbidden]; ok {
			t.Errorf("Account response exposes forbidden property %s", forbidden)
		}
	}
}

func requireMap(t *testing.T, parent map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := parent[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is missing or not an object", key)
	}
	return value
}

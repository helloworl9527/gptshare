package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"chatgpt-monitor/internal/account"
)

const testAllocationAPIKey = "allocation-api-key-32-bytes-fixture"

type allocationAccountStub struct {
	importedInput *account.TokenInput
	importResult  account.Account
	importErr     error
	accounts      []account.Account
	listErr       error
}

func (s *allocationAccountStub) ImportByToken(_ context.Context, input *account.TokenInput) (account.Account, error) {
	copied := *input
	s.importedInput = &copied
	if s.importErr != nil {
		return account.Account{}, s.importErr
	}
	return s.importResult, nil
}

func (s *allocationAccountStub) ReauthorizeByToken(context.Context, int64, *account.TokenInput) (account.Account, error) {
	return account.Account{}, errors.New("not implemented")
}
func (s *allocationAccountStub) Delete(context.Context, int64) error {
	return errors.New("not implemented")
}
func (s *allocationAccountStub) List(context.Context) ([]account.Account, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.accounts, nil
}
func (s *allocationAccountStub) Get(context.Context, int64) (account.Account, error) {
	return account.Account{}, errors.New("not implemented")
}
func (s *allocationAccountStub) StartDeviceImport(context.Context, string) (account.DeviceStart, error) {
	return account.DeviceStart{}, errors.New("not implemented")
}
func (s *allocationAccountStub) StartDeviceReauthorization(context.Context, int64) (account.DeviceStart, error) {
	return account.DeviceStart{}, errors.New("not implemented")
}
func (s *allocationAccountStub) PollDevice(context.Context, string) (account.DevicePoll, error) {
	return account.DevicePoll{}, errors.New("not implemented")
}

func TestAllocationAPIKeyAuthenticationMatrix(t *testing.T) {
	fixture := &allocationAccountStub{accounts: []account.Account{allocationAccount("acct-1", "alive")}}
	for _, tt := range []struct {
		name       string
		configKey  string
		header     string
		wantStatus int
	}{
		{"success", testAllocationAPIKey, "Bearer " + testAllocationAPIKey, http.StatusOK},
		{"missing", testAllocationAPIKey, "", http.StatusUnauthorized},
		{"wrong", testAllocationAPIKey, "Bearer wrong-allocation-api-key-32-bytes", http.StatusUnauthorized},
		{"empty config", "", "Bearer " + testAllocationAPIKey, http.StatusUnauthorized},
		{"default config", "change-me", "Bearer change-me", http.StatusUnauthorized},
		{"lowercase bearer accepted", testAllocationAPIKey, "bearer " + testAllocationAPIKey, http.StatusOK},
		{"malformed scheme rejected", testAllocationAPIKey, "Token " + testAllocationAPIKey, http.StatusUnauthorized},
	} {
		t.Run(tt.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/accounts/batch-status", strings.NewReader(`{"provider_account_ids":["acct-1"]}`))
			request.Header.Set("Content-Type", "application/json")
			if tt.header != "" {
				request.Header.Set("Authorization", tt.header)
			}
			router := NewRouter(healthStub{}, nil, fixture, Config{AllocationServiceAPIKey: tt.configKey}, slog.New(slog.NewJSONHandler(io.Discard, nil)))
			router.ServeHTTP(recorder, request)
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if tt.wantStatus == http.StatusUnauthorized && !strings.Contains(recorder.Body.String(), `"code":"unauthorized"`) {
				t.Fatalf("unauthorized response is not uniform: %s", recorder.Body.String())
			}
		})
	}
}

func TestAllocationImportForAllocation(t *testing.T) {
	expiry := time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC)
	service := &allocationAccountStub{importResult: allocationAccountWithExpiry("acct-imported", "alive", expiry)}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/accounts/import-for-allocation", strings.NewReader(`{"token":"sensitive-token","token_type":"access_token","label":"ops label"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testAllocationAPIKey)

	NewRouter(healthStub{}, nil, service, Config{AllocationServiceAPIKey: testAllocationAPIKey}, slog.New(slog.NewJSONHandler(io.Discard, nil))).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if service.importedInput == nil || service.importedInput.AccessToken != "sensitive-token" || service.importedInput.RefreshToken != "" || service.importedInput.SessionToken != "" || service.importedInput.Label != "ops label" {
		t.Fatalf("unexpected imported input: %#v", service.importedInput)
	}
	body := recorder.Body.String()
	for _, want := range []string{`"provider_account_id":"acct-imported"`, `"email":"acct-imported@example.test"`, `"status":"alive"`, `"plan":"plus"`, `"auth_expiry":"2026-08-23T00:00:00Z"`, `"subscription_expiry":"2026-08-23T00:00:00Z"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, "sensitive-token") {
		t.Fatal("response leaked imported token")
	}
}

func TestAllocationReadOnlyAccountListForPullSync(t *testing.T) {
	service := &allocationAccountStub{accounts: []account.Account{
		allocationAccount("acct-1", "alive"),
		allocationAccount("acct-2", "dead_normal"),
	}}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/accounts", nil)
	request.Header.Set("Authorization", "Bearer "+testAllocationAPIKey)

	NewRouter(healthStub{}, nil, service, Config{AllocationServiceAPIKey: testAllocationAPIKey}, slog.New(slog.NewJSONHandler(io.Discard, nil))).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"provider_account_id":"acct-1"`, `"email":"acct-1@example.test"`, `"auth_expiry":"2026-08-24T00:00:00Z"`, `"status":"dead_normal"`, `"plan":"plus"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("list response missing %s: %s", want, body)
		}
	}
	for _, forbidden := range []string{"access_token", "refresh_token", "session_token", "credential", "token"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("list response leaked %s: %s", forbidden, body)
		}
	}
}

func TestAllocationBatchAndSingleStatusContracts(t *testing.T) {
	service := &allocationAccountStub{accounts: []account.Account{
		allocationAccount("acct-1", "alive"),
		allocationAccount("acct-2", "dead_banned"),
	}}
	router := NewRouter(healthStub{}, nil, service, Config{AllocationServiceAPIKey: testAllocationAPIKey}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	batch := httptest.NewRecorder()
	batchRequest := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/accounts/batch-status", strings.NewReader(`{"provider_account_ids":["acct-1","missing","acct-2"]}`))
	batchRequest.Header.Set("Content-Type", "application/json")
	batchRequest.Header.Set("Authorization", "Bearer "+testAllocationAPIKey)
	router.ServeHTTP(batch, batchRequest)
	if batch.Code != http.StatusOK {
		t.Fatalf("batch status=%d body=%s", batch.Code, batch.Body.String())
	}
	batchBody := batch.Body.String()
	for _, want := range []string{`"provider_account_id":"acct-1"`, `"status":"alive"`, `"provider_account_id":"missing"`, `"code":"not_found"`, `"provider_account_id":"acct-2"`, `"status":"dead_banned"`} {
		if !strings.Contains(batchBody, want) {
			t.Fatalf("batch response missing %s: %s", want, batchBody)
		}
	}

	single := httptest.NewRecorder()
	singleRequest := httptest.NewRequest(http.MethodGet, "/api/v1/monitor/accounts/acct-2/status", nil)
	singleRequest.Header.Set("Authorization", "Bearer "+testAllocationAPIKey)
	router.ServeHTTP(single, singleRequest)
	if single.Code != http.StatusOK || !strings.Contains(single.Body.String(), `"status":"dead_banned"`) {
		t.Fatalf("single status=%d body=%s", single.Code, single.Body.String())
	}
}

func TestAllocationBatchStatusFiftyPlusAccountsUnderOneSecond(t *testing.T) {
	accounts := make([]account.Account, 75)
	ids := make([]string, 75)
	for i := range accounts {
		id := "acct-" + strings.Repeat("0", 3-lenInt(i)) + intString(i)
		accounts[i] = allocationAccount(id, "alive")
		ids[i] = `"` + id + `"`
	}
	service := &allocationAccountStub{accounts: accounts}
	body := `{"provider_account_ids":[` + strings.Join(ids, ",") + `]}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/accounts/batch-status", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+testAllocationAPIKey)
	recorder := httptest.NewRecorder()

	started := time.Now()
	NewRouter(healthStub{}, nil, service, Config{AllocationServiceAPIKey: testAllocationAPIKey}, slog.New(slog.NewJSONHandler(io.Discard, nil))).ServeHTTP(recorder, request)
	elapsed := time.Since(started)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if elapsed >= time.Second {
		t.Fatalf("batch status took %s", elapsed)
	}
}

func allocationAccount(providerID, status string) account.Account {
	return allocationAccountWithExpiry(providerID, status, time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC))
}

func allocationAccountWithExpiry(providerID, status string, expiry time.Time) account.Account {
	email := providerID + "@example.test"
	return account.Account{ProviderAccountID: providerID, Email: &email, Status: status, Plan: "plus", CurrentExpiry: &expiry, AuthExpiry: expiry}
}

func intString(value int) string {
	if value == 0 {
		return "0"
	}
	var out []byte
	for value > 0 {
		out = append([]byte{byte('0' + value%10)}, out...)
		value /= 10
	}
	return string(out)
}

func lenInt(value int) int {
	if value < 10 {
		return 1
	}
	if value < 100 {
		return 2
	}
	return 3
}

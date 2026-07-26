package server_test

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
	"chatgpt-monitor/internal/httpapi"
)

const allocationAPIKey = "allocation-server-contract-32-bytes-key"

type health struct{}

func (health) Health(context.Context) error { return nil }

type accountService struct {
	accounts []account.Account
}

func (s accountService) ImportByToken(context.Context, *account.TokenInput) (account.Account, error) {
	return account.Account{}, errors.New("not implemented")
}
func (s accountService) ReauthorizeByToken(context.Context, int64, *account.TokenInput) (account.Account, error) {
	return account.Account{}, errors.New("not implemented")
}
func (s accountService) Delete(context.Context, int64) error { return errors.New("not implemented") }
func (s accountService) List(context.Context) ([]account.Account, error) {
	return s.accounts, nil
}
func (s accountService) Get(context.Context, int64) (account.Account, error) {
	return account.Account{}, errors.New("not implemented")
}
func (s accountService) StartDeviceImport(context.Context, string) (account.DeviceStart, error) {
	return account.DeviceStart{}, errors.New("not implemented")
}
func (s accountService) StartDeviceReauthorization(context.Context, int64) (account.DeviceStart, error) {
	return account.DeviceStart{}, errors.New("not implemented")
}
func (s accountService) PollDevice(context.Context, string) (account.DevicePoll, error) {
	return account.DevicePoll{}, errors.New("not implemented")
}

func TestAllocationBatchStatusRouteContract(t *testing.T) {
	expiry := time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)
	router := httpapi.NewRouter(health{}, nil, accountService{accounts: []account.Account{{
		ProviderAccountID: "acct-contract", Status: "alive", Plan: "plus", CurrentExpiry: &expiry, AuthExpiry: expiry,
	}}}, httpapi.Config{AllocationServiceAPIKey: allocationAPIKey}, slog.New(slog.NewJSONHandler(io.Discard, nil)))

	request := httptest.NewRequest(http.MethodPost, "/api/v1/monitor/accounts/batch-status", strings.NewReader(`{"provider_account_ids":["acct-contract","missing"]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+allocationAPIKey)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, want := range []string{`"provider_account_id":"acct-contract"`, `"status":"alive"`, `"provider_account_id":"missing"`, `"code":"not_found"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("response missing %s: %s", want, body)
		}
	}
}

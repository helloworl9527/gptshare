package monitorclient

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"allocation-service/monitorfacade"
)

func TestStatusMatrixFailSafe(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		body       string
		wantStatus string
		wantErr    bool
		wantFault  monitorfacade.FaultKind
	}{
		{name: "alive", statusCode: http.StatusOK, body: `{"provider_account_id":"acct-1","status":"alive"}`, wantStatus: "alive"},
		{name: "unknown", statusCode: http.StatusOK, body: `{"provider_account_id":"acct-1","monitor_status":"unknown"}`, wantStatus: "unknown"},
		{name: "dead_normal", statusCode: http.StatusOK, body: `{"provider_account_id":"acct-1","status":"dead_normal"}`, wantStatus: "dead_normal"},
		{name: "dead_banned", statusCode: http.StatusOK, body: `{"provider_account_id":"acct-1","status":"dead_banned"}`, wantStatus: "dead_banned"},
		{name: "not_found_http", statusCode: http.StatusNotFound, body: `{"code":"not_found"}`, wantStatus: "not_found"},
		{name: "5xx", statusCode: http.StatusBadGateway, body: `{"code":"bad_gateway"}`, wantErr: true, wantFault: monitorfacade.FaultUnavailable},
		{name: "invalid_json", statusCode: http.StatusOK, body: `{`, wantErr: true, wantFault: monitorfacade.FaultContractChanged},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer allocation-monitor-key" {
					t.Fatalf("missing auth header")
				}
				w.WriteHeader(tt.statusCode)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(0), WithCircuitBreaker(10, time.Second))
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Status(context.Background(), "acct-1")
			if tt.wantErr {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err=%v want ErrUnavailable", err)
				}
				if kind, ok := monitorfacade.FaultKindOf(err); !ok || kind != tt.wantFault {
					t.Fatalf("fault=%s,%v want %s", kind, ok, tt.wantFault)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.MonitorStatus != tt.wantStatus {
				t.Fatalf("status=%q want %q", got.MonitorStatus, tt.wantStatus)
			}
		})
	}
}

func TestStatusTimeoutIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = w.Write([]byte(`{"provider_account_id":"acct-1","status":"alive"}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", &http.Client{Timeout: 5 * time.Millisecond}, WithRetries(0), WithCircuitBreaker(10, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background(), "acct-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err=%v want ErrUnavailable", err)
	} else if kind, ok := monitorfacade.FaultKindOf(err); !ok || kind != monitorfacade.FaultTimeout {
		t.Fatalf("fault=%s,%v want timeout", kind, ok)
	}
}

func TestImportForAllocationParsesPhaseOneAccountDetails(t *testing.T) {
	expiry := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer allocation-monitor-key" {
			t.Fatalf("missing auth header")
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if req["token"] != "import-token" || req["token_type"] != "session_token" {
			t.Fatalf("bad import request: %#v", req)
		}
		_, _ = w.Write([]byte(`{"provider_account_id":"acct-import","email":"owner@example.test","status":"alive","plan":"plus","auth_expiry":"` + expiry + `"}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(0), WithCircuitBreaker(10, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ImportForAllocation(context.Background(), ImportRequest{Token: "import-token", TokenType: "session_token"})
	if err != nil {
		t.Fatal(err)
	}
	if got.MonitorAccountID != "acct-import" || got.Email != "owner@example.test" || got.MonitorStatus != "alive" || got.Plan != "plus" || got.AccountExpiry.IsZero() {
		t.Fatalf("bad import result: %+v", got)
	}
}

func TestListAccountsParsesReadOnlyPullSyncResponse(t *testing.T) {
	expiry := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/monitor/accounts" {
			t.Fatalf("bad request %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer allocation-monitor-key" {
			t.Fatalf("missing auth header")
		}
		_, _ = w.Write([]byte(`{"accounts":[{"provider_account_id":"acct-pull","email":"pull@example.test","status":"alive","plan":"plus","auth_expiry":"` + expiry + `"}]}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(0), WithCircuitBreaker(10, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("items=%d want 1", len(got))
	}
	if got[0].MonitorAccountID != "acct-pull" || got[0].Email != "pull@example.test" || got[0].MonitorStatus != "alive" || got[0].Plan != "plus" || got[0].AccountExpiry.IsZero() {
		t.Fatalf("bad pull result: %+v", got[0])
	}
}

func TestListAccountsKeepsInvalidItemForPerAccountSyncHandling(t *testing.T) {
	past := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)
	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"accounts":[
			{"provider_account_id":"expired","status":"dead_normal","auth_expiry":"` + past + `"},
			{"provider_account_id":"invalid","status":"alive","auth_expiry":"not-a-time"},
			{"provider_account_id":"after-invalid","status":"alive","auth_expiry":"` + future + `"}
		]}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(0), WithCircuitBreaker(10, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].MonitorStatus != "dead_normal" || got[0].AccountExpiry.IsZero() || got[1].SyncErrorCode != "missing_account_expiry" || got[2].MonitorAccountID != "after-invalid" {
		t.Fatalf("unexpected pull items: %+v", got)
	}
}

func TestBatchStatusItemMatrix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ProviderAccountIDs []string `json:"provider_account_ids"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		if strings.Join(req.ProviderAccountIDs, ",") != "alive,unknown,normal,banned,missing" {
			t.Fatalf("bad request ids: %#v", req.ProviderAccountIDs)
		}
		_, _ = w.Write([]byte(`{"items":[
			{"provider_account_id":"alive","status":"alive"},
			{"provider_account_id":"unknown","monitor_status":"unknown"},
			{"provider_account_id":"normal","status":"dead_normal"},
			{"provider_account_id":"banned","status":"dead_banned"},
			{"provider_account_id":"missing","error":{"code":"not_found","message":"missing"}}
		]}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(0), WithCircuitBreaker(10, time.Second))
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.BatchStatus(context.Background(), []string{"alive", "unknown", "normal", "banned", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"alive":   "alive",
		"unknown": "unknown",
		"normal":  "dead_normal",
		"banned":  "dead_banned",
		"missing": "not_found",
	}
	for id, status := range want {
		if got[id].MonitorStatus != status {
			t.Fatalf("%s status=%q want %q", id, got[id].MonitorStatus, status)
		}
	}
}

func TestRetryAndCircuitBreaker(t *testing.T) {
	var calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) <= 2 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"provider_account_id":"acct-1","status":"alive"}`))
	}))
	defer server.Close()
	client, err := NewWithOptions(server.URL, "allocation-monitor-key", server.Client(), WithRetries(1), WithCircuitBreaker(1, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Status(context.Background(), "acct-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("first err=%v want ErrUnavailable", err)
	}
	before := atomic.LoadInt32(&calls)
	if _, err := client.Status(context.Background(), "acct-1"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("circuit err=%v want ErrUnavailable", err)
	}
	if after := atomic.LoadInt32(&calls); after != before {
		t.Fatalf("circuit did not short-circuit: before=%d after=%d", before, after)
	}
}

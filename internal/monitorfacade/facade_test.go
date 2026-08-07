package monitorfacade

import (
	"context"
	"errors"
	"testing"
	"time"

	allocationfacade "allocation-service/monitorfacade"
	"chatgpt-monitor/internal/account"
)

type fakeSource struct {
	items []account.Account
	err   error
}

func (f fakeSource) ImportByToken(context.Context, *account.TokenInput) (account.Account, error) {
	if f.err != nil {
		return account.Account{}, f.err
	}
	if len(f.items) == 0 {
		return account.Account{}, errors.New("no fixture")
	}
	return f.items[0], nil
}

func (f fakeSource) List(context.Context) ([]account.Account, error) { return f.items, f.err }

func TestListStatusAndBatchStayInProcess(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour)
	facade, err := New(fakeSource{items: []account.Account{{ProviderAccountID: "provider-1", AuthExpiry: expiry, Status: "alive", Plan: "plus"}}})
	if err != nil {
		t.Fatal(err)
	}
	items, err := facade.ListAccounts(context.Background())
	if err != nil || len(items) != 1 || items[0].MonitorAccountID != "provider-1" {
		t.Fatalf("ListAccounts() = %#v, %v", items, err)
	}
	status, err := facade.Status(context.Background(), "missing")
	if err != nil || status.MonitorStatus != "not_found" {
		t.Fatalf("Status() = %#v, %v", status, err)
	}
	statuses, err := facade.BatchStatus(context.Background(), []string{"provider-1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if statuses["provider-1"].MonitorStatus != "alive" || statuses["missing"].MonitorStatus != "not_found" {
		t.Fatalf("unexpected statuses: %#v", statuses)
	}
	if !facade.Available(context.Background()) {
		t.Fatal("in-process facade must be available when its source can be read")
	}
}

func TestTypedFaultMatrix(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want allocationfacade.FaultKind
	}{
		{name: "timeout", err: context.DeadlineExceeded, want: allocationfacade.FaultTimeout},
		{name: "unavailable", err: errors.New("store unavailable"), want: allocationfacade.FaultUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			facade, _ := New(fakeSource{err: test.err})
			_, err := facade.ListAccounts(context.Background())
			kind, ok := allocationfacade.FaultKindOf(err)
			if !ok || kind != test.want {
				t.Fatalf("fault = %v, %v; want %s", kind, ok, test.want)
			}
		})
	}
}

func TestListContractChangeIsReportedPerItem(t *testing.T) {
	expiry := time.Now().UTC().Add(time.Hour)
	facade, _ := New(fakeSource{items: []account.Account{
		{ProviderAccountID: "provider-1"},
		{ProviderAccountID: "provider-2", AuthExpiry: expiry, Status: "alive"},
	}})
	items, err := facade.ListAccounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].SyncErrorCode != "missing_account_expiry" || items[1].MonitorAccountID != "provider-2" {
		t.Fatalf("unexpected list result: %+v", items)
	}
}

package account

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"allocation-service/internal/models"
	"allocation-service/internal/repository"
	"allocation-service/monitorfacade"
)

type pullRepository struct {
	seeds []repository.SyncedAccount
}

func (r *pullRepository) CreateAccount(context.Context, repository.AccountSeed) (int64, error) {
	return 0, errors.New("unexpected CreateAccount call")
}

func (r *pullRepository) UpsertSyncedAccount(_ context.Context, seed repository.SyncedAccount) (models.Account, bool, error) {
	if seed.MonitorAccountID == "write-failure" {
		return models.Account{}, false, errors.New("injected write failure")
	}
	r.seeds = append(r.seeds, seed)
	return models.Account{ID: int64(len(r.seeds)), MonitorAccountID: seed.MonitorAccountID, MonitorStatus: seed.MonitorStatus}, true, nil
}

func (r *pullRepository) UpdateAccount(context.Context, int64, repository.AccountUpdate) (models.Account, error) {
	return models.Account{}, errors.New("unexpected UpdateAccount call")
}

func (r *pullRepository) UpdateAccountMonitorStatus(context.Context, int64, string, string) (models.Account, error) {
	return models.Account{}, errors.New("unexpected UpdateAccountMonitorStatus call")
}

func (r *pullRepository) DeleteAccount(context.Context, int64) error {
	return errors.New("unexpected DeleteAccount call")
}
func (r *pullRepository) Account(context.Context, int64) (models.Account, error) {
	return models.Account{}, errors.New("unexpected Account call")
}
func (r *pullRepository) ListAccounts(context.Context) ([]models.Account, error) { return nil, nil }
func (r *pullRepository) AccountCapacitySettings(context.Context) (repository.AccountCapacitySettings, error) {
	return repository.AccountCapacitySettings{}, nil
}
func (r *pullRepository) SetDefaultAccountCapacity(context.Context, int) (repository.AccountCapacitySettings, error) {
	return repository.AccountCapacitySettings{}, nil
}
func (r *pullRepository) ApplyDefaultAccountCapacity(context.Context) (repository.ApplyDefaultCapacityResult, error) {
	return repository.ApplyDefaultCapacityResult{}, nil
}
func (r *pullRepository) CreateMonitorSyncRun(context.Context, int) (int64, error) { return 0, nil }
func (r *pullRepository) FinishMonitorSyncRun(context.Context, int64, string, int, int, string) error {
	return nil
}
func (r *pullRepository) LatestMonitorSyncRun(context.Context) (repository.MonitorSyncRun, error) {
	return repository.MonitorSyncRun{}, nil
}

type pullMonitor struct {
	items []monitorfacade.StatusResult
}

func (m pullMonitor) ImportForAllocation(context.Context, monitorfacade.ImportRequest) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, errors.New("unexpected ImportForAllocation call")
}
func (m pullMonitor) ListAccounts(context.Context) ([]monitorfacade.StatusResult, error) {
	return m.items, nil
}
func (m pullMonitor) Status(context.Context, string) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, errors.New("unexpected Status call")
}
func (m pullMonitor) BatchStatus(context.Context, []string) (map[string]monitorfacade.StatusResult, error) {
	return nil, errors.New("unexpected BatchStatus call")
}

func TestPullFromMonitorContinuesAfterContradictoryItem(t *testing.T) {
	now := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	monitor := pullMonitor{items: []monitorfacade.StatusResult{
		{MonitorAccountID: "alive", MonitorStatus: "alive", AccountExpiry: now.Add(time.Hour)},
		{MonitorAccountID: "expired", MonitorStatus: "dead_normal", AccountExpiry: now.Add(-24 * time.Hour)},
		{MonitorAccountID: "conflict", MonitorStatus: "alive", AccountExpiry: now.Add(-time.Hour)},
		{MonitorAccountID: "banned", MonitorStatus: "dead_banned", AccountExpiry: now.Add(-48 * time.Hour)},
		{MonitorAccountID: "write-failure", MonitorStatus: "alive", AccountExpiry: now.Add(time.Hour)},
		{MonitorAccountID: "after-conflict", MonitorStatus: "alive", AccountExpiry: now.Add(2 * time.Hour)},
		{MonitorAccountID: "after-conflict", MonitorStatus: "alive", AccountExpiry: now.Add(2 * time.Hour)},
	}}
	repo := &pullRepository{}
	service := NewService(repo, monitor)
	service.SetNow(func() time.Time { return now })

	result, err := service.PullFromMonitor(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 7 || result.Created != 4 || result.Updated != 0 || result.Skipped != 1 || result.Failed != 2 {
		t.Fatalf("unexpected result: %+v", result)
	}
	wantIDs := []string{"alive", "expired", "banned", "after-conflict"}
	gotIDs := make([]string, 0, len(repo.seeds))
	for _, seed := range repo.seeds {
		gotIDs = append(gotIDs, seed.MonitorAccountID)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("upsert order = %#v, want %#v", gotIDs, wantIDs)
	}
	if len(result.Errors) != 3 || result.Errors[0].MonitorAccountID != "conflict" || result.Errors[0].Code != "alive_expiry_conflict" || result.Errors[1].MonitorAccountID != "write-failure" || result.Errors[1].Code != "account_sync_failed" || result.Errors[2].Code != "duplicate_monitor_account" {
		t.Fatalf("unexpected item errors: %+v", result.Errors)
	}
}

package module

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	accountsvc "allocation-service/internal/account"
	"allocation-service/internal/card"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
	"allocation-service/monitorfacade"
	"allocation-service/platform/supervisor"
	"github.com/gin-gonic/gin"
)

func TestMigrateIsIdempotentAndReachesLatestAllocationSchema(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "allocation.db")
	for attempt := 0; attempt < 2; attempt++ {
		if err := Migrate(context.Background(), dbPath); err != nil {
			t.Fatalf("migration attempt %d: %v", attempt+1, err)
		}
	}

	database, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var version, count int
	if err := database.DB().QueryRow("SELECT max(version), count(*) FROM schema_migrations").Scan(&version, &count); err != nil {
		t.Fatal(err)
	}
	latest, err := store.LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != latest || count != latest {
		t.Fatalf("allocation migration ledger = version %d count %d, want version %d count %d", version, count, latest, latest)
	}
}

func TestMergedRegisterPublicRoutesIncludesAPIsButNotStandaloneUI(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), filepath.Join(dir, "allocation.db"), map[string][]byte{"allocation-2026": []byte(strings.Repeat("a", 32))}, "allocation-2026", emptyMonitor{}, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	router := gin.New()
	opened.RegisterPublicRoutes(router)
	want := map[string]bool{
		http.MethodPost + " /api/redeem":      false,
		http.MethodPost + " /api/cards/query": false,
	}
	for _, route := range router.Routes() {
		if route.Method == http.MethodGet && route.Path == "/" {
			t.Fatal("merged allocation module must not own the unified public page")
		}
		key := route.Method + " " + route.Path
		if _, ok := want[key]; ok {
			want[key] = true
		}
	}
	for route, mounted := range want {
		if !mounted {
			t.Fatalf("%s was not mounted", route)
		}
	}
}

type emptyMonitor struct{}

func (emptyMonitor) ImportForAllocation(context.Context, monitorfacade.ImportRequest) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
}
func (emptyMonitor) ListAccounts(context.Context) ([]monitorfacade.StatusResult, error) {
	return nil, nil
}
func (emptyMonitor) Status(context.Context, string) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultNotFound)
}
func (emptyMonitor) BatchStatus(context.Context, []string) (map[string]monitorfacade.StatusResult, error) {
	return map[string]monitorfacade.StatusResult{}, nil
}
func (emptyMonitor) Available(context.Context) bool { return true }

var _ monitorfacade.Client = emptyMonitor{}

func TestMergedModuleUsesFacadeWithoutHTTPOrAPIKey(t *testing.T) {
	monitor := &memoryMonitor{available: true, list: []monitorfacade.StatusResult{{MonitorAccountID: "phase-one-1", MonitorStatus: "alive", Email: "first@example.test", AccountExpiry: time.Now().UTC().Add(48 * time.Hour), Plan: "plus"}}}
	opened := openModule(t, monitor)
	defer opened.Close()

	oldTransport := http.DefaultTransport
	trap := &networkTrap{}
	http.DefaultTransport = trap
	defer func() { http.DefaultTransport = oldTransport }()

	first, err := opened.accounts.PullFromMonitor(context.Background())
	if err != nil || first.Created != 1 || len(first.Accounts) != 1 || first.Accounts[0].Status != "pending_credentials" {
		t.Fatalf("first pull = %+v, %v", first, err)
	}
	item := first.Accounts[0]
	updated, err := opened.accounts.Update(context.Background(), item.ID, accountsvc.UpdateInput{
		DisplayUsername: item.DisplayUsername, DisplayPassword: "retained-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXP",
		AccountExpiry: item.AccountExpiry, MaxConcurrentUsers: item.MaxConcurrentUsers, Status: "pending_credentials", MonitorStatus: "alive", MonitorAccountID: item.MonitorAccountID,
	})
	if err != nil || updated.Status != "available" {
		t.Fatalf("credential completion = %+v, %v", updated, err)
	}
	beforePassword, beforeTOTP := encryptedCredentials(t, opened, item.ID)

	monitor.setList([]monitorfacade.StatusResult{{MonitorAccountID: "phase-one-1", MonitorStatus: "unknown", Email: "updated@example.test", AccountExpiry: time.Now().UTC().Add(72 * time.Hour), Plan: "plus"}})
	second, err := opened.accounts.PullFromMonitor(context.Background())
	if err != nil || second.Created != 0 || second.Updated != 1 || second.Accounts[0].Status != "available" {
		t.Fatalf("second pull = %+v, %v", second, err)
	}
	afterPassword, afterTOTP := encryptedCredentials(t, opened, item.ID)
	if !reflect.DeepEqual(beforePassword, afterPassword) || !reflect.DeepEqual(beforeTOTP, afterTOTP) {
		t.Fatal("second pull overwrote completed credentials")
	}
	if trap.calls != 0 {
		t.Fatalf("merged module made %d HTTP request(s)", trap.calls)
	}
	source := readModuleSource(t)
	legacyAPIKeyName := "ALLOCATION_SERVICE_" + "API_KEY"
	if strings.Contains(source, "internal/monitorclient") || strings.Contains(source, "http://") || strings.Contains(source, legacyAPIKeyName) {
		t.Fatal("merged module assembly depends on HTTP monitor configuration")
	}
}

func TestFacadeTypedFaultInjectionPreservesPullAndBatchBranches(t *testing.T) {
	for _, kind := range []monitorfacade.FaultKind{monitorfacade.FaultUnavailable, monitorfacade.FaultTimeout, monitorfacade.FaultContractChanged} {
		t.Run(string(kind), func(t *testing.T) {
			monitor := &memoryMonitor{listErr: monitorfacade.NewFault(kind)}
			opened := openModule(t, monitor)
			defer opened.Close()
			_, err := opened.accounts.PullFromMonitor(context.Background())
			got, ok := monitorfacade.FaultKindOf(err)
			if !ok || got != kind {
				t.Fatalf("pull fault = %v, %v; want %s", got, ok, kind)
			}
		})
	}

	monitor := &memoryMonitor{available: true, list: statusFixtures("alive", "unknown", "normal", "banned", "missing")}
	opened := openModule(t, monitor)
	defer opened.Close()
	if _, err := opened.accounts.PullFromMonitor(context.Background()); err != nil {
		t.Fatal(err)
	}
	monitor.batch = map[string]monitorfacade.StatusResult{
		"alive":   {MonitorAccountID: "alive", MonitorStatus: "alive"},
		"unknown": {MonitorAccountID: "unknown", MonitorStatus: "unknown"},
		"normal":  {MonitorAccountID: "normal", MonitorStatus: "dead_normal"},
		"banned":  {MonitorAccountID: "banned", MonitorStatus: "dead_banned"},
		"missing": {MonitorAccountID: "missing", MonitorStatus: "not_found"},
	}
	result, err := opened.accounts.SyncAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]string, len(result.Accounts))
	for _, item := range result.Accounts {
		got[item.MonitorAccountID] = item.MonitorStatus
	}
	want := map[string]string{"alive": "alive", "unknown": "unknown", "normal": "dead_normal", "banned": "dead_banned", "missing": "not_found"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("status matrix = %#v, want %#v", got, want)
	}
	for _, kind := range []monitorfacade.FaultKind{monitorfacade.FaultUnavailable, monitorfacade.FaultTimeout, monitorfacade.FaultContractChanged} {
		monitor.setBatchError(monitorfacade.NewFault(kind))
		degraded, err := opened.accounts.SyncAll(context.Background())
		if err != nil || len(degraded.Warnings) != 1 || degraded.Warnings[0] != "phase_one_monitor_unavailable" {
			t.Fatalf("%s degradation = %+v, %v", kind, degraded, err)
		}
		for _, item := range degraded.Accounts {
			if item.MonitorStatus != "unknown_monitor" {
				t.Fatalf("%s status = %q, want unknown_monitor", kind, item.MonitorStatus)
			}
		}
	}
}

func TestRedeemExcludesDeadBannedEvenWhenFacadeUnavailable(t *testing.T) {
	monitor := &memoryMonitor{available: true}
	opened := openModule(t, monitor)
	defer opened.Close()
	now := time.Now().UTC()
	opened.repo.SetNow(func() time.Time { return now })
	bannedID, err := opened.repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "banned", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(8 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "dead_banned", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	aliveID, err := opened.repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "alive", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	createCard(t, opened, "2345-6789-ABCD")
	online, err := opened.allocator.Redeem(context.Background(), "2345-6789-ABCD")
	if err != nil || online.Account.ID != aliveID || online.Account.ID == bannedID {
		t.Fatalf("online redeem = %+v, %v", online, err)
	}
	monitor.setAvailable(false)
	createCard(t, opened, "3456-789A-BCDE")
	offline, err := opened.allocator.Redeem(context.Background(), "3456-789A-BCDE")
	if err != nil || len(offline.Warnings) != 1 || offline.Warnings[0] != "monitor_unavailable" || offline.Account.ID != aliveID || offline.Account.ID == bannedID {
		t.Fatalf("offline redeem = %+v, %v", offline, err)
	}
}

func TestManagedFacadeWorkerConcurrentWithRedeemAndReplacement(t *testing.T) {
	monitor := &memoryMonitor{available: true, list: statusFixtures("sync-account"), batch: map[string]monitorfacade.StatusResult{"sync-account": {MonitorAccountID: "sync-account", MonitorStatus: "alive"}}}
	opened := openModule(t, monitor)
	defer opened.Close()
	now := time.Now().UTC()
	opened.repo.SetNow(func() time.Time { return now })
	if _, err := opened.repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "redeem-account", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive", Status: "available",
	}); err != nil {
		t.Fatal(err)
	}
	createCard(t, opened, "4567-89AB-CDEF")

	ctx, cancel := context.WithCancel(context.Background())
	runner := supervisor.New(slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)), supervisor.Config{InitialBackoff: time.Millisecond})
	if err := runner.GoManaged(ctx, "facade-sync", "allocation", func(taskCtx context.Context) error {
		return opened.RunFacadeSync(taskCtx, time.Millisecond)
	}); err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		if _, err := opened.allocator.Redeem(context.Background(), "4567-89AB-CDEF"); err != nil {
			errCh <- err
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 5; i++ {
			if _, err := opened.replacements.RunOnce(context.Background()); err != nil {
				errCh <- err
				return
			}
		}
	}()
	workers.Wait()
	time.Sleep(10 * time.Millisecond)
	cancel()
	runner.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

func TestBatchStatusDeadBannedDrivesReplacement(t *testing.T) {
	monitor := &memoryMonitor{available: true, batch: map[string]monitorfacade.StatusResult{
		"old-monitor":   {MonitorAccountID: "old-monitor", MonitorStatus: "dead_banned"},
		"spare-monitor": {MonitorAccountID: "spare-monitor", MonitorStatus: "alive"},
	}}
	opened := openModule(t, monitor)
	defer opened.Close()
	now := time.Now().UTC()
	opened.repo.SetNow(func() time.Time { return now })
	oldID, err := opened.repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "old", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorAccountID: "old-monitor", MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	spareID, err := opened.repo.CreateAccount(context.Background(), repository.AccountSeed{
		DisplayUsername: "spare", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(35 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorAccountID: "spare-monitor", MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	createCardWithDuration(t, opened, "5678-9ABC-DEFG", 30)
	primary, err := opened.allocator.Redeem(context.Background(), "5678-9ABC-DEFG")
	if err != nil || primary.Account.ID != oldID {
		t.Fatalf("initial allocation = %+v, %v; want old %d", primary, err, oldID)
	}
	if _, err := opened.accounts.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	banned, err := opened.repo.Account(context.Background(), oldID)
	if err != nil || banned.MonitorStatus != "dead_banned" || banned.Status != "banned" {
		t.Fatalf("synced banned account = %+v, %v", banned, err)
	}
	monitor.batch["old-monitor"] = monitorfacade.StatusResult{MonitorAccountID: "old-monitor", MonitorStatus: "alive"}
	if _, err := opened.accounts.SyncAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	stillBanned, err := opened.repo.Account(context.Background(), oldID)
	if err != nil || stillBanned.MonitorStatus != "alive" || stillBanned.Status != "banned" {
		t.Fatalf("alive observation automatically restored banned account = %+v, %v", stillBanned, err)
	}
	opened.replacements.SetNow(func() time.Time { return now.Add(time.Hour) })
	run, err := opened.replacements.RunOnce(context.Background())
	if err != nil || len(run.Replaced) != 1 || run.Replaced[0].NewAccountID != spareID || run.Replaced[0].Reason != "banned" {
		t.Fatalf("replacement = %+v, %v; want spare %d", run, err, spareID)
	}
}

type memoryMonitor struct {
	mu        sync.RWMutex
	list      []monitorfacade.StatusResult
	listErr   error
	batch     map[string]monitorfacade.StatusResult
	batchErr  error
	available bool
}

func (m *memoryMonitor) ImportForAllocation(context.Context, monitorfacade.ImportRequest) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, monitorfacade.NewFault(monitorfacade.FaultUnavailable)
}
func (m *memoryMonitor) ListAccounts(context.Context) ([]monitorfacade.StatusResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]monitorfacade.StatusResult(nil), m.list...), m.listErr
}
func (m *memoryMonitor) Status(_ context.Context, id string) (monitorfacade.ImportResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, item := range m.list {
		if item.MonitorAccountID == id {
			return monitorfacade.ImportResult(item), nil
		}
	}
	return monitorfacade.ImportResult{MonitorAccountID: id, MonitorStatus: "not_found"}, nil
}
func (m *memoryMonitor) BatchStatus(context.Context, []string) (map[string]monitorfacade.StatusResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make(map[string]monitorfacade.StatusResult, len(m.batch))
	for id, item := range m.batch {
		result[id] = item
	}
	return result, m.batchErr
}
func (m *memoryMonitor) Available(context.Context) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.available
}
func (m *memoryMonitor) setList(items []monitorfacade.StatusResult) {
	m.mu.Lock()
	m.list = append([]monitorfacade.StatusResult(nil), items...)
	m.mu.Unlock()
}
func (m *memoryMonitor) setAvailable(value bool) {
	m.mu.Lock()
	m.available = value
	m.mu.Unlock()
}
func (m *memoryMonitor) setBatchError(err error) {
	m.mu.Lock()
	m.batchErr = err
	m.mu.Unlock()
}

type networkTrap struct{ calls int }

func (n *networkTrap) RoundTrip(*http.Request) (*http.Response, error) {
	n.calls++
	return nil, errors.New("unexpected monitor network request")
}

func openModule(t *testing.T, monitor monitorfacade.Client) *Module {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	opened, err := Open(context.Background(), filepath.Join(dir, "allocation.db"), map[string][]byte{"allocation-2026": []byte(strings.Repeat("a", 32))}, "allocation-2026", monitor, slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return opened
}

func encryptedCredentials(t *testing.T, opened *Module, id int64) ([]byte, []byte) {
	t.Helper()
	var password, totp []byte
	if err := opened.store.DB().QueryRow("SELECT display_password_secret,display_2fa_secret FROM chatgpt_accounts WHERE id=?", id).Scan(&password, &totp); err != nil {
		t.Fatal(err)
	}
	return password, totp
}

func statusFixtures(ids ...string) []monitorfacade.StatusResult {
	items := make([]monitorfacade.StatusResult, 0, len(ids))
	for _, id := range ids {
		items = append(items, monitorfacade.StatusResult{MonitorAccountID: id, MonitorStatus: "alive", Email: id + "@example.test", AccountExpiry: time.Now().UTC().Add(48 * time.Hour), Plan: "plus"})
	}
	return items
}

func createCard(t *testing.T, opened *Module, code string) {
	createCardWithDuration(t, opened, code, 7)
}

func createCardWithDuration(t *testing.T, opened *Module, code string, duration int) {
	t.Helper()
	_, err := opened.repo.CreateCard(context.Background(), repository.CardSeed{CodeHash: card.HashCode(code), CodeSuffix: code[len(code)-4:], DurationDays: duration})
	if err != nil {
		t.Fatal(err)
	}
}

func readModuleSource(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile("module.go")
	if err != nil {
		t.Fatal(err)
	}
	return string(contents)
}

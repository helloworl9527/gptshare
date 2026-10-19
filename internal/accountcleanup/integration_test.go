package accountcleanup

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"allocation-service/accountsync"
	allocationmodule "allocation-service/module"
	"allocation-service/monitorfacade"
)

type emptyFacade struct{}

func (emptyFacade) ImportForAllocation(context.Context, monitorfacade.ImportRequest) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, monitorfacade.ErrUnavailable
}
func (emptyFacade) ListAccounts(context.Context) ([]monitorfacade.StatusResult, error) {
	return nil, nil
}
func (emptyFacade) Status(context.Context, string) (monitorfacade.ImportResult, error) {
	return monitorfacade.ImportResult{}, monitorfacade.ErrUnavailable
}
func (emptyFacade) BatchStatus(context.Context, []string) (map[string]monitorfacade.StatusResult, error) {
	return nil, nil
}
func (emptyFacade) Available(context.Context) bool { return false }

func TestCleanupAcrossRealMonitorAndAllocationDatabases(t *testing.T) {
	s, monitorDB, now, _ := setup(t)
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "allocation.db")
	module, err := allocationmodule.Open(ctx, dbPath, map[string][]byte{"test": []byte(strings.Repeat("a", 32))}, "test", emptyFacade{}, s.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = module.Close() })
	s.guard = module
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	stamp := now.Format(time.RFC3339Nano)
	past := now.Add(-time.Hour).Format(time.RFC3339Nano)
	future := now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	for _, provider := range []string{"historical", "occupied", "monitor-only"} {
		seed(t, monitorDB, provider, "alive", past, *now)
		if provider == "monitor-only" {
			continue
		}
		result, err := db.Exec(`INSERT INTO chatgpt_accounts
			(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,
			account_expiry,max_concurrent_users,monitor_account_id,monitor_status,status,created_at,updated_at)
			VALUES (?,x'0102','test',x'0102','test',?,2,?,'alive','expired',?,?)`, provider, past, provider, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		accountID, _ := result.LastInsertId()
		result, err = db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
			VALUES (?,'TEST',30,'redeemed',?,?,?,?)`, []byte(provider), stamp, future, stamp, stamp)
		if err != nil {
			t.Fatal(err)
		}
		cardID, _ := result.LastInsertId()
		state, active := "revoked", 0
		if provider == "occupied" {
			state, active = "primary", 1
		}
		if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, cardID, accountID, stamp, future, state, active, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	due(t, s, now)
	result, err := s.RunDue(ctx)
	if err != nil || result.Removed != 2 || result.Skipped != 1 {
		t.Fatalf("cleanup=%+v err=%v", result, err)
	}
	if count(t, monitorDB, `SELECT count(*) FROM accounts WHERE deleted_at IS NULL AND provider_account_id='occupied' AND length(enc_credentials)>0`) != 1 || count(t, monitorDB, "SELECT count(*) FROM status_change_log") != 3 {
		t.Fatal("occupied account or history was lost")
	}
	if count(t, db, "SELECT count(*) FROM cards") != 2 || count(t, db, "SELECT count(*) FROM allocations") != 2 || count(t, db, "SELECT count(*) FROM chatgpt_accounts WHERE archived_at IS NOT NULL") != 2 {
		t.Fatal("pool/card/allocation history mismatch")
	}
	if count(t, db, `SELECT count(*) FROM chatgpt_accounts WHERE monitor_account_id='occupied' AND archived_at IS NULL AND length(display_password_secret)>0`) != 1 {
		t.Fatal("active customer lost pool credentials")
	}
	for _, provider := range []string{"historical", "monitor-only"} {
		applied, err := module.ApplyMonitorAccountEvent(ctx, accountsync.Event{EventID: "late-" + provider, Version: 1, Type: accountsync.EventAccountUpdated, OccurredAt: *now, ProviderAccountID: provider, Email: "synthetic@example.test", Plan: "plus", SubscriptionExpiry: now.Add(24 * time.Hour), Status: "alive"})
		if err != nil || applied.Disposition != "ignored_archived" {
			t.Fatalf("late sync=%+v err=%v", applied, err)
		}
	}
}

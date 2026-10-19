package accountcleanup

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"chatgpt-monitor/internal/store"
)

type fakeGuard struct {
	blocked map[string]bool
	err     error
	calls   []string
}

func (g *fakeGuard) PrepareExpiredAccountCleanup(_ context.Context, id string, _ time.Time) (bool, error) {
	g.calls = append(g.calls, id)
	return !g.blocked[id], g.err
}

func setup(t *testing.T) (*Service, *sql.DB, *time.Time, *fakeGuard) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(context.Background(), filepath.Join(dir, "monitor.db"), os.DirFS("../../migrations"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	guard := &fakeGuard{blocked: make(map[string]bool)}
	s, err := New(database.DB(), guard, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, database.DB(), &now, guard
}

func seed(t *testing.T, db *sql.DB, name, status string, expiry any, now time.Time) int64 {
	t.Helper()
	stamp := now.Format(time.RFC3339Nano)
	result, err := db.Exec(`INSERT INTO accounts(provider_account_id,token_type,enc_credentials,credential_key_id,
		current_expiry,auth_expiry,status,import_time,updated_at) VALUES (?,'access',x'010203','key',?,?,?, ?,?)`, name, expiry, stamp, status, stamp, stamp)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := result.LastInsertId()
	if _, err := db.Exec(`INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)`, id, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO status_change_log(account_id,at,field,evidence_code,evidence_level,evidence_signature)
		VALUES (?,?,'status','seed','live_verified','seed')`, id, stamp); err != nil {
		t.Fatal(err)
	}
	return id
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func due(t *testing.T, s *Service, now *time.Time) {
	t.Helper()
	if result, err := s.RunDue(context.Background()); err != nil || result.Ran {
		t.Fatalf("initialize schedule=%+v err=%v", result, err)
	}
	*now = now.Add(Interval)
}

func TestDurableSevenDayScheduleAndHistory(t *testing.T) {
	s, db, now, _ := setup(t)
	id := seed(t, db, "expired", "alive", now.Add(-time.Hour).Format(time.RFC3339Nano), *now)
	stamp := now.Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO device_auth_sessions(id,account_id,enc_device_code,credential_key_id,interval_seconds,expires_at,state,created_at,updated_at)
		VALUES ('cleanup-device',?,x'01','key',5,?,'pending',?,?)`, id, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO oauth_auth_sessions(id,account_id,enc_session,credential_key_id,state,expires_at,created_at,updated_at)
		VALUES ('cleanup-oauth',?,x'01','key','pending',?,?,?)`, id, stamp, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	due(t, s, now)
	// Recreating the service (restart) must not change the first due date.
	restarted, _ := New(db, s.guard, s.logger)
	restarted.now = s.now
	*now = now.Add(-time.Second)
	if result, err := restarted.RunDue(context.Background()); err != nil || result.Ran {
		t.Fatalf("early run=%+v err=%v", result, err)
	}
	*now = now.Add(time.Second)
	result, err := restarted.RunDue(context.Background())
	if err != nil || !result.Ran || result.Removed != 1 {
		t.Fatalf("due run=%+v err=%v", result, err)
	}
	if count(t, db, `SELECT count(*) FROM accounts WHERE id=? AND deleted_at IS NOT NULL AND length(enc_credentials)=0 AND credential_key_id='' AND credential_generation=2`, id) != 1 {
		t.Fatal("soft deletion/credential erasure missing")
	}
	if count(t, db, `SELECT count(*) FROM authorization_epochs WHERE account_id=? AND ended_at IS NOT NULL AND terminal_status='dead_normal'`, id) != 1 || count(t, db, "SELECT count(*) FROM status_change_log WHERE account_id=?", id) != 1 {
		t.Fatal("monitor history must be retained")
	}
	if count(t, db, "SELECT count(*) FROM account_cleanup_log WHERE account_id=?", id) != 1 {
		t.Fatal("missing cleanup audit receipt")
	}
	if count(t, db, "SELECT count(*) FROM device_auth_sessions") != 0 || count(t, db, "SELECT count(*) FROM oauth_auth_sessions") != 0 {
		t.Fatal("temporary authorization sessions retained credentials")
	}
	if result, err := s.RunDue(context.Background()); err != nil || result.Ran {
		t.Fatalf("duplicate run=%+v err=%v", result, err)
	}
	*now = now.Add(Interval + time.Hour) // catch up after an outage
	if result, err := s.RunDue(context.Background()); err != nil || !result.Ran || result.Removed != 0 {
		t.Fatalf("catch up=%+v err=%v", result, err)
	}
}

func TestEligibilityAndPendingSynchronization(t *testing.T) {
	s, db, now, guard := setup(t)
	old := now.Add(-time.Hour).Format(time.RFC3339Nano)
	future := now.Add(30 * 24 * time.Hour).Format(time.RFC3339Nano)
	cases := []struct {
		name, status string
		expiry       any
		removed      bool
	}{
		{"expired", "alive", old, true},
		{"normal-no-expiry", "dead_normal", nil, true},
		{"auth-only-expired", "alive", nil, false},
		{"not-expired", "alive", future, false},
		{"renewed-terminal-status", "dead_normal", future, false},
		{"unknown-expiry", "alive", "invalid-date", false},
		{"banned-not-expired", "dead_banned", future, false},
		{"active-customer", "alive", old, false},
	}
	guard.blocked["active-customer"] = true
	for _, test := range cases {
		seed(t, db, test.name, test.status, test.expiry, *now)
	}
	for _, status := range []string{"pending", "processing", "retrying", "failed", "delivered"} {
		id := seed(t, db, "sync-"+status, "alive", old, *now)
		if _, err := db.Exec(`INSERT INTO allocation_account_outbox(event_id,account_id,account_version,event_type,payload_json,delivery_status,created_at,updated_at)
			VALUES (?,?,1,'account.updated','{}',?,?,?)`, status, id, status, old, old); err != nil {
			t.Fatal(err)
		}
	}
	due(t, s, now)
	result, err := s.RunDue(context.Background())
	if err != nil || result.Removed != 3 || result.Skipped != 5 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	for _, test := range cases {
		removed := count(t, db, `SELECT count(*) FROM accounts WHERE provider_account_id=? AND deleted_at IS NOT NULL`, test.name) == 1
		if removed != test.removed {
			t.Errorf("%s removed=%v want=%v", test.name, removed, test.removed)
		}
	}
	if count(t, db, `SELECT count(*) FROM allocation_account_outbox`) != 5 {
		t.Fatal("sync history was deleted")
	}
	if count(t, db, `SELECT count(*) FROM accounts WHERE provider_account_id LIKE 'sync-%' AND deleted_at IS NOT NULL`) != 1 {
		t.Fatal("undelivered events must block cleanup")
	}
}

func TestFailureDoesNotDeleteOrResetDueDate(t *testing.T) {
	s, db, now, guard := setup(t)
	seed(t, db, "expired", "alive", now.Add(-time.Hour).Format(time.RFC3339Nano), *now)
	due(t, s, now)
	guard.err = errors.New("allocation database unavailable")
	if _, err := s.RunDue(context.Background()); err == nil {
		t.Fatal("expected guard failure")
	}
	if count(t, db, "SELECT count(*) FROM accounts WHERE deleted_at IS NULL AND length(enc_credentials)>0") != 1 || count(t, db, "SELECT count(*) FROM maintenance_jobs WHERE lease_token IS NULL AND last_error_code='account_cleanup_failed'") != 1 {
		t.Fatal("failed run must retain credentials and release lease")
	}
	guard.err = nil
	if result, err := s.RunDue(context.Background()); err != nil || result.Removed != 1 {
		t.Fatalf("retry=%+v err=%v", result, err)
	}
}

func TestLeasePreventsOverlapAndRecoversAfterCrash(t *testing.T) {
	s, db, now, _ := setup(t)
	due(t, s, now)
	if _, err := db.Exec(`UPDATE maintenance_jobs SET lease_token='crashed',lease_until=?`, now.Add(time.Minute).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if result, err := s.RunDue(context.Background()); err != nil || result.Ran {
		t.Fatalf("leased run=%+v err=%v", result, err)
	}
	*now = now.Add(2 * time.Minute)
	if result, err := s.RunDue(context.Background()); err != nil || !result.Ran {
		t.Fatalf("recover=%+v err=%v", result, err)
	}
}

func TestConcurrentDueCallsOnlyRunOnce(t *testing.T) {
	s, _, now, _ := setup(t)
	due(t, s, now)
	var wg sync.WaitGroup
	results := make(chan Result, 8)
	errorsCh := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := s.RunDue(context.Background())
			results <- result
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	runs := 0
	for result := range results {
		if result.Ran {
			runs++
		}
	}
	if runs != 1 {
		t.Fatalf("runs=%d want=1", runs)
	}
}

func TestRemovalRechecksRenewedAccountAndCanceledTask(t *testing.T) {
	s, db, now, guard := setup(t)
	id := seed(t, db, "renewed-before-delete", "alive", now.Add(time.Hour).Format(time.RFC3339Nano), *now)
	if removed, err := s.remove(context.Background(), id, *now); err != nil || removed || len(guard.calls) != 0 {
		t.Fatalf("renewed account removed=%v err=%v", removed, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

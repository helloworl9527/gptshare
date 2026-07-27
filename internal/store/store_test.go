package store

import (
	"context"
	"database/sql"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	_ "modernc.org/sqlite"
)

func TestOpenAppliesAndRepeatsMigrations(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(privateTempDir(t), "monitor.db")
	migrationFS := repositoryMigrations(t)

	first, err := Open(ctx, dbPath, migrationFS)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first: %v", err)
	}
	second, err := Open(ctx, dbPath, migrationFS)
	if err != nil {
		t.Fatalf("repeat open: %v", err)
	}
	defer second.Close()

	var count int
	if err := second.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 5 {
		t.Fatalf("migration count = %d, want 5", count)
	}
	assertRequiredSchema(t, second.db)
	assertPragmas(t, second.db)
}

func TestAccountsEmailMigrationFromSchemaThree(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(privateTempDir(t), "monitor.db")
	first, err := Open(ctx, dbPath, firstNMigrations(t, 3))
	if err != nil {
		t.Fatalf("open schema 3: %v", err)
	}
	if _, err := first.db.Exec(`INSERT INTO accounts
		(provider_account_id,label,token_type,enc_credentials,credential_key_id,auth_expiry,import_time,updated_at)
		VALUES ('acct-schema-three','acct-schema-three','access',x'01','key','2026-08-19T00:00:00Z','2026-07-22T00:00:00Z','2026-07-22T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`INSERT INTO authorization_epochs(account_id,started_at,auth_expiry)
		VALUES (1,'2026-07-22T00:00:00Z','2026-08-19T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`INSERT INTO status_change_log(account_id,at,field,evidence_code,evidence_level,evidence_signature)
		VALUES (1,'2026-07-22T00:00:00Z','status','seed','live_verified','seed')`); err != nil {
		t.Fatal(err)
	}
	before := rowCounts(t, first.db, "accounts", "authorization_epochs", "status_change_log")
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, dbPath, repositoryMigrations(t))
	if err != nil {
		t.Fatalf("upgrade to schema 5: %v", err)
	}
	defer upgraded.Close()
	after := rowCounts(t, upgraded.db, "accounts", "authorization_epochs", "status_change_log")
	for table, count := range before {
		if after[table] != count {
			t.Fatalf("%s count changed from %d to %d", table, count, after[table])
		}
	}
	var email sql.NullString
	if err := upgraded.db.QueryRow("SELECT email FROM accounts WHERE provider_account_id='acct-schema-three'").Scan(&email); err != nil {
		t.Fatal(err)
	}
	if email.Valid {
		t.Fatalf("email default=%q, want NULL", email.String)
	}
	var migrationCount, userVersion int
	if err := upgraded.db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&migrationCount); err != nil {
		t.Fatal(err)
	}
	if err := upgraded.db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if migrationCount != 5 || userVersion != 5 {
		t.Fatalf("migration_count=%d user_version=%d", migrationCount, userVersion)
	}
}

func TestSchemaConstraints(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	ctx := context.Background()

	_, err := s.db.ExecContext(ctx, `INSERT INTO accounts
		(provider_account_id, token_type, enc_credentials, credential_key_id, auth_expiry, import_time, updated_at)
		VALUES ('acct-1', 'access', x'01', 'key-1', '2026-08-19T00:00:00Z', '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z')`)
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO accounts
		(provider_account_id, token_type, enc_credentials, credential_key_id, auth_expiry, import_time, updated_at)
		VALUES ('acct-1', 'refresh', x'02', 'key-1', '2026-08-19T00:00:00Z', '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z')`); err == nil {
		t.Fatal("active provider_account_id duplicate unexpectedly succeeded")
	}
	if _, err := s.db.Exec("UPDATE accounts SET deleted_at = '2026-07-22T01:00:00Z' WHERE provider_account_id = 'acct-1'"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO accounts
		(provider_account_id, token_type, enc_credentials, credential_key_id, auth_expiry, import_time, updated_at)
		VALUES ('acct-1', 'refresh', x'02', 'key-1', '2026-08-19T00:00:00Z', '2026-07-22T00:00:00Z', '2026-07-22T00:00:00Z')`); err != nil {
		t.Fatalf("reimport after soft delete: %v", err)
	}
	if _, err := s.db.Exec("UPDATE accounts SET status = 'maybe' WHERE deleted_at IS NULL"); err == nil {
		t.Fatal("invalid status unexpectedly succeeded")
	}
	if _, err := s.db.Exec(`INSERT INTO authorization_epochs(account_id, started_at, auth_expiry)
		VALUES (9999, '2026-07-22T00:00:00Z', '2026-08-19T00:00:00Z')`); err == nil {
		t.Fatal("foreign key violation unexpectedly succeeded")
	}
	if _, err := s.db.Exec(`INSERT INTO status_change_log
		(account_id, at, field, evidence_code, evidence_level, evidence_signature, review_decision)
		VALUES (2, '2026-07-22T00:00:00Z', 'status', 'candidate', 'contract_verified_live_pending', 'sig-v1', 'confirmed')`); err == nil {
		t.Fatal("review decision without reviewed_at unexpectedly succeeded")
	}
	if _, err := s.db.Exec(`INSERT INTO status_change_log
		(account_id, at, field, evidence_code, evidence_level, evidence_signature, review_decision)
		VALUES (2, '2026-07-22T00:00:00Z', 'status', 'candidate', 'contract_verified_live_pending', 'sig-v1', 'pending')`); err != nil {
		t.Fatalf("pending review with no reviewed_at: %v", err)
	}
}

func TestMigrationRollbackIsAtomic(t *testing.T) {
	dir := privateTempDir(t)
	dbPath := filepath.Join(dir, "rollback.db")
	migrationFS := fstest.MapFS{
		"0001_base.sql": {Data: []byte("CREATE TABLE stable(id INTEGER PRIMARY KEY);")},
		"0002_bad.sql":  {Data: []byte("CREATE TABLE transient(id INTEGER); THIS IS NOT SQL;")},
	}
	if _, err := Open(context.Background(), dbPath, migrationFS); err == nil {
		t.Fatal("invalid migration unexpectedly succeeded")
	}

	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !tableExists(t, db, "stable") {
		t.Fatal("previous migration was not retained")
	}
	if tableExists(t, db, "transient") {
		t.Fatal("failed migration was not rolled back")
	}
	var count int
	if err := db.QueryRow("SELECT count(*) FROM schema_migrations").Scan(&count); err != nil || count != 1 {
		t.Fatalf("migration records = %d, err=%v", count, err)
	}
}

func TestUpgradeFromPreviousMigrationFixture(t *testing.T) {
	dbPath := filepath.Join(privateTempDir(t), "upgrade.db")
	base := []byte("CREATE TABLE base_record(id INTEGER PRIMARY KEY);")
	firstFS := fstest.MapFS{"0001_base.sql": {Data: base}}
	s, err := Open(context.Background(), dbPath, firstFS)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()

	upgradedFS := fstest.MapFS{
		"0001_base.sql": {Data: base},
		"0002_more.sql": {Data: []byte("CREATE TABLE more_record(id INTEGER PRIMARY KEY);")},
	}
	s, err = Open(context.Background(), dbPath, upgradedFS)
	if err != nil {
		t.Fatalf("upgrade open: %v", err)
	}
	defer s.Close()
	if !tableExists(t, s.db, "base_record") || !tableExists(t, s.db, "more_record") {
		t.Fatal("upgrade schema incomplete")
	}
}

func TestMigrationChecksumMismatchFailsClosed(t *testing.T) {
	dbPath := filepath.Join(privateTempDir(t), "checksum.db")
	firstFS := fstest.MapFS{"0001_base.sql": {Data: []byte("CREATE TABLE base_record(id INTEGER PRIMARY KEY);")}}
	s, err := Open(context.Background(), dbPath, firstFS)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	changedFS := fstest.MapFS{"0001_base.sql": {Data: []byte("CREATE TABLE changed_record(id INTEGER PRIMARY KEY);")}}
	if _, err := Open(context.Background(), dbPath, changedFS); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("checksum mismatch error = %v", err)
	}
}

func TestConcurrentWriterWaitsForLock(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("INSERT INTO settings(key,value,is_secret,updated_at) VALUES ('held', x'01', 0, '2026-07-22T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := s.db.Exec("INSERT INTO settings(key,value,is_secret,updated_at) VALUES ('waited', x'02', 0, '2026-07-22T00:00:00Z')")
		done <- err
	}()
	time.Sleep(150 * time.Millisecond)
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("waiting writer failed: %v", err)
	}
	if elapsed := time.Since(started); elapsed < 100*time.Millisecond {
		t.Fatalf("writer did not wait for lock: %s", elapsed)
	}
}

func TestDatabasePermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	dbPath := filepath.Join(dir, "permissions.db")
	s, err := Open(context.Background(), dbPath, repositoryMigrations(t))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertMode(t, dir, 0o700)
	assertMode(t, dbPath, 0o600)
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(dbPath + suffix); err == nil {
			assertMode(t, dbPath+suffix, 0o600)
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
}

func TestAdminLoginAttemptsDoesNotStoreRawIP(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	rows, err := s.db.Query("PRAGMA table_info(admin_login_attempts)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "client_ip" || name == "ip" {
			t.Fatalf("raw IP column %q is forbidden", name)
		}
	}
}

func TestEvidenceReviewColumnsArePersistent(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	rows, err := s.db.Query("PRAGMA table_info(status_change_log)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	for _, name := range []string{"evidence_code", "evidence_level", "evidence_signature", "review_decision", "reviewed_at", "review_operator", "review_reason"} {
		if !columns[name] {
			t.Errorf("missing status_change_log column %s", name)
		}
	}
}

func TestNotificationSchemaAndPublicSettingsDefaults(t *testing.T) {
	s := openTestStore(t)
	defer s.Close()
	columns := make(map[string]bool)
	rows, err := s.db.Query("PRAGMA table_info(alert_events)")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notnull, &defaultValue, &pk); err != nil {
			t.Fatal(err)
		}
		columns[name] = true
	}
	rows.Close()
	for _, name := range []string{"delivery_status", "attempts", "next_attempt_at", "claimed_at", "last_error_code"} {
		if !columns[name] {
			t.Errorf("missing alert_events column %s", name)
		}
	}
	var poll, days string
	if err := s.db.QueryRow("SELECT CAST(value AS TEXT) FROM settings WHERE key='poll_interval'").Scan(&poll); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow("SELECT CAST(value AS TEXT) FROM settings WHERE key='near_expiry_days'").Scan(&days); err != nil {
		t.Fatal(err)
	}
	if poll != "3600" || days != "3" {
		t.Fatalf("poll=%s days=%s", poll, days)
	}
	var internalPoll int
	if err := s.db.QueryRow("SELECT count(*) FROM settings WHERE key='internal.poll_interval_seconds'").Scan(&internalPoll); err != nil || internalPoll != 0 {
		t.Fatalf("legacy internal poll key count=%d err=%v", internalPoll, err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(privateTempDir(t), "test.db"), repositoryMigrations(t))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func repositoryMigrations(t *testing.T) fs.FS {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return os.DirFS(root)
}

func firstNMigrations(t *testing.T, count int) fs.FS {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	migrations := fstest.MapFS{}
	added := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		if added >= count {
			break
		}
		contents, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		migrations[entry.Name()] = &fstest.MapFile{Data: contents}
		added++
	}
	if added != count {
		t.Fatalf("added %d migrations, want %d", added, count)
	}
	return migrations
}

func rowCounts(t *testing.T, db *sql.DB, tables ...string) map[string]int {
	t.Helper()
	counts := make(map[string]int, len(tables))
	for _, table := range tables {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		counts[table] = count
	}
	return counts
}

func assertRequiredSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"accounts", "authorization_epochs", "status_change_log", "poll_runs", "alert_events", "device_auth_sessions", "oauth_auth_sessions", "settings", "settings_audit", "admin_login_attempts", "admin_sessions", "schema_migrations"} {
		if !tableExists(t, db, table) {
			t.Errorf("missing table %s", table)
		}
	}
	for _, index := range []string{"accounts_provider_active_uq", "accounts_poll_due_idx", "accounts_poll_ready_idx", "authorization_epochs_active_uq", "status_change_review_idx", "alert_events_delivery_idx", "oauth_auth_expiry_idx", "settings_audit_at_idx", "admin_login_attempts_rate_idx", "poll_runs_account_started_idx", "accounts_pending_evidence_idx"} {
		var count int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&count); err != nil || count != 1 {
			t.Errorf("missing index %s: count=%d err=%v", index, count, err)
		}
	}
}

func assertPragmas(t *testing.T, db *sql.DB) {
	t.Helper()
	checks := map[string]any{"foreign_keys": 1, "journal_mode": "wal", "busy_timeout": busyTimeoutMS, "user_version": 5}
	for pragma, want := range checks {
		var got any
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatal(err)
		}
		switch expected := want.(type) {
		case int:
			var integer int
			if err := db.QueryRow("PRAGMA " + pragma).Scan(&integer); err != nil || integer != expected {
				t.Errorf("PRAGMA %s=%d err=%v, want %d", pragma, integer, err, expected)
			}
		case string:
			if value, ok := got.(string); !ok || value != expected {
				t.Errorf("PRAGMA %s=%v, want %s", pragma, got, expected)
			}
		}
	}
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var count int
	if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count == 1
}

func assertMode(t *testing.T, name string, want fs.FileMode) {
	t.Helper()
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode=%04o, want %04o", name, got, want)
	}
}

package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

func TestOpenHealthAndPrivateSQLite(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "allocation.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.Health(context.Background()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("db permissions=%04o", info.Mode().Perm())
	}
	if store.DB().Stats().MaxOpenConnections != 1 {
		t.Fatalf("max open conns=%d", store.DB().Stats().MaxOpenConnections)
	}
	var version int
	if err := store.DB().QueryRow("SELECT max(version) FROM schema_migrations").Scan(&version); err != nil {
		t.Fatal(err)
	}
	latest, err := LatestSchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	if version != latest {
		t.Fatalf("schema version=%d latest=%d", version, latest)
	}
	matches, err := filepath.Glob(path + ".pre-migrate-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("empty database created unexpected pre-migration backup: %v", matches)
	}
}

func TestRejectsLooseDatabaseDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), filepath.Join(dir, "allocation.db")); err == nil {
		t.Fatal("loose directory unexpectedly accepted")
	}
}

func TestRepeatedOpenDoesNotReplayMigration(t *testing.T) {
	dir := secureTempDir(t)
	path := filepath.Join(dir, "allocation.db")
	first, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	first.Close()
	second, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var count int
	if err := second.DB().QueryRow("SELECT count(*) FROM schema_migrations WHERE version=1").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("migration replayed count=%d", count)
	}
}

func TestFixtureDatabaseUpgradeCreatesBackupAndPreservesData(t *testing.T) {
	dir := secureTempDir(t)
	path := filepath.Join(dir, "allocation.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec("CREATE TABLE legacy_fixture(id INTEGER PRIMARY KEY, value TEXT NOT NULL); INSERT INTO legacy_fixture(value) VALUES ('kept')"); err != nil {
		t.Fatal(err)
	}
	legacy.Close()
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var value string
	if err := store.DB().QueryRow("SELECT value FROM legacy_fixture WHERE id=1").Scan(&value); err != nil || value != "kept" {
		t.Fatalf("fixture value=%q err=%v", value, err)
	}
	matches, err := filepath.Glob(path + ".pre-migrate-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("backup count=%d matches=%v", len(matches), matches)
	}
}

func TestCardDuration90DayMigrationPreservesCardsAndForeignKeys(t *testing.T) {
	dir := secureTempDir(t)
	path := filepath.Join(dir, "allocation.db")
	legacy, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range migrations {
		if item.version >= 10 {
			break
		}
		if err := applyMigration(context.Background(), legacy, item); err != nil {
			t.Fatalf("apply legacy migration %d: %v", item.version, err)
		}
	}
	now := "2026-07-30T00:00:00Z"
	if _, err := legacy.Exec(`INSERT INTO chatgpt_accounts(
		display_username,display_password_secret,display_password_key_id,
		display_2fa_secret,display_2fa_key_id,account_expiry,max_concurrent_users,
		current_allocations,monitor_status,status,created_at,updated_at
	) VALUES ('legacy@example.test',x'01','key',x'02','key','2026-08-29T00:00:00Z',1,1,'alive','full',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	result, err := legacy.Exec(`INSERT INTO cards(
		code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,
		created_at,updated_at,encrypted_code_key_id,encrypted_code
	) VALUES (x'0102','OLD9',90,'redeemed',?,'2026-10-28T00:00:00Z',?,?,'key',x'0304')`, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	cardID, _ := result.LastInsertId()
	if _, err := legacy.Exec(`INSERT INTO allocations(
		card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at
	) VALUES (?,1,?,'2026-10-28T00:00:00Z','primary',1,?,?)`, cardID, now, now, now); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var duration, allocationCardID int
	var codeHash, encrypted []byte
	var suffix, status, redeemedAt, expiresAt, keyID string
	if err := upgraded.DB().QueryRow(`SELECT code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,
		encrypted_code_key_id,encrypted_code FROM cards WHERE id=?`, cardID).Scan(
		&codeHash, &suffix, &duration, &status, &redeemedAt, &expiresAt, &keyID, &encrypted,
	); err != nil {
		t.Fatal(err)
	}
	if string(codeHash) != string([]byte{1, 2}) || suffix != "OLD9" || duration != 90 || status != "redeemed" ||
		redeemedAt != now || expiresAt != "2026-10-28T00:00:00Z" || keyID != "key" || string(encrypted) != string([]byte{3, 4}) {
		t.Fatalf("migrated card hash=%x suffix=%q duration=%d status=%q redeemed=%q expires=%q key=%q encrypted=%x",
			codeHash, suffix, duration, status, redeemedAt, expiresAt, keyID, encrypted)
	}
	if err := upgraded.DB().QueryRow("SELECT card_id FROM allocations WHERE card_id=?", cardID).Scan(&allocationCardID); err != nil || int64(allocationCardID) != cardID {
		t.Fatalf("allocation card=%d err=%v", allocationCardID, err)
	}
	var violations int
	rows, err := upgraded.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations != 0 {
		t.Fatalf("foreign key violations=%d", violations)
	}
	var foreignKeysEnabled int
	if err := upgraded.DB().QueryRow("PRAGMA foreign_keys").Scan(&foreignKeysEnabled); err != nil || foreignKeysEnabled != 1 {
		t.Fatalf("foreign keys enabled=%d err=%v", foreignKeysEnabled, err)
	}
	for _, days := range []int{1, 31, 45, 89, 90} {
		if _, err := upgraded.DB().Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at)
			VALUES (randomblob(32),? ,?,'unused',?,?)`, fmt.Sprintf("%04d", days), days, now, now); err != nil {
			t.Fatalf("duration %d rejected: %v", days, err)
		}
	}
	for _, days := range []int{-1, 0, 91} {
		if _, err := upgraded.DB().Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at)
			VALUES (randomblob(32),? ,?,'unused',?,?)`, fmt.Sprintf("%04d", days), days, now, now); err == nil {
			t.Fatalf("out-of-range duration %d unexpectedly accepted", days)
		}
	}
}

func TestAllocationConstraintsRejectInvalidRows(t *testing.T) {
	store, cleanup := openTestStore(t)
	defer cleanup.Close()
	db := store.DB()
	now := "2026-07-24T00:00:00Z"
	if _, err := db.Exec(`INSERT INTO chatgpt_accounts(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,account_expiry,max_concurrent_users,monitor_status,status,created_at,updated_at)
		VALUES ('acct',x'01','k',x'02','k','2026-08-01T00:00:00Z',2,'alive','available',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at) VALUES (x'01','0001',30,'unused',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at) VALUES (x'02','0002',30,'unused',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (1,1,?,'2026-08-01T00:00:00Z','primary',1,?,?)`, now, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (1,1,?,'2026-08-01T00:00:00Z','primary',1,?,?)`, now, now, now); err == nil {
		t.Fatal("duplicate active primary unexpectedly accepted")
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (2,1,?,'2026-08-01T00:00:00Z','grace',1,?,?)`, now, now, now); err == nil {
		t.Fatal("grace without grace_until and superseded allocation unexpectedly accepted")
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (2,1,?,'2026-08-01T00:00:00Z','revoked',1,?,?)`, now, now, now); err == nil {
		t.Fatal("revoked active allocation unexpectedly accepted")
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,grace_until,allocation_state,active,superseded_by_allocation_id,created_at,updated_at)
		VALUES (2,1,?,'2026-08-01T00:00:00Z','2026-07-23T00:00:00Z','grace',1,1,?,?)`, now, now, now); err == nil {
		t.Fatal("grace_until before allocated_at unexpectedly accepted")
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,grace_until,allocation_state,active,superseded_by_allocation_id,created_at,updated_at)
		VALUES (2,1,?,'2026-08-01T00:00:00Z','2026-07-25T00:00:00Z','grace',1,1,?,?)`, now, now, now); err != nil {
		t.Fatalf("valid active grace rejected: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,grace_until,allocation_state,active,superseded_by_allocation_id,created_at,updated_at)
		VALUES (2,1,?,'2026-08-01T00:00:00Z','2026-07-25T00:00:00Z','grace',1,1,?,?)`, now, now, now); err == nil {
		t.Fatal("duplicate active grace unexpectedly accepted")
	}
}

func TestIntegrityCheckAndBackupRestore(t *testing.T) {
	store, cleanup := openTestStore(t)
	defer cleanup.Close()
	if err := IntegrityCheck(context.Background(), store.DB()); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(cleanup.dir, "allocation.db")
	backup, err := BackupSQLite(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec("CREATE TABLE restore_marker(value TEXT)"); err != nil {
		t.Fatal(err)
	}
	store.Close()
	if err := RestoreSQLiteBackup(backup, dbPath); err != nil {
		t.Fatal(err)
	}
	restored, err := Open(context.Background(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var count int
	err = restored.DB().QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='restore_marker'").Scan(&count)
	if err != nil || count != 0 {
		t.Fatalf("restore marker survived count=%d err=%v", count, err)
	}
}

type cleanupStore struct {
	dir   string
	close func()
}

func (c cleanupStore) Close() { c.close() }

func openTestStore(t *testing.T) (*Store, cleanupStore) {
	t.Helper()
	dir := secureTempDir(t)
	path := filepath.Join(dir, "allocation.db")
	store, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	return store, cleanupStore{dir: dir, close: func() { _ = store.Close() }}
}

func secureTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

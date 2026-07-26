package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

type migration struct {
	version  int
	name     string
	contents []byte
	checksum string
}

func migrate(ctx context.Context, db *sql.DB, migrationFS fs.FS) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL UNIQUE,
		checksum TEXT NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	migrations, err := loadMigrations(migrationFS)
	if err != nil {
		return err
	}
	for _, item := range migrations {
		var appliedName, checksum string
		err := db.QueryRowContext(ctx, "SELECT name, checksum FROM schema_migrations WHERE version = ?", item.version).Scan(&appliedName, &checksum)
		switch {
		case err == nil:
			if appliedName != item.name {
				return fmt.Errorf("migration %04d name mismatch", item.version)
			}
			if checksum != item.checksum {
				return fmt.Errorf("migration %04d checksum mismatch", item.version)
			}
			continue
		case err != sql.ErrNoRows:
			return fmt.Errorf("read migration %04d: %w", item.version, err)
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %04d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx, string(item.contents)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %04d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO schema_migrations(version, name, checksum) VALUES (?, ?, ?)",
			item.version, item.name, item.checksum); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %04d: %w", item.version, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", item.version)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("set schema version %04d: %w", item.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %04d: %w", item.version, err)
		}
	}
	var schemaVersion int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schemaVersion); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	if schemaVersion != migrations[len(migrations)-1].version {
		return fmt.Errorf("schema version is %d, want %d", schemaVersion, migrations[len(migrations)-1].version)
	}
	return nil
}

func loadMigrations(migrationFS fs.FS) ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	items := make([]migration, 0, len(entries))
	seen := make(map[int]bool)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		matches := migrationName.FindStringSubmatch(entry.Name())
		if matches == nil {
			continue
		}
		version, _ := strconv.Atoi(matches[1])
		if seen[version] {
			return nil, fmt.Errorf("duplicate migration version %04d", version)
		}
		seen[version] = true
		contents, err := fs.ReadFile(migrationFS, path.Clean(entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", entry.Name(), err)
		}
		sum := sha256.Sum256(contents)
		items = append(items, migration{version: version, name: entry.Name(), contents: contents, checksum: hex.EncodeToString(sum[:])})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("no versioned migrations found")
	}
	sort.Slice(items, func(i, j int) bool { return items[i].version < items[j].version })
	if items[0].version != 1 {
		return nil, fmt.Errorf("migration sequence must start at 0001")
	}
	for i := 1; i < len(items); i++ {
		if items[i].version != items[i-1].version+1 {
			return nil, fmt.Errorf("migration sequence gap after %04d", items[i-1].version)
		}
	}
	return items, nil
}

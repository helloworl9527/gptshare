package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const busyTimeoutMS = 5000

type Store struct {
	db *sql.DB
}

func Open(ctx context.Context, dbPath string) (*Store, error) {
	if dbPath == "" {
		return nil, errors.New("database path is required")
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, fmt.Errorf("resolve database path: %w", err)
	}
	if err := ensurePrivateDirectory(filepath.Dir(absPath)); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(absPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close database file: %w", err)
	}
	if err := os.Chmod(absPath, 0o600); err != nil {
		return nil, fmt.Errorf("secure database file: %w", err)
	}
	dsn := (&url.URL{Scheme: "file", Path: filepath.ToSlash(absPath)}).String() +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxIdleTime(5 * time.Minute)
	closeOnError := func(openErr error) (*Store, error) {
		_ = db.Close()
		return nil, openErr
	}
	if err := db.PingContext(ctx); err != nil {
		return closeOnError(fmt.Errorf("ping sqlite: %w", err))
	}
	if err := verifyPragmas(ctx, db); err != nil {
		return closeOnError(err)
	}
	if err := migrate(ctx, db, absPath); err != nil {
		return closeOnError(err)
	}
	if err := IntegrityCheck(ctx, db); err != nil {
		return closeOnError(err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Health(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return fmt.Errorf("database ping: %w", err)
	}
	var one int
	if err := s.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); err != nil || one != 1 {
		return fmt.Errorf("database query: value=%d err=%w", one, err)
	}
	return nil
}

func (s *Store) DB() *sql.DB { return s.db }

func ensurePrivateDirectory(dir string) error {
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create database directory: %w", err)
		}
		return os.Chmod(dir, 0o700)
	}
	if err != nil {
		return fmt.Errorf("inspect database directory: %w", err)
	}
	if !info.IsDir() {
		return errors.New("database parent is not a directory")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("database directory permissions must be 0700 or stricter, got %04o", info.Mode().Perm())
	}
	return nil
}

func verifyPragmas(ctx context.Context, db *sql.DB) error {
	var foreignKeys int
	if err := db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		return fmt.Errorf("foreign_keys pragma not enabled: value=%d err=%w", foreignKeys, err)
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil || !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("journal_mode pragma not WAL: value=%q err=%w", journalMode, err)
	}
	var busyTimeout int
	if err := db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil || busyTimeout != busyTimeoutMS {
		return fmt.Errorf("busy_timeout pragma not %d: value=%d err=%w", busyTimeoutMS, busyTimeout, err)
	}
	var synchronous int
	if err := db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&synchronous); err != nil || synchronous != 1 {
		return fmt.Errorf("synchronous pragma not NORMAL: value=%d err=%w", synchronous, err)
	}
	return nil
}

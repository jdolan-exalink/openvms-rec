// Package db opens the Recorder SQLite database and applies migrations.
//
// It uses modernc.org/sqlite (pure Go) so Windows and ARM builds need no CGO
// toolchain. WAL mode lets the API read while ingest writes.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/jdolan-exalink/openvms-rec/migrations"
)

// Open opens (creating if needed) the database at path and migrates it.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and avoids SQLITE_BUSY storms.
	// Good enough for the MVP index load; add a separate read-only pool if
	// API reads ever contend with ingest.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	if err := Migrate(ctx, db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// Migrate applies pending migrations from the embedded set. Each file runs in
// its own transaction together with its bookkeeping row, so a failure leaves
// the schema at the previous version.
func Migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	slices.Sort(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var n int
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version, applied_at) VALUES (?, ?)`, version, time.Now().UnixMilli()); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

// Version returns the latest applied migration.
func Version(ctx context.Context, db *sql.DB) (string, error) {
	var v sql.NullString
	err := db.QueryRowContext(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&v)
	return v.String, err
}

// Backup writes a consistent copy of the live database to dst using
// VACUUM INTO (safe while the recorder runs). dst must not exist.
func Backup(ctx context.Context, db *sql.DB, dst string) error {
	_, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

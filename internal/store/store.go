// Package store owns every SQLite access of the service. All statements are
// parameterised; no query is ever built by concatenating caller supplied data.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite driver, no cgo
)

// Errors returned by the store.
var (
	ErrNotFound     = errors.New("store: not found")
	ErrCodeTaken    = errors.New("store: short code already in use")
	ErrNoCodesLeft  = errors.New("store: no short code available in the 001-999 range")
	ErrInvalidInput = errors.New("store: invalid input")
)

// DB wraps the SQLite handle.
type DB struct {
	sql       *sql.DB
	loc       *time.Location
	shortBase string
}

// SetShortBaseURL records the public origin used to build Short URL QR
// payloads, for example "https://url.mvtp.cloud". The store needs it so that
// renaming a code keeps the payload of its linked QR codes in sync.
func (db *DB) SetShortBaseURL(base string) {
	db.shortBase = strings.TrimRight(base, "/")
}

// ShortPayload renders the QR payload of a short code.
func (db *DB) ShortPayload(code string) string { return db.shortPayload(code) }

func (db *DB) shortPayload(code string) string { return db.shortBase + "/" + code }

// Open opens (and creates if needed) the SQLite database in WAL mode.
func Open(path string, loc *time.Location) (*DB, error) {
	if loc == nil {
		loc = time.UTC
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("store: create database directory: %w", err)
		}
	}

	dsn := "file:" + url.PathEscape(path) + "?" +
		"_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)" +
		"&_txlock=immediate"

	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open database: %w", err)
	}
	// SQLite tolerates a handful of connections in WAL mode; writers are
	// serialised by the busy timeout and by immediate transactions.
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: ping database: %w", err)
	}

	db := &DB{sql: sqlDB, loc: loc}
	var mode string
	if err := sqlDB.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("store: read journal mode: %w", err)
	}
	if mode != "wal" {
		sqlDB.Close()
		return nil, fmt.Errorf("store: journal mode is %q, expected wal", mode)
	}
	return db, nil
}

// Close releases the database handle.
func (db *DB) Close() error { return db.sql.Close() }

// SQL exposes the underlying handle for maintenance tasks such as backups.
func (db *DB) SQL() *sql.DB { return db.sql }

// Location returns the timezone used for day bucketed statistics.
func (db *DB) Location() *time.Location { return db.loc }

// SQLiteVersion reports the version of the embedded SQLite library.
func (db *DB) SQLiteVersion(ctx context.Context) (string, error) {
	var v string
	err := db.sql.QueryRowContext(ctx, "SELECT sqlite_version()").Scan(&v)
	return v, err
}

// Backup writes a consistent copy of the database to path using VACUUM INTO.
func (db *DB) Backup(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("store: create backup directory: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("store: backup target %s already exists", path)
	}
	if _, err := db.sql.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("store: backup: %w", err)
	}
	return nil
}

// withTx runs fn inside an immediate transaction, rolling back on error.
func (db *DB) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// NewQRID returns a random identifier for a QR record. It doubles as the file
// name of the rendered images, so it is restricted to lowercase hex.
func NewQRID() (string, error) {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("store: generate identifier: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// nullableUnix converts an optional timestamp into a nullable integer column.
func nullableUnix(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC().Unix()
}

// timeFromNull converts a nullable integer column back into a timestamp.
func timeFromNull(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(n.Int64, 0).UTC()
	return &t
}

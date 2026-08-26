package store

import (
	"context"
	"database/sql"
	"fmt"
)

// migrations are applied in order; each one runs inside its own transaction and
// is recorded in schema_migrations so restarts are idempotent.
var migrations = []struct {
	version int
	name    string
	stmts   []string
}{
	{
		version: 1,
		name:    "initial schema",
		stmts: []string{
			`CREATE TABLE admin (
				id            INTEGER PRIMARY KEY CHECK (id = 1),
				username      TEXT    NOT NULL,
				password_hash TEXT    NOT NULL,
				created_at    INTEGER NOT NULL,
				updated_at    INTEGER NOT NULL
			)`,
			`CREATE TABLE short_urls (
				id               INTEGER PRIMARY KEY AUTOINCREMENT,
				code             TEXT    NOT NULL UNIQUE
				                 CHECK (length(code) = 3 AND code GLOB '[0-9][0-9][0-9]' AND code <> '000'),
				target_url       TEXT    NOT NULL,
				note             TEXT    NOT NULL DEFAULT '',
				enabled          INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
				click_count      INTEGER NOT NULL DEFAULT 0,
				created_at       INTEGER NOT NULL,
				updated_at       INTEGER NOT NULL,
				last_accessed_at INTEGER,
				expires_at       INTEGER
			)`,
			`CREATE INDEX idx_short_urls_expires_at ON short_urls(expires_at)`,
			`CREATE INDEX idx_short_urls_created_at ON short_urls(created_at)`,
			`CREATE TABLE qr_codes (
				id         TEXT    PRIMARY KEY,
				kind       TEXT    NOT NULL CHECK (kind IN ('url', 'text', 'short')),
				payload    TEXT    NOT NULL,
				label      TEXT    NOT NULL DEFAULT '',
				short_code TEXT    REFERENCES short_urls(code) ON DELETE CASCADE ON UPDATE CASCADE,
				ec_level   TEXT    NOT NULL CHECK (ec_level IN ('L', 'M', 'Q', 'H')),
				size       INTEGER NOT NULL,
				version    INTEGER NOT NULL DEFAULT 0,
				png_path   TEXT    NOT NULL DEFAULT '',
				svg_path   TEXT    NOT NULL DEFAULT '',
				created_at INTEGER NOT NULL,
				updated_at INTEGER NOT NULL,
				expires_at INTEGER
			)`,
			`CREATE INDEX idx_qr_codes_expires_at ON qr_codes(expires_at)`,
			`CREATE INDEX idx_qr_codes_short_code ON qr_codes(short_code)`,
			`CREATE INDEX idx_qr_codes_created_at ON qr_codes(created_at)`,
			`CREATE TABLE sessions (
				token_hash   TEXT    PRIMARY KEY,
				csrf_token   TEXT    NOT NULL,
				created_at   INTEGER NOT NULL,
				expires_at   INTEGER NOT NULL,
				last_seen_at INTEGER NOT NULL
			)`,
			`CREATE INDEX idx_sessions_expires_at ON sessions(expires_at)`,
			`CREATE TABLE stats_daily (
				day           TEXT    PRIMARY KEY,
				redirects     INTEGER NOT NULL DEFAULT 0,
				urls_created  INTEGER NOT NULL DEFAULT 0,
				qrs_created   INTEGER NOT NULL DEFAULT 0
			)`,
			`CREATE TABLE cleanup_state (
				id            INTEGER PRIMARY KEY CHECK (id = 1),
				last_run_at   INTEGER NOT NULL DEFAULT 0,
				lease_until   INTEGER NOT NULL DEFAULT 0,
				runs          INTEGER NOT NULL DEFAULT 0,
				removed_urls  INTEGER NOT NULL DEFAULT 0,
				removed_qrs   INTEGER NOT NULL DEFAULT 0
			)`,
			`INSERT INTO cleanup_state (id) VALUES (1)`,
		},
	},
	{
		version: 2,
		name:    "administrator audit log",
		stmts: []string{
			`CREATE TABLE audit_log (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				created_at INTEGER NOT NULL,
				action     TEXT    NOT NULL,
				subject    TEXT    NOT NULL DEFAULT '',
				detail     TEXT    NOT NULL DEFAULT '',
				ip         TEXT    NOT NULL DEFAULT '',
				success    INTEGER NOT NULL DEFAULT 1 CHECK (success IN (0, 1))
			)`,
			`CREATE INDEX idx_audit_log_created_at ON audit_log(created_at DESC)`,
		},
	},
}

// Migrate brings the schema up to date. It is safe to call concurrently and on
// every start.
func (db *DB) Migrate(ctx context.Context) error {
	if _, err := db.sql.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    INTEGER PRIMARY KEY,
		name       TEXT    NOT NULL,
		applied_at INTEGER NOT NULL
	)`); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	for _, m := range migrations {
		err := db.withTx(ctx, func(tx *sql.Tx) error {
			var count int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM schema_migrations WHERE version = ?", m.version).Scan(&count); err != nil {
				return fmt.Errorf("store: check migration %d: %w", m.version, err)
			}
			if count > 0 {
				return nil
			}
			for i, stmt := range m.stmts {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("store: migration %d statement %d: %w", m.version, i+1, err)
				}
			}
			if _, err := tx.ExecContext(ctx,
				"INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, unixepoch())",
				m.version, m.name); err != nil {
				return fmt.Errorf("store: record migration %d: %w", m.version, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// SchemaVersion returns the highest applied migration version.
func (db *DB) SchemaVersion(ctx context.Context) (int, error) {
	var v sql.NullInt64
	if err := db.sql.QueryRowContext(ctx, "SELECT MAX(version) FROM schema_migrations").Scan(&v); err != nil {
		return 0, err
	}
	return int(v.Int64), nil
}

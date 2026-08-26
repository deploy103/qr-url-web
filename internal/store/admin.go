package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Admin is the single administrator account of the service.
type Admin struct {
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// HasAdmin reports whether the administrator account has been provisioned.
func (db *DB) HasAdmin(ctx context.Context) (bool, error) {
	var n int
	if err := db.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin WHERE id = 1").Scan(&n); err != nil {
		return false, fmt.Errorf("store: count admin: %w", err)
	}
	return n > 0, nil
}

// Admin loads the administrator account.
func (db *DB) Admin(ctx context.Context) (Admin, error) {
	var a Admin
	var created, updated int64
	err := db.sql.QueryRowContext(ctx,
		"SELECT username, password_hash, created_at, updated_at FROM admin WHERE id = 1").
		Scan(&a.Username, &a.PasswordHash, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Admin{}, ErrNotFound
	}
	if err != nil {
		return Admin{}, fmt.Errorf("store: load admin: %w", err)
	}
	a.CreatedAt = time.Unix(created, 0).UTC()
	a.UpdatedAt = time.Unix(updated, 0).UTC()
	return a, nil
}

// SetAdmin creates or replaces the administrator credentials. Every existing
// session is dropped so that a password change logs the old browser out.
func (db *DB) SetAdmin(ctx context.Context, username, passwordHash string, now time.Time) error {
	if username == "" || passwordHash == "" {
		return fmt.Errorf("%w: username and password hash are required", ErrInvalidInput)
	}
	return db.withTx(ctx, func(tx *sql.Tx) error {
		unix := now.UTC().Unix()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO admin (id, username, password_hash, created_at, updated_at)
			 VALUES (1, ?, ?, ?, ?)
			 ON CONFLICT(id) DO UPDATE SET username = excluded.username,
			                               password_hash = excluded.password_hash,
			                               updated_at = excluded.updated_at`,
			username, passwordHash, unix, unix); err != nil {
			return fmt.Errorf("store: save admin: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM sessions"); err != nil {
			return fmt.Errorf("store: drop sessions: %w", err)
		}
		return nil
	})
}

// UpdateAdminPassword replaces only the password and invalidates every session
// except the one the change was made from, so the administrator is not logged
// out of the browser they are working in while any other session dies.
func (db *DB) UpdateAdminPassword(ctx context.Context, passwordHash, keepTokenHash string, now time.Time) (int64, error) {
	if passwordHash == "" {
		return 0, fmt.Errorf("%w: password hash is required", ErrInvalidInput)
	}
	var revoked int64
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			"UPDATE admin SET password_hash = ?, updated_at = ? WHERE id = 1", passwordHash, now.UTC().Unix())
		if err != nil {
			return fmt.Errorf("store: update admin password: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		res, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash <> ?", keepTokenHash)
		if err != nil {
			return fmt.Errorf("store: revoke other sessions: %w", err)
		}
		revoked, _ = res.RowsAffected()
		return nil
	})
	return revoked, err
}

// DeleteOtherSessions signs out every session but the given one.
func (db *DB) DeleteOtherSessions(ctx context.Context, keepTokenHash string) (int64, error) {
	res, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash <> ?", keepTokenHash)
	if err != nil {
		return 0, fmt.Errorf("store: revoke other sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

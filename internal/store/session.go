package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is a server side administrator session. Only the hash of the cookie
// value is stored, so a database leak cannot be replayed as a login.
type Session struct {
	TokenHash  string
	CSRFToken  string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	LastSeenAt time.Time
}

// CreateSession stores a new session record.
func (db *DB) CreateSession(ctx context.Context, tokenHash, csrfToken string, now, expiresAt time.Time) error {
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO sessions (token_hash, csrf_token, created_at, expires_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)`,
		tokenHash, csrfToken, now.UTC().Unix(), expiresAt.UTC().Unix(), now.UTC().Unix())
	if err != nil {
		return fmt.Errorf("store: create session: %w", err)
	}
	return nil
}

// Session loads a live session and refreshes its last seen timestamp. Expired
// records are reported as not found and removed.
func (db *DB) Session(ctx context.Context, tokenHash string, now time.Time) (Session, error) {
	var s Session
	var created, expires, lastSeen int64
	err := db.sql.QueryRowContext(ctx,
		"SELECT token_hash, csrf_token, created_at, expires_at, last_seen_at FROM sessions WHERE token_hash = ?",
		tokenHash).Scan(&s.TokenHash, &s.CSRFToken, &created, &expires, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: load session: %w", err)
	}
	s.CreatedAt = time.Unix(created, 0).UTC()
	s.ExpiresAt = time.Unix(expires, 0).UTC()
	s.LastSeenAt = time.Unix(lastSeen, 0).UTC()

	if !now.UTC().Before(s.ExpiresAt) {
		_, _ = db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
		return Session{}, ErrNotFound
	}
	if now.UTC().Sub(s.LastSeenAt) > time.Minute {
		if _, err := db.sql.ExecContext(ctx,
			"UPDATE sessions SET last_seen_at = ? WHERE token_hash = ?", now.UTC().Unix(), tokenHash); err != nil {
			return Session{}, fmt.Errorf("store: touch session: %w", err)
		}
	}
	return s, nil
}

// DeleteSession removes a single session.
func (db *DB) DeleteSession(ctx context.Context, tokenHash string) error {
	if _, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash); err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

// DeleteExpiredSessions drops every session that is past its lifetime.
func (db *DB) DeleteExpiredSessions(ctx context.Context, now time.Time) (int64, error) {
	res, err := db.sql.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", now.UTC().Unix())
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountSessions returns the number of stored sessions.
func (db *DB) CountSessions(ctx context.Context) (int64, error) {
	var n int64
	err := db.sql.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions").Scan(&n)
	return n, err
}

package store

import (
	"context"
	"fmt"
	"time"
)

// Audit actions. Keeping them as constants makes the set greppable and stops
// typos from creating a second spelling of the same event.
const (
	AuditLogin          = "login"
	AuditLoginFailed    = "login.failed"
	AuditLogout         = "logout"
	AuditPasswordChange = "password.change"
	AuditSessionsRevoke = "sessions.revoke"
	AuditShortCreate    = "shorturl.create"
	AuditShortUpdate    = "shorturl.update"
	AuditShortDelete    = "shorturl.delete"
	AuditShortExtend    = "shorturl.extend"
	AuditQRCreate       = "qr.create"
	AuditQRUpdate       = "qr.update"
	AuditQRDelete       = "qr.delete"
	AuditQRExtend       = "qr.extend"
	AuditQRRegenerate   = "qr.regenerate"
	AuditCleanup        = "cleanup.run"
	AuditExport         = "export"
)

// auditRetentionDays bounds the size of the log.
const auditRetentionDays = 180

// AuditEntry is one recorded administrator action.
type AuditEntry struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Action    string    `json:"action"`
	Subject   string    `json:"subject"`
	Detail    string    `json:"detail"`
	IP        string    `json:"ip"`
	Success   bool      `json:"success"`
}

// maxAuditField keeps a hostile input from bloating the log.
const maxAuditField = 300

func truncateField(s string) string {
	if len(s) <= maxAuditField {
		return s
	}
	// Cut on a rune boundary so the stored value stays valid UTF-8.
	r := []rune(s)
	for len(string(r)) > maxAuditField {
		r = r[:len(r)-1]
	}
	return string(r) + "…"
}

// RecordAudit appends an entry to the audit log. Failures are returned but the
// caller is expected to treat them as non fatal: losing a log line must never
// fail the operation that was already carried out.
func (db *DB) RecordAudit(ctx context.Context, e AuditEntry) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now()
	}
	_, err := db.sql.ExecContext(ctx,
		`INSERT INTO audit_log (created_at, action, subject, detail, ip, success)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		e.CreatedAt.UTC().Unix(), e.Action, truncateField(e.Subject),
		truncateField(e.Detail), e.IP, boolToInt(e.Success))
	if err != nil {
		return fmt.Errorf("store: record audit entry: %w", err)
	}
	return nil
}

// AuditLog returns the most recent entries, newest first.
func (db *DB) AuditLog(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, created_at, action, subject, detail, ip, success
		 FROM audit_log ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: read audit log: %w", err)
	}
	defer rows.Close()

	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var created int64
		if err := rows.Scan(&e.ID, &created, &e.Action, &e.Subject, &e.Detail, &e.IP, &e.Success); err != nil {
			return nil, err
		}
		e.CreatedAt = time.Unix(created, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

// FailedLoginsSince counts the failed login attempts recorded after t, which
// the dashboard surfaces so a brute force attempt does not go unnoticed.
func (db *DB) FailedLoginsSince(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := db.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM audit_log WHERE action = ? AND created_at >= ?",
		AuditLoginFailed, t.UTC().Unix()).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count failed logins: %w", err)
	}
	return n, nil
}

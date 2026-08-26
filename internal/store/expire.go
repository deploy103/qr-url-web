package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ExpiredSweep summarises one pass of the expiration cleanup.
type ExpiredSweep struct {
	ShortURLs []string // codes that were removed
	QRCodes   []string // QR identifiers that were removed
	Files     []string // files on disk that the caller must unlink
	Sessions  int64
	StatDays  int64
	AuditRows int64
}

// Removed reports whether the sweep deleted anything.
func (s ExpiredSweep) Removed() bool {
	return len(s.ShortURLs) > 0 || len(s.QRCodes) > 0 ||
		s.Sessions > 0 || s.StatDays > 0 || s.AuditRows > 0
}

// statsRetentionDays bounds the size of the daily statistics table.
const statsRetentionDays = 400

// AcquireCleanupLease takes a short lived lease so that only one cleanup runs
// at a time, even across processes. It returns false when another run holds it.
func (db *DB) AcquireCleanupLease(ctx context.Context, now time.Time, lease time.Duration) (bool, error) {
	var acquired bool
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		unix := now.UTC().Unix()
		res, err := tx.ExecContext(ctx,
			"UPDATE cleanup_state SET lease_until = ?, runs = runs + 1 WHERE id = 1 AND lease_until <= ?",
			unix+int64(lease/time.Second), unix)
		if err != nil {
			return fmt.Errorf("store: acquire cleanup lease: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store: acquire cleanup lease: %w", err)
		}
		acquired = n > 0
		return nil
	})
	return acquired, err
}

// ReleaseCleanupLease clears the lease and records the outcome of the run.
func (db *DB) ReleaseCleanupLease(ctx context.Context, now time.Time, removedURLs, removedQRs int) error {
	_, err := db.sql.ExecContext(ctx,
		`UPDATE cleanup_state
		 SET lease_until = 0, last_run_at = ?, removed_urls = removed_urls + ?, removed_qrs = removed_qrs + ?
		 WHERE id = 1`,
		now.UTC().Unix(), removedURLs, removedQRs)
	if err != nil {
		return fmt.Errorf("store: release cleanup lease: %w", err)
	}
	return nil
}

// CleanupStatus reports the bookkeeping of the cleanup worker.
type CleanupStatus struct {
	LastRunAt   *time.Time `json:"lastRunAt"`
	Runs        int64      `json:"runs"`
	RemovedURLs int64      `json:"removedURLs"`
	RemovedQRs  int64      `json:"removedQRs"`
}

// CleanupStatus returns the cleanup bookkeeping row.
func (db *DB) CleanupStatus(ctx context.Context) (CleanupStatus, error) {
	var s CleanupStatus
	var last sql.NullInt64
	err := db.sql.QueryRowContext(ctx,
		"SELECT NULLIF(last_run_at, 0), runs, removed_urls, removed_qrs FROM cleanup_state WHERE id = 1").
		Scan(&last, &s.Runs, &s.RemovedURLs, &s.RemovedQRs)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, fmt.Errorf("store: cleanup status: %w", err)
	}
	s.LastRunAt = timeFromNull(last)
	return s, nil
}

// DeleteExpired removes every expired short URL and QR record in a single
// transaction and returns what was deleted. Files are only reported; unlinking
// them is the caller's job so that it happens after the commit.
func (db *DB) DeleteExpired(ctx context.Context, now time.Time) (ExpiredSweep, error) {
	var sweep ExpiredSweep
	unix := now.UTC().Unix()

	err := db.withTx(ctx, func(tx *sql.Tx) error {
		// 1. Short URLs whose expiry has passed. Their linked QR records go
		//    with them through the foreign key cascade.
		codes, err := selectStrings(ctx, tx,
			"SELECT code FROM short_urls WHERE expires_at IS NOT NULL AND expires_at <= ?", unix)
		if err != nil {
			return err
		}
		if len(codes) > 0 {
			cascadeFiles, err := qrFilesForCodes(ctx, tx, codes)
			if err != nil {
				return err
			}
			cascadeIDs, err := qrIDsForCodes(ctx, tx, codes)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM short_urls WHERE expires_at IS NOT NULL AND expires_at <= ?", unix); err != nil {
				return fmt.Errorf("store: delete expired short urls: %w", err)
			}
			sweep.ShortURLs = codes
			sweep.QRCodes = append(sweep.QRCodes, cascadeIDs...)
			sweep.Files = append(sweep.Files, cascadeFiles...)
		}

		// 2. QR records that expired on their own.
		rows, err := tx.QueryContext(ctx,
			"SELECT id, png_path, svg_path FROM qr_codes WHERE expires_at IS NOT NULL AND expires_at <= ?", unix)
		if err != nil {
			return fmt.Errorf("store: select expired qr codes: %w", err)
		}
		var ids []string
		var files []string
		for rows.Next() {
			var id, png, svg string
			if err := rows.Scan(&id, &png, &svg); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
			files = append(files, nonEmpty(png, svg)...)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(ids) > 0 {
			if _, err := tx.ExecContext(ctx,
				"DELETE FROM qr_codes WHERE expires_at IS NOT NULL AND expires_at <= ?", unix); err != nil {
				return fmt.Errorf("store: delete expired qr codes: %w", err)
			}
			sweep.QRCodes = append(sweep.QRCodes, ids...)
			sweep.Files = append(sweep.Files, files...)
		}

		// 3. Expired sessions and ancient statistics rows.
		res, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", unix)
		if err != nil {
			return fmt.Errorf("store: delete expired sessions: %w", err)
		}
		sweep.Sessions, _ = res.RowsAffected()

		cutoff := now.In(db.loc).AddDate(0, 0, -statsRetentionDays).Format("2006-01-02")
		res, err = tx.ExecContext(ctx, "DELETE FROM stats_daily WHERE day < ?", cutoff)
		if err != nil {
			return fmt.Errorf("store: prune statistics: %w", err)
		}
		sweep.StatDays, _ = res.RowsAffected()

		auditCutoff := now.AddDate(0, 0, -auditRetentionDays).UTC().Unix()
		res, err = tx.ExecContext(ctx, "DELETE FROM audit_log WHERE created_at < ?", auditCutoff)
		if err != nil {
			return fmt.Errorf("store: prune audit log: %w", err)
		}
		sweep.AuditRows, _ = res.RowsAffected()
		return nil
	})
	return sweep, err
}

// DeleteExpiredShortURL removes one expired short URL. It is used by the
// redirect path so that an expired link disappears the moment it is hit, well
// before the periodic sweep runs.
func (db *DB) DeleteExpiredShortURL(ctx context.Context, code string, now time.Time) (bool, []string, error) {
	var removed bool
	var files []string
	unix := now.UTC().Unix()
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM short_urls WHERE code = ? AND expires_at IS NOT NULL AND expires_at <= ?",
			code, unix).Scan(&n)
		if err != nil {
			return fmt.Errorf("store: check expired short url: %w", err)
		}
		if n == 0 {
			return nil
		}
		files, err = qrFilesForCodes(ctx, tx, []string{code})
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM short_urls WHERE code = ? AND expires_at IS NOT NULL AND expires_at <= ?",
			code, unix); err != nil {
			return fmt.Errorf("store: delete expired short url: %w", err)
		}
		removed = true
		return nil
	})
	return removed, files, err
}

// KnownQRFiles returns every file path currently referenced by a QR record,
// which lets the cleanup detect orphaned files on disk.
func (db *DB) KnownQRFiles(ctx context.Context) (map[string]struct{}, error) {
	rows, err := db.sql.QueryContext(ctx, "SELECT png_path, svg_path FROM qr_codes")
	if err != nil {
		return nil, fmt.Errorf("store: known qr files: %w", err)
	}
	defer rows.Close()
	known := make(map[string]struct{})
	for rows.Next() {
		var png, svg string
		if err := rows.Scan(&png, &svg); err != nil {
			return nil, err
		}
		for _, p := range nonEmpty(png, svg) {
			known[p] = struct{}{}
		}
	}
	return known, rows.Err()
}

func selectStrings(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func qrIDsForCodes(ctx context.Context, tx *sql.Tx, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	placeholders := ""
	args := make([]any, len(codes))
	for i, c := range codes {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "?"
		args[i] = c
	}
	return selectStrings(ctx, tx, "SELECT id FROM qr_codes WHERE short_code IN ("+placeholders+")", args...)
}

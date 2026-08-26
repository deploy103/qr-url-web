package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// expiringSoonWindow is how far ahead the dashboard looks for items that are
// about to expire.
const expiringSoonWindow = 48 * time.Hour

// ExpiringSoonWindow exposes the dashboard lookahead.
func ExpiringSoonWindow() time.Duration { return expiringSoonWindow }

// dayKey renders a timestamp as the YYYY-MM-DD bucket in the configured
// timezone, so "today" matches what the administrator sees.
func (db *DB) dayKey(t time.Time) string { return t.In(db.loc).Format("2006-01-02") }

// DayKey exposes the day bucket of a timestamp.
func (db *DB) DayKey(t time.Time) string { return db.dayKey(t) }

// statColumns is an allow list of the counters that bumpStat may touch.
var statColumns = map[string]bool{"redirects": true, "urls_created": true, "qrs_created": true}

func bumpStat(ctx context.Context, tx *sql.Tx, day, column string, delta int) error {
	if !statColumns[column] {
		return fmt.Errorf("store: unknown statistic %q", column)
	}
	_, err := tx.ExecContext(ctx,
		`INSERT INTO stats_daily (day, `+column+`) VALUES (?, ?)
		 ON CONFLICT(day) DO UPDATE SET `+column+` = `+column+` + excluded.`+column, day, delta)
	if err != nil {
		return fmt.Errorf("store: bump %s: %w", column, err)
	}
	return nil
}

// DailyStat is one day of aggregated activity.
type DailyStat struct {
	Day         string `json:"day"`
	Redirects   int64  `json:"redirects"`
	URLsCreated int64  `json:"urlsCreated"`
	QRsCreated  int64  `json:"qrsCreated"`
}

// Dashboard is the aggregated view shown on the admin landing page.
type Dashboard struct {
	ActiveShortURLs   int64       `json:"activeShortURLs"`
	TotalShortURLs    int64       `json:"totalShortURLs"`
	DisabledShortURLs int64       `json:"disabledShortURLs"`
	TotalQRCodes      int64       `json:"totalQRCodes"`
	RevocableQRCodes  int64       `json:"revocableQRCodes"`
	Today             DailyStat   `json:"today"`
	History           []DailyStat `json:"history"`
	FreeCodes         int64       `json:"freeCodes"`
	TotalClicks       int64       `json:"totalClicks"`
}

// Stats returns the dashboard aggregates.
func (db *DB) Stats(ctx context.Context, now time.Time) (Dashboard, error) {
	var d Dashboard
	unix := now.UTC().Unix()

	row := db.sql.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM short_urls WHERE enabled = 1 AND (expires_at IS NULL OR expires_at > ?)),
			(SELECT COUNT(*) FROM short_urls),
			(SELECT COUNT(*) FROM short_urls WHERE enabled = 0),
			(SELECT COUNT(*) FROM qr_codes),
			(SELECT COUNT(*) FROM qr_codes WHERE kind = 'short'),
			(SELECT IFNULL(SUM(click_count), 0) FROM short_urls)`, unix)
	if err := row.Scan(&d.ActiveShortURLs, &d.TotalShortURLs, &d.DisabledShortURLs,
		&d.TotalQRCodes, &d.RevocableQRCodes, &d.TotalClicks); err != nil {
		return d, fmt.Errorf("store: dashboard counters: %w", err)
	}
	d.FreeCodes = 999 - d.TotalShortURLs

	today := db.dayKey(now)
	d.Today = DailyStat{Day: today}
	err := db.sql.QueryRowContext(ctx,
		"SELECT redirects, urls_created, qrs_created FROM stats_daily WHERE day = ?", today).
		Scan(&d.Today.Redirects, &d.Today.URLsCreated, &d.Today.QRsCreated)
	if err != nil && err != sql.ErrNoRows {
		return d, fmt.Errorf("store: today statistics: %w", err)
	}

	rows, err := db.sql.QueryContext(ctx,
		"SELECT day, redirects, urls_created, qrs_created FROM stats_daily ORDER BY day DESC LIMIT 14")
	if err != nil {
		return d, fmt.Errorf("store: statistics history: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s DailyStat
		if err := rows.Scan(&s.Day, &s.Redirects, &s.URLsCreated, &s.QRsCreated); err != nil {
			return d, err
		}
		d.History = append(d.History, s)
	}
	if d.History == nil {
		d.History = []DailyStat{}
	}
	return d, rows.Err()
}

// ExpiringShortURLs lists the short URLs that expire within the dashboard
// lookahead window.
func (db *DB) ExpiringShortURLs(ctx context.Context, now time.Time, limit int) ([]ShortURL, error) {
	return db.ListShortURLs(ctx, ShortURLFilter{
		Status: "expiring", Sort: "expires_at", Order: "asc", Limit: limit,
	}, now)
}

// ExpiringQRCodes lists the QR records that expire within the lookahead window.
func (db *DB) ExpiringQRCodes(ctx context.Context, now time.Time, limit int) ([]QRCode, error) {
	return db.ListQRCodes(ctx, QRFilter{
		Status: "expiring", Sort: "expires_at", Order: "asc", Limit: limit,
	}, now)
}

// RecentShortURLs lists the newest short URLs.
func (db *DB) RecentShortURLs(ctx context.Context, now time.Time, limit int) ([]ShortURL, error) {
	return db.ListShortURLs(ctx, ShortURLFilter{Sort: "created_at", Order: "desc", Limit: limit}, now)
}

// RecentQRCodes lists the newest QR records.
func (db *DB) RecentQRCodes(ctx context.Context, now time.Time, limit int) ([]QRCode, error) {
	return db.ListQRCodes(ctx, QRFilter{Sort: "created_at", Order: "desc", Limit: limit}, now)
}

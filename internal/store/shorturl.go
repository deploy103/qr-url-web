package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ShortURL is a single 001-999 redirect entry.
type ShortURL struct {
	ID             int64
	Code           string
	TargetURL      string
	Note           string
	Enabled        bool
	ClickCount     int64
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastAccessedAt *time.Time
	ExpiresAt      *time.Time
	QRCount        int
}

// IsExpired reports whether the entry is past its expiry at time now.
func (s ShortURL) IsExpired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// Usable reports whether a redirect may be served.
func (s ShortURL) Usable(now time.Time) bool { return s.Enabled && !s.IsExpired(now) }

// ShortURLInput carries the mutable fields of a short URL.
type ShortURLInput struct {
	Code      string // empty selects the lowest free code
	TargetURL string
	Note      string
	Enabled   bool
	ExpiresAt *time.Time
}

// ShortURLFilter selects and orders a listing.
type ShortURLFilter struct {
	Query  string
	Status string // "", "active", "disabled", "expiring", "expired"
	Sort   string
	Order  string
	Limit  int
	Offset int
}

// sortColumns is an allow list; the requested sort key never reaches SQL
// directly. Nullable columns are ordered with their NULLs last.
var sortColumns = map[string][]string{
	"code":             {"s.code"},
	"target_url":       {"s.target_url"},
	"created_at":       {"s.created_at"},
	"updated_at":       {"s.updated_at"},
	"expires_at":       {"s.expires_at IS NULL", "s.expires_at"},
	"click_count":      {"s.click_count"},
	"last_accessed_at": {"s.last_accessed_at IS NULL", "s.last_accessed_at"},
}

// orderClause renders an allow listed sort key into an ORDER BY clause.
func orderClause(sort, order string) string {
	cols, ok := sortColumns[sort]
	if !ok {
		cols = sortColumns["code"]
	}
	direction := "ASC"
	if strings.EqualFold(order, "desc") {
		direction = "DESC"
	}
	parts := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		parts = append(parts, c+" "+direction)
	}
	parts = append(parts, "s.id "+direction) // stable tie breaker
	return strings.Join(parts, ", ")
}

const shortURLColumns = `s.id, s.code, s.target_url, s.note, s.enabled, s.click_count,
	s.created_at, s.updated_at, s.last_accessed_at, s.expires_at,
	(SELECT COUNT(*) FROM qr_codes q WHERE q.short_code = s.code)`

func scanShortURL(scan func(dest ...any) error) (ShortURL, error) {
	var s ShortURL
	var lastAccessed, expires sql.NullInt64
	var created, updated int64
	err := scan(&s.ID, &s.Code, &s.TargetURL, &s.Note, &s.Enabled, &s.ClickCount,
		&created, &updated, &lastAccessed, &expires, &s.QRCount)
	if err != nil {
		return ShortURL{}, err
	}
	s.CreatedAt = time.Unix(created, 0).UTC()
	s.UpdatedAt = time.Unix(updated, 0).UTC()
	s.LastAccessedAt = timeFromNull(lastAccessed)
	s.ExpiresAt = timeFromNull(expires)
	return s, nil
}

// shortURLWhere renders the filter into a WHERE clause and its arguments, so
// the listing and the count can never drift apart.
func shortURLWhere(f ShortURLFilter, now time.Time) (string, []any, error) {
	var where []string
	var args []any

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(s.code LIKE ? ESCAPE '\\' OR s.target_url LIKE ? ESCAPE '\\' OR s.note LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like)
	}
	unix := now.UTC().Unix()
	switch f.Status {
	case "active":
		where = append(where, "s.enabled = 1 AND (s.expires_at IS NULL OR s.expires_at > ?)")
		args = append(args, unix)
	case "disabled":
		where = append(where, "s.enabled = 0")
	case "expiring":
		where = append(where, "s.expires_at IS NOT NULL AND s.expires_at > ? AND s.expires_at <= ?")
		args = append(args, unix, unix+int64(expiringSoonWindow/time.Second))
	case "expired":
		where = append(where, "s.expires_at IS NOT NULL AND s.expires_at <= ?")
		args = append(args, unix)
	case "", "all":
	default:
		return "", nil, fmt.Errorf("%w: unknown status filter %q", ErrInvalidInput, f.Status)
	}

	if len(where) == 0 {
		return "", args, nil
	}
	return " WHERE " + strings.Join(where, " AND "), args, nil
}

// CountShortURLs returns how many rows match the filter, ignoring the page
// limit. The console uses it to say when a listing has been truncated.
func (db *DB) CountShortURLs(ctx context.Context, f ShortURLFilter, now time.Time) (int64, error) {
	clause, args, err := shortURLWhere(f, now)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := db.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM short_urls s"+clause, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count short urls: %w", err)
	}
	return n, nil
}

// ListShortURLs returns the short URLs matching the filter.
func (db *DB) ListShortURLs(ctx context.Context, f ShortURLFilter, now time.Time) ([]ShortURL, error) {
	clause, args, err := shortURLWhere(f, now)
	if err != nil {
		return nil, err
	}

	query := "SELECT " + shortURLColumns + " FROM short_urls s" + clause +
		" ORDER BY " + orderClause(f.Sort, f.Order)
	if f.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, max(f.Offset, 0))
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list short urls: %w", err)
	}
	defer rows.Close()

	out := []ShortURL{}
	for rows.Next() {
		s, err := scanShortURL(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan short url: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ShortURLByCode looks a short URL up by its three digit code.
func (db *DB) ShortURLByCode(ctx context.Context, code string) (ShortURL, error) {
	s, err := scanShortURL(db.sql.QueryRowContext(ctx,
		"SELECT "+shortURLColumns+" FROM short_urls s WHERE s.code = ?", code).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ShortURL{}, ErrNotFound
	}
	if err != nil {
		return ShortURL{}, fmt.Errorf("store: short url by code: %w", err)
	}
	return s, nil
}

// ShortURLByID looks a short URL up by its numeric identifier.
func (db *DB) ShortURLByID(ctx context.Context, id int64) (ShortURL, error) {
	s, err := scanShortURL(db.sql.QueryRowContext(ctx,
		"SELECT "+shortURLColumns+" FROM short_urls s WHERE s.id = ?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return ShortURL{}, ErrNotFound
	}
	if err != nil {
		return ShortURL{}, fmt.Errorf("store: short url by id: %w", err)
	}
	return s, nil
}

// CreateShortURL inserts a new short URL. An empty input code makes the store
// allocate the lowest unused number in the 001-999 range.
func (db *DB) CreateShortURL(ctx context.Context, in ShortURLInput, now time.Time) (ShortURL, error) {
	var created ShortURL
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		code := in.Code
		if code == "" {
			var err error
			if code, err = allocateCode(ctx, tx); err != nil {
				return err
			}
		} else {
			var exists int
			if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM short_urls WHERE code = ?", code).Scan(&exists); err != nil {
				return fmt.Errorf("store: check code: %w", err)
			}
			if exists > 0 {
				return ErrCodeTaken
			}
		}

		unix := now.UTC().Unix()
		res, err := tx.ExecContext(ctx,
			`INSERT INTO short_urls (code, target_url, note, enabled, click_count, created_at, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
			code, in.TargetURL, in.Note, boolToInt(in.Enabled), unix, unix, nullableUnix(in.ExpiresAt))
		if err != nil {
			if isUniqueViolation(err) {
				return ErrCodeTaken
			}
			return fmt.Errorf("store: insert short url: %w", err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return fmt.Errorf("store: insert short url id: %w", err)
		}
		if err := bumpStat(ctx, tx, db.dayKey(now), "urls_created", 1); err != nil {
			return err
		}

		created, err = scanShortURL(tx.QueryRowContext(ctx,
			"SELECT "+shortURLColumns+" FROM short_urls s WHERE s.id = ?", id).Scan)
		if err != nil {
			return fmt.Errorf("store: reload short url: %w", err)
		}
		return nil
	})
	return created, err
}

// allocateCode picks the lowest free code in the 001-999 range.
func allocateCode(ctx context.Context, tx *sql.Tx) (string, error) {
	const q = `
		WITH RECURSIVE candidates(n) AS (
			SELECT 1 UNION ALL SELECT n + 1 FROM candidates WHERE n < 999
		)
		SELECT printf('%03d', n) FROM candidates
		WHERE printf('%03d', n) NOT IN (SELECT code FROM short_urls)
		ORDER BY n LIMIT 1`
	var code string
	err := tx.QueryRowContext(ctx, q).Scan(&code)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoCodesLeft
	}
	if err != nil {
		return "", fmt.Errorf("store: allocate code: %w", err)
	}
	return code, nil
}

// ShortURLUpdate describes a partial update; nil fields are left untouched.
type ShortURLUpdate struct {
	Code       *string
	TargetURL  *string
	Note       *string
	Enabled    *bool
	ExpiresAt  **time.Time // pointer to pointer: nil = keep, *nil = clear
	ResetClick bool
}

// UpdateShortURL applies a partial update and returns the stored row.
func (db *DB) UpdateShortURL(ctx context.Context, id int64, up ShortURLUpdate, now time.Time) (ShortURL, error) {
	var out ShortURL
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var sets []string
		var args []any

		if up.Code != nil {
			var exists int
			if err := tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM short_urls WHERE code = ? AND id <> ?", *up.Code, id).Scan(&exists); err != nil {
				return fmt.Errorf("store: check code: %w", err)
			}
			if exists > 0 {
				return ErrCodeTaken
			}
			sets = append(sets, "code = ?")
			args = append(args, *up.Code)
		}
		if up.TargetURL != nil {
			sets = append(sets, "target_url = ?")
			args = append(args, *up.TargetURL)
		}
		if up.Note != nil {
			sets = append(sets, "note = ?")
			args = append(args, *up.Note)
		}
		if up.Enabled != nil {
			sets = append(sets, "enabled = ?")
			args = append(args, boolToInt(*up.Enabled))
		}
		if up.ExpiresAt != nil {
			sets = append(sets, "expires_at = ?")
			args = append(args, nullableUnix(*up.ExpiresAt))
		}
		if up.ResetClick {
			sets = append(sets, "click_count = 0", "last_accessed_at = NULL")
		}
		if len(sets) == 0 {
			return fmt.Errorf("%w: nothing to update", ErrInvalidInput)
		}
		sets = append(sets, "updated_at = ?")
		args = append(args, now.UTC().Unix(), id)

		res, err := tx.ExecContext(ctx,
			"UPDATE short_urls SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			if isUniqueViolation(err) {
				return ErrCodeTaken
			}
			return fmt.Errorf("store: update short url: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}

		// The foreign key cascades the new code into qr_codes, but the stored
		// payload string has to follow as well or the QR image would keep
		// pointing at the old address.
		if up.Code != nil {
			if _, err := tx.ExecContext(ctx,
				"UPDATE qr_codes SET payload = ?, updated_at = ? WHERE short_code = ?",
				db.shortPayload(*up.Code), now.UTC().Unix(), *up.Code); err != nil {
				return fmt.Errorf("store: rewrite linked qr payloads: %w", err)
			}
		}

		out, err = scanShortURL(tx.QueryRowContext(ctx,
			"SELECT "+shortURLColumns+" FROM short_urls s WHERE s.id = ?", id).Scan)
		if err != nil {
			return fmt.Errorf("store: reload short url: %w", err)
		}
		return nil
	})
	return out, err
}

// DeleteShortURL removes a short URL together with the QR codes linked to it
// and returns the files that the caller must unlink.
func (db *DB) DeleteShortURL(ctx context.Context, id int64) (ShortURL, []string, error) {
	var deleted ShortURL
	var files []string
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		deleted, err = scanShortURL(tx.QueryRowContext(ctx,
			"SELECT "+shortURLColumns+" FROM short_urls s WHERE s.id = ?", id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load short url: %w", err)
		}
		files, err = qrFilesForCodes(ctx, tx, []string{deleted.Code})
		if err != nil {
			return err
		}
		// The qr_codes foreign key cascades, so the linked records go with it.
		if _, err := tx.ExecContext(ctx, "DELETE FROM short_urls WHERE id = ?", id); err != nil {
			return fmt.Errorf("store: delete short url: %w", err)
		}
		return nil
	})
	return deleted, files, err
}

// RecordRedirect increments the counters of a successful redirect.
func (db *DB) RecordRedirect(ctx context.Context, id int64, now time.Time) error {
	return db.withTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx,
			"UPDATE short_urls SET click_count = click_count + 1, last_accessed_at = ? WHERE id = ?",
			now.UTC().Unix(), id); err != nil {
			return fmt.Errorf("store: record redirect: %w", err)
		}
		return bumpStat(ctx, tx, db.dayKey(now), "redirects", 1)
	})
}

// UsedCodes returns every code currently in use, sorted ascending.
func (db *DB) UsedCodes(ctx context.Context) ([]string, error) {
	rows, err := db.sql.QueryContext(ctx, "SELECT code FROM short_urls ORDER BY code")
	if err != nil {
		return nil, fmt.Errorf("store: used codes: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// escapeLike neutralises the LIKE wildcards in user supplied search terms.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "unique constraint")
}

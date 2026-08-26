package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// QR kinds.
const (
	KindURL   = "url"   // the payload is the target URL itself
	KindText  = "text"  // the payload is arbitrary text
	KindShort = "short" // the payload is a short URL of this service
)

// QRCode is a stored QR Code record.
type QRCode struct {
	ID        string
	Kind      string
	Payload   string
	Label     string
	ShortCode *string
	ECLevel   string
	Size      int
	Version   int
	PNGPath   string
	SVGPath   string
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time
}

// IsExpired reports whether the record is past its expiry at time now.
func (q QRCode) IsExpired(now time.Time) bool {
	return q.ExpiresAt != nil && !now.Before(*q.ExpiresAt)
}

// Revocable reports whether expiring the record actually stops people from
// reaching the destination. Only Short URL QR codes have that property: a
// direct or text QR carries its payload inside the printed image, which the
// server cannot reach.
func (q QRCode) Revocable() bool { return q.Kind == KindShort }

// QRCodeInput carries the fields needed to store a new QR Code.
type QRCodeInput struct {
	ID        string
	Kind      string
	Payload   string
	Label     string
	ShortCode *string
	ECLevel   string
	Size      int
	Version   int
	PNGPath   string
	SVGPath   string
	ExpiresAt *time.Time
}

// QRFilter selects and orders a QR listing.
type QRFilter struct {
	Query  string
	Kind   string
	Status string
	Sort   string
	Order  string
	Limit  int
	Offset int
}

var qrSortColumns = map[string][]string{
	"created_at": {"q.created_at"},
	"updated_at": {"q.updated_at"},
	"expires_at": {"q.expires_at IS NULL", "q.expires_at"},
	"kind":       {"q.kind"},
	"payload":    {"q.payload"},
	"label":      {"q.label"},
}

func qrOrderClause(sort, order string) string {
	cols, ok := qrSortColumns[sort]
	if !ok {
		cols = qrSortColumns["created_at"]
	}
	direction := "DESC"
	if strings.EqualFold(order, "asc") {
		direction = "ASC"
	}
	parts := make([]string, 0, len(cols)+1)
	for _, c := range cols {
		parts = append(parts, c+" "+direction)
	}
	parts = append(parts, "q.id "+direction)
	return strings.Join(parts, ", ")
}

const qrColumns = `q.id, q.kind, q.payload, q.label, q.short_code, q.ec_level, q.size,
	q.version, q.png_path, q.svg_path, q.created_at, q.updated_at, q.expires_at`

func scanQRCode(scan func(dest ...any) error) (QRCode, error) {
	var q QRCode
	var shortCode sql.NullString
	var expires sql.NullInt64
	var created, updated int64
	err := scan(&q.ID, &q.Kind, &q.Payload, &q.Label, &shortCode, &q.ECLevel, &q.Size,
		&q.Version, &q.PNGPath, &q.SVGPath, &created, &updated, &expires)
	if err != nil {
		return QRCode{}, err
	}
	if shortCode.Valid {
		c := shortCode.String
		q.ShortCode = &c
	}
	q.CreatedAt = time.Unix(created, 0).UTC()
	q.UpdatedAt = time.Unix(updated, 0).UTC()
	q.ExpiresAt = timeFromNull(expires)
	return q, nil
}

// qrWhere renders the filter into a WHERE clause and its arguments.
func qrWhere(f QRFilter, now time.Time) (string, []any, error) {
	var where []string
	var args []any

	if q := strings.TrimSpace(f.Query); q != "" {
		where = append(where, "(q.payload LIKE ? ESCAPE '\\' OR q.label LIKE ? ESCAPE '\\' OR IFNULL(q.short_code, '') LIKE ? ESCAPE '\\')")
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like, like)
	}
	switch f.Kind {
	case KindURL, KindText, KindShort:
		where = append(where, "q.kind = ?")
		args = append(args, f.Kind)
	case "", "all":
	default:
		return "", nil, fmt.Errorf("%w: unknown QR kind %q", ErrInvalidInput, f.Kind)
	}

	unix := now.UTC().Unix()
	switch f.Status {
	case "active":
		where = append(where, "(q.expires_at IS NULL OR q.expires_at > ?)")
		args = append(args, unix)
	case "expiring":
		where = append(where, "q.expires_at IS NOT NULL AND q.expires_at > ? AND q.expires_at <= ?")
		args = append(args, unix, unix+int64(expiringSoonWindow/time.Second))
	case "expired":
		where = append(where, "q.expires_at IS NOT NULL AND q.expires_at <= ?")
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

// CountQRCodes returns how many records match the filter, ignoring the limit.
func (db *DB) CountQRCodes(ctx context.Context, f QRFilter, now time.Time) (int64, error) {
	clause, args, err := qrWhere(f, now)
	if err != nil {
		return 0, err
	}
	var n int64
	if err := db.sql.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM qr_codes q"+clause, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count qr codes: %w", err)
	}
	return n, nil
}

// ListQRCodes returns the QR records matching the filter.
func (db *DB) ListQRCodes(ctx context.Context, f QRFilter, now time.Time) ([]QRCode, error) {
	clause, args, err := qrWhere(f, now)
	if err != nil {
		return nil, err
	}

	query := "SELECT " + qrColumns + " FROM qr_codes q" + clause +
		" ORDER BY " + qrOrderClause(f.Sort, f.Order)
	if f.Limit > 0 {
		query += " LIMIT ? OFFSET ?"
		args = append(args, f.Limit, max(f.Offset, 0))
	}

	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list qr codes: %w", err)
	}
	defer rows.Close()
	out := []QRCode{}
	for rows.Next() {
		q, err := scanQRCode(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("store: scan qr code: %w", err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// QRCodeByID loads a single QR record.
func (db *DB) QRCodeByID(ctx context.Context, id string) (QRCode, error) {
	q, err := scanQRCode(db.sql.QueryRowContext(ctx,
		"SELECT "+qrColumns+" FROM qr_codes q WHERE q.id = ?", id).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return QRCode{}, ErrNotFound
	}
	if err != nil {
		return QRCode{}, fmt.Errorf("store: qr code by id: %w", err)
	}
	return q, nil
}

// QRCodesByShortCode lists the QR records linked to a short code.
func (db *DB) QRCodesByShortCode(ctx context.Context, code string) ([]QRCode, error) {
	rows, err := db.sql.QueryContext(ctx,
		"SELECT "+qrColumns+" FROM qr_codes q WHERE q.short_code = ? ORDER BY q.created_at", code)
	if err != nil {
		return nil, fmt.Errorf("store: qr codes by short code: %w", err)
	}
	defer rows.Close()
	out := []QRCode{}
	for rows.Next() {
		q, err := scanQRCode(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// CreateQRCode stores a new QR record.
func (db *DB) CreateQRCode(ctx context.Context, in QRCodeInput, now time.Time) (QRCode, error) {
	var out QRCode
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		unix := now.UTC().Unix()
		var shortCode any
		if in.ShortCode != nil {
			shortCode = *in.ShortCode
		}
		_, err := tx.ExecContext(ctx,
			`INSERT INTO qr_codes (id, kind, payload, label, short_code, ec_level, size, version,
				png_path, svg_path, created_at, updated_at, expires_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			in.ID, in.Kind, in.Payload, in.Label, shortCode, in.ECLevel, in.Size, in.Version,
			in.PNGPath, in.SVGPath, unix, unix, nullableUnix(in.ExpiresAt))
		if err != nil {
			return fmt.Errorf("store: insert qr code: %w", err)
		}
		if err := bumpStat(ctx, tx, db.dayKey(now), "qrs_created", 1); err != nil {
			return err
		}
		out, err = scanQRCode(tx.QueryRowContext(ctx,
			"SELECT "+qrColumns+" FROM qr_codes q WHERE q.id = ?", in.ID).Scan)
		if err != nil {
			return fmt.Errorf("store: reload qr code: %w", err)
		}
		return nil
	})
	return out, err
}

// QRCodeUpdate describes a partial update of a QR record.
type QRCodeUpdate struct {
	Label     *string
	ExpiresAt **time.Time
	ECLevel   *string
	Size      *int
	Version   *int
	PNGPath   *string
	SVGPath   *string
}

// UpdateQRCode applies a partial update and returns the stored record.
func (db *DB) UpdateQRCode(ctx context.Context, id string, up QRCodeUpdate, now time.Time) (QRCode, error) {
	var out QRCode
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var sets []string
		var args []any
		if up.Label != nil {
			sets, args = append(sets, "label = ?"), append(args, *up.Label)
		}
		if up.ExpiresAt != nil {
			sets, args = append(sets, "expires_at = ?"), append(args, nullableUnix(*up.ExpiresAt))
		}
		if up.ECLevel != nil {
			sets, args = append(sets, "ec_level = ?"), append(args, *up.ECLevel)
		}
		if up.Size != nil {
			sets, args = append(sets, "size = ?"), append(args, *up.Size)
		}
		if up.Version != nil {
			sets, args = append(sets, "version = ?"), append(args, *up.Version)
		}
		if up.PNGPath != nil {
			sets, args = append(sets, "png_path = ?"), append(args, *up.PNGPath)
		}
		if up.SVGPath != nil {
			sets, args = append(sets, "svg_path = ?"), append(args, *up.SVGPath)
		}
		if len(sets) == 0 {
			return fmt.Errorf("%w: nothing to update", ErrInvalidInput)
		}
		sets = append(sets, "updated_at = ?")
		args = append(args, now.UTC().Unix(), id)

		res, err := tx.ExecContext(ctx, "UPDATE qr_codes SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return fmt.Errorf("store: update qr code: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		out, err = scanQRCode(tx.QueryRowContext(ctx,
			"SELECT "+qrColumns+" FROM qr_codes q WHERE q.id = ?", id).Scan)
		if err != nil {
			return fmt.Errorf("store: reload qr code: %w", err)
		}
		return nil
	})
	return out, err
}

// DeleteQRCode removes a QR record and returns the files to unlink.
func (db *DB) DeleteQRCode(ctx context.Context, id string) (QRCode, []string, error) {
	var removed QRCode
	var files []string
	err := db.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		removed, err = scanQRCode(tx.QueryRowContext(ctx,
			"SELECT "+qrColumns+" FROM qr_codes q WHERE q.id = ?", id).Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("store: load qr code: %w", err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM qr_codes WHERE id = ?", id); err != nil {
			return fmt.Errorf("store: delete qr code: %w", err)
		}
		files = nonEmpty(removed.PNGPath, removed.SVGPath)
		return nil
	})
	return removed, files, err
}

// qrFilesForCodes collects the stored files of the QR records linked to the
// given short codes.
func qrFilesForCodes(ctx context.Context, tx *sql.Tx, codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(codes)), ",")
	args := make([]any, len(codes))
	for i, c := range codes {
		args[i] = c
	}
	rows, err := tx.QueryContext(ctx,
		"SELECT png_path, svg_path FROM qr_codes WHERE short_code IN ("+placeholders+")", args...)
	if err != nil {
		return nil, fmt.Errorf("store: collect linked qr files: %w", err)
	}
	defer rows.Close()
	var files []string
	for rows.Next() {
		var png, svg string
		if err := rows.Scan(&png, &svg); err != nil {
			return nil, err
		}
		files = append(files, nonEmpty(png, svg)...)
	}
	return files, rows.Err()
}

func nonEmpty(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

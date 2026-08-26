package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/deploy103/qr-url-web/internal/auth"
	"github.com/deploy103/qr-url-web/internal/store"
)

// handleExport writes the catalogue as CSV or JSON so the administrator can
// keep a copy outside the service. It is a read-only convenience on top of the
// database backup, not a replacement for it.
func (s *Server) handleExport(w http.ResponseWriter, r *http.Request) {
	// A download has to be a plain navigation, so the token travels in the
	// query string instead of a header. It is still the session bound token,
	// so a cross site link cannot trigger an export.
	sess, ok := sessionFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "로그인이 필요합니다.")
		return
	}
	token := r.URL.Query().Get("csrf")
	if token == "" {
		token = r.Header.Get("X-CSRF-Token")
	}
	if !s.sameOrigin(r) || token == "" || !auth.ConstantTimeEqual(token, sess.CSRFToken) {
		writeJSONError(w, http.StatusForbidden, "보안 토큰이 올바르지 않습니다. 페이지를 새로고침한 뒤 다시 시도해 주세요.")
		return
	}

	kind := r.PathValue("kind")
	format := strings.ToLower(r.URL.Query().Get("format"))
	if format == "" {
		format = "csv"
	}
	if format != "csv" && format != "json" {
		writeJSONError(w, http.StatusBadRequest, "형식은 csv 또는 json만 지원합니다.")
		return
	}

	now := s.now()
	ctx := r.Context()

	var body []byte
	var contentType string
	var err error

	switch kind {
	case "shorturls":
		links, listErr := s.db.ListShortURLs(ctx, store.ShortURLFilter{Sort: "code", Order: "asc"}, now)
		if listErr != nil {
			s.apiError(w, "export short urls", listErr)
			return
		}
		if format == "json" {
			body, err = json.MarshalIndent(s.shortURLViews(links, now), "", "  ")
			contentType = "application/json; charset=utf-8"
		} else {
			body, err = shortURLsCSV(s, links, now)
			contentType = "text/csv; charset=utf-8"
		}
	case "qrcodes":
		records, listErr := s.db.ListQRCodes(ctx, store.QRFilter{Sort: "created_at", Order: "asc"}, now)
		if listErr != nil {
			s.apiError(w, "export qr codes", listErr)
			return
		}
		if format == "json" {
			body, err = json.MarshalIndent(s.qrViews(records, now), "", "  ")
			contentType = "application/json; charset=utf-8"
		} else {
			body, err = qrCodesCSV(s, records, now)
			contentType = "text/csv; charset=utf-8"
		}
	case "audit":
		entries, listErr := s.db.AuditLog(ctx, 500)
		if listErr != nil {
			s.apiError(w, "export audit log", listErr)
			return
		}
		if format == "json" {
			body, err = json.MarshalIndent(entries, "", "  ")
			contentType = "application/json; charset=utf-8"
		} else {
			body, err = auditCSV(entries)
			contentType = "text/csv; charset=utf-8"
		}
	default:
		writeJSONError(w, http.StatusNotFound, "내보낼 수 없는 대상입니다.")
		return
	}
	if err != nil {
		s.apiError(w, "render export", err)
		return
	}

	filename := fmt.Sprintf("qrurl-%s-%s.%s", kind, now.In(s.cfg.Location).Format("20060102-150405"), format)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
	s.audit(r, store.AuditExport, kind, format)
}

// csvCellPrefix guards against formula injection: a cell that a spreadsheet
// would evaluate is prefixed with a single quote so it stays literal text.
func csvCell(value string) string {
	if value == "" {
		return value
	}
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	}
	return value
}

func writeCSV(rows [][]string) ([]byte, error) {
	var buf bytes.Buffer
	// A byte order mark makes Excel open the UTF-8 file with the right
	// encoding, which matters for the Korean notes and labels.
	buf.WriteString("\ufeff")
	wtr := csv.NewWriter(&buf)
	for _, row := range rows {
		safe := make([]string, len(row))
		for i, cell := range row {
			safe[i] = csvCell(cell)
		}
		if err := wtr.Write(safe); err != nil {
			return nil, err
		}
	}
	wtr.Flush()
	if err := wtr.Error(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func shortURLsCSV(s *Server, links []store.ShortURL, now time.Time) ([]byte, error) {
	rows := [][]string{{
		"code", "short_url", "target_url", "note", "status", "enabled",
		"click_count", "created_at", "updated_at", "last_accessed_at", "expires_at", "qr_count",
	}}
	for _, l := range links {
		created := l.CreatedAt
		updated := l.UpdatedAt
		rows = append(rows, []string{
			l.Code,
			s.cfg.ShortURL(l.Code),
			l.TargetURL,
			l.Note,
			shortURLStatus(l, now),
			strconv.FormatBool(l.Enabled),
			strconv.FormatInt(l.ClickCount, 10),
			formatTime(&created),
			formatTime(&updated),
			formatTime(l.LastAccessedAt),
			formatTime(l.ExpiresAt),
			strconv.Itoa(l.QRCount),
		})
	}
	return writeCSV(rows)
}

func qrCodesCSV(s *Server, records []store.QRCode, now time.Time) ([]byte, error) {
	rows := [][]string{{
		"id", "kind", "label", "payload", "short_code", "revocable", "status",
		"ec_level", "size_px", "symbol_version", "created_at", "expires_at",
	}}
	for _, q := range records {
		created := q.CreatedAt
		shortCode := ""
		if q.ShortCode != nil {
			shortCode = *q.ShortCode
		}
		rows = append(rows, []string{
			q.ID,
			q.Kind,
			q.Label,
			q.Payload,
			shortCode,
			strconv.FormatBool(q.Revocable()),
			qrStatus(q, now),
			q.ECLevel,
			strconv.Itoa(q.Size),
			strconv.Itoa(q.Version),
			formatTime(&created),
			formatTime(q.ExpiresAt),
		})
	}
	return writeCSV(rows)
}

func auditCSV(entries []store.AuditEntry) ([]byte, error) {
	rows := [][]string{{"id", "created_at", "action", "subject", "detail", "ip", "success"}}
	for _, e := range entries {
		created := e.CreatedAt
		rows = append(rows, []string{
			strconv.FormatInt(e.ID, 10),
			formatTime(&created),
			e.Action,
			e.Subject,
			e.Detail,
			e.IP,
			strconv.FormatBool(e.Success),
		})
	}
	return writeCSV(rows)
}

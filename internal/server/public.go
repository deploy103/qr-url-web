package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"regexp"
	"strings"
	"time"

	"github.com/mvtp/qrurl/internal/httpx"
	"github.com/mvtp/qrurl/internal/qr"
	"github.com/mvtp/qrurl/internal/store"
	"github.com/mvtp/qrurl/internal/validate"
)

// codeRe is the exact shape of a short code path: three digits, never 000.
var codeRe = regexp.MustCompile(`^[0-9]{3}$`)

type pageData struct {
	Title       string
	Description string
	BaseURL     string
	Body        string
	PublicQR    bool
	MaxPayload  int
	Sizes       []int
	Levels      []levelOption
	Presets     []presetOption
	CSRFToken   string
	Error       string
	Message     string
	Status      int
}

type levelOption struct {
	Value string
	Label string
}

type presetOption struct {
	Value string
	Label string
}

var levelOptions = []levelOption{
	{"L", "L · 약 7% 복원"},
	{"M", "M · 약 15% 복원 (권장)"},
	{"Q", "Q · 약 25% 복원"},
	{"H", "H · 약 30% 복원"},
}

var presetOptions = []presetOption{
	{"none", "만료 없음"},
	{"1h", "1시간"},
	{"1d", "1일"},
	{"7d", "7일"},
	{"30d", "30일"},
	{"custom", "직접 지정"},
}

var sizeOptions = []int{256, 384, 512, 768, 1024, 1536, 2048}

func (s *Server) basePageData(title, description string) pageData {
	return pageData{
		Title:       title,
		Description: description,
		BaseURL:     s.cfg.BaseURL,
		PublicQR:    s.cfg.PublicQR,
		MaxPayload:  qr.MaxPayloadBytes,
		Sizes:       sizeOptions,
		Levels:      levelOptions,
		Presets:     presetOptions,
	}
}

func (s *Server) handleHome(w http.ResponseWriter, r *http.Request) {
	data := s.basePageData("QR 코드 생성기", "URL 또는 텍스트를 그대로 담은 QR 코드를 만듭니다.")
	s.renderPage(w, r, http.StatusOK, "home", data)
}

func (s *Server) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The public probe deliberately reveals nothing that helps fingerprint the
	// deployment. Version, SQLite build and schema number are only added for a
	// caller on the local machine, which is how the operator and any monitoring
	// agent reach it. The console shows the same detail behind authentication.
	type health struct {
		Status        string `json:"status"`
		Time          string `json:"time"`
		UptimeSeconds int64  `json:"uptimeSeconds"`
		Version       string `json:"version,omitempty"`
		SQLite        string `json:"sqlite,omitempty"`
		SchemaVersion int    `json:"schemaVersion,omitempty"`
	}
	h := health{
		Status:        "ok",
		Time:          s.now().Format(time.RFC3339),
		UptimeSeconds: int64(time.Since(s.startedAt).Seconds()),
	}
	w.Header().Set("Cache-Control", "no-store")

	sqliteVersion, err := s.db.SQLiteVersion(ctx)
	if err != nil {
		h.Status = "degraded"
		writeJSON(w, http.StatusServiceUnavailable, h)
		return
	}
	if httpx.IsLoopback(r) {
		h.Version = BuildVersion
		h.SQLite = sqliteVersion
		if v, err := s.db.SchemaVersion(ctx); err == nil {
			h.SchemaVersion = v
		}
	}
	writeJSON(w, http.StatusOK, h)
}

// handleRedirect serves the 001-999 short links. Expiry is checked on every
// request, so a link stops working the moment it lapses even if the background
// cleanup has not run yet.
func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	w.Header().Set("Cache-Control", "no-store")

	// Three digit codes are trivially enumerable by design, so a single
	// address may not sweep the range at speed. Legitimate use is far below
	// this budget.
	if !s.redirectLimiter.Allow(s.clientIP(r)) {
		s.tooManyRequests(w, r)
		return
	}

	if !codeRe.MatchString(code) || code == "000" {
		s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다",
			"주소가 올바르지 않습니다. 단축 주소는 001부터 999까지의 숫자 3자리입니다.")
		return
	}

	link, err := s.db.ShortURLByCode(r.Context(), code)
	if errors.Is(err, store.ErrNotFound) {
		s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다",
			"존재하지 않는 단축 주소입니다.")
		return
	}
	if err != nil {
		s.log.Error("server: lookup short url", "code", code, "error", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "오류가 발생했습니다",
			"잠시 후 다시 시도해 주세요.")
		return
	}

	now := s.now()
	if link.IsExpired(now) {
		// Remove the lapsed entry right away, together with its QR files.
		if removed, files, err := s.db.DeleteExpiredShortURL(r.Context(), code, now); err != nil {
			s.log.Error("server: delete expired short url", "code", code, "error", err)
		} else if removed {
			if err := s.files.Remove(files...); err != nil {
				s.log.Error("server: remove qr files", "code", code, "error", err)
			}
			s.log.Info("expired short url removed on access", "code", code)
		}
		s.renderNotice(w, r, http.StatusGone, "만료된 주소입니다",
			"이 단축 주소는 만료되어 더 이상 사용할 수 없습니다.")
		return
	}
	if !link.Enabled {
		s.renderNotice(w, r, http.StatusForbidden, "사용할 수 없는 주소입니다",
			"이 단축 주소는 현재 비활성 상태입니다.")
		return
	}

	if err := s.db.RecordRedirect(r.Context(), link.ID, now); err != nil {
		// A failed counter update must not break the redirect itself.
		s.log.Error("server: record redirect", "code", code, "error", err)
	}
	http.Redirect(w, r, link.TargetURL, http.StatusFound)
}

// handlePublicRender renders a QR Code on the fly. Nothing is written to the
// database or to disk, so the public generator cannot be used to fill the
// administrator's library.
func (s *Server) handlePublicRender(w http.ResponseWriter, r *http.Request) {
	// The session cookie is scoped to /adminpage, so this endpoint is always
	// anonymous. Turning the public generator off closes it outright; the
	// administrator preview lives behind /adminpage/qr/preview.*.
	if !s.cfg.PublicQR || !s.publicQRAllowed(r) {
		s.renderNotice(w, r, http.StatusForbidden, "사용할 수 없습니다",
			"공개 QR 생성이 비활성화되어 있습니다. 관리자 페이지에서 QR을 만들어 주세요.")
		return
	}
	if !s.publicLimiter.Allow(s.clientIP(r)) {
		s.tooManyRequests(w, r)
		return
	}
	s.renderQRFromQuery(w, r)
}

// publicQRAllowed reports whether the caller may use the public generator.
// With no allow list configured everyone may; with one, only those networks.
func (s *Server) publicQRAllowed(r *http.Request) bool {
	if !s.cfg.PublicQRAllow.Enabled() {
		return true
	}
	addr, err := netip.ParseAddr(s.clientIP(r))
	if err != nil {
		return false
	}
	return s.cfg.PublicQRAllow.Contains(addr)
}

// handleAdminQRPreview renders a QR Code for the administrator console without
// storing anything, so the creation dialog can show a live preview even when
// the public generator is disabled.
func (s *Server) handleAdminQRPreview(w http.ResponseWriter, r *http.Request) {
	s.renderQRFromQuery(w, r)
}

// renderQRFromQuery encodes the data query parameter and writes the image.
func (s *Server) renderQRFromQuery(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	payload, err := validate.Payload(q.Get("data"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	level, err := validate.ECLevel(q.Get("ec"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	size, err := validate.Size(q.Get("size"), 512)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	code, err := qr.Encode(payload, level)
	if err != nil {
		http.Error(w, qrEncodeMessage(err), http.StatusBadRequest)
		return
	}

	svg := strings.HasSuffix(r.URL.Path, ".svg")
	var body []byte
	var contentType, ext string
	if svg {
		body, contentType, ext = code.SVG(qr.RenderOptions{Size: size}), "image/svg+xml; charset=utf-8", "svg"
	} else {
		body, err = code.PNG(qr.RenderOptions{Size: size})
		if err != nil {
			s.log.Error("server: render png", "error", err)
			http.Error(w, "QR 이미지를 만들지 못했습니다.", http.StatusInternalServerError)
			return
		}
		contentType, ext = "image/png", "png"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "private, max-age=60")
	w.Header().Set("X-QR-Version", fmt.Sprintf("%d", code.Version()))
	if q.Get("download") == "1" {
		w.Header().Set("Content-Disposition", contentDisposition(q.Get("name"), ext))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// qrEncodeMessage maps encoder failures onto messages fit for the interface.
func qrEncodeMessage(err error) string {
	switch {
	case errors.Is(err, qr.ErrTooLong):
		return "내용이 너무 길어 QR 코드에 담을 수 없습니다. 오류 정정 레벨을 낮추거나 내용을 줄여 주세요."
	case errors.Is(err, qr.ErrEmptyPayload):
		return "QR에 담을 내용을 입력해 주세요."
	default:
		return "QR 코드를 만들지 못했습니다."
	}
}

var filenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// contentDisposition builds a safe attachment header. The ASCII form is
// stripped down to a character set that cannot break the header, and the
// RFC 5987 form carries the original name percent encoded so browsers can show
// a Korean file name.
func contentDisposition(name, ext string) string {
	ascii := strings.Trim(filenameUnsafe.ReplaceAllString(name, "-"), "-.")
	if len(ascii) > 60 {
		ascii = ascii[:60]
	}
	if ascii == "" {
		ascii = "qrcode"
	}
	ascii += "." + ext

	utf8Name := sanitiseDisplayName(name)
	if utf8Name == "" {
		return fmt.Sprintf("attachment; filename=%q", ascii)
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		ascii, percentEncode(utf8Name+"."+ext))
}

// sanitiseDisplayName keeps letters of any script but drops control characters
// and everything a file system or a header could stumble over, including runs
// of dots that would read as a parent directory.
func sanitiseDisplayName(name string) string {
	var b strings.Builder
	lastDot := false
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// Control characters are dropped outright.
		case strings.ContainsRune(`/\:"'*?<>|;,`, r):
			b.WriteRune('-')
			lastDot = false
		case r == '.':
			if !lastDot {
				b.WriteRune('.')
			}
			lastDot = true
		default:
			b.WriteRune(r)
			lastDot = false
		}
	}
	out := strings.Trim(b.String(), " .-")
	if len([]rune(out)) > 60 {
		out = string([]rune(out)[:60])
	}
	return strings.Trim(out, " .-")
}

// percentEncode escapes every byte outside the RFC 3986 unreserved set, which
// leaves nothing that could terminate or split a header.
func percentEncode(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '.', c == '_', c == '~':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}

func (s *Server) renderNotice(w http.ResponseWriter, r *http.Request, status int, title, message string) {
	if isAPIPath(r.URL.Path) {
		writeJSONError(w, status, message)
		return
	}
	data := s.basePageData(title, message)
	data.Message = message
	data.Status = status
	s.renderPage(w, r, status, "notice", data)
}

func (s *Server) tooManyRequests(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Retry-After", "60")
	s.renderNotice(w, r, http.StatusTooManyRequests, "요청이 너무 많습니다",
		"잠시 후 다시 시도해 주세요.")
}

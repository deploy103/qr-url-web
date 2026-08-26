// Package validate holds the input rules shared by the HTTP handlers.
package validate

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/mvtp/qrurl/internal/qr"
)

// Limits applied to user supplied values.
const (
	MaxTargetURLLength = 2000
	MaxNoteLength      = 200
	MaxLabelLength     = 120
	MaxPayloadLength   = qr.MaxPayloadBytes
	MaxExpiry          = 10 * 365 * 24 * time.Hour
)

// Errors returned by this package are safe to show to the administrator.
var (
	ErrEmptyCode       = errors.New("단축 코드를 입력해 주세요")
	ErrCodeFormat      = errors.New("단축 코드는 001부터 999 사이의 숫자 3자리여야 합니다")
	ErrEmptyURL        = errors.New("주소를 입력해 주세요")
	ErrURLTooLong      = fmt.Errorf("주소는 %d자를 넘을 수 없습니다", MaxTargetURLLength)
	ErrURLScheme       = errors.New("http:// 또는 https:// 로 시작하는 주소만 사용할 수 있습니다")
	ErrURLHost         = errors.New("주소에 호스트가 없습니다")
	ErrURLCredentials  = errors.New("주소에 아이디/비밀번호를 포함할 수 없습니다")
	ErrURLControlChar  = errors.New("주소에 사용할 수 없는 문자가 있습니다")
	ErrEmptyPayload    = errors.New("QR에 담을 내용을 입력해 주세요")
	ErrPayloadTooLong  = fmt.Errorf("QR 내용은 %d바이트를 넘을 수 없습니다", MaxPayloadLength)
	ErrPayloadNotUTF8  = errors.New("QR 내용이 올바른 UTF-8이 아닙니다")
	ErrExpiryPast      = errors.New("만료 시각은 현재 시각보다 뒤여야 합니다")
	ErrExpiryTooFar    = errors.New("만료 시각이 너무 멉니다")
	ErrExpiryFormat    = errors.New("만료 시각 형식이 올바르지 않습니다")
	ErrExpiryPreset    = errors.New("지원하지 않는 만료 옵션입니다")
	ErrNoteTooLong     = fmt.Errorf("메모는 %d자를 넘을 수 없습니다", MaxNoteLength)
	ErrLabelTooLong    = fmt.Errorf("이름은 %d자를 넘을 수 없습니다", MaxLabelLength)
	ErrUnknownQRKind   = errors.New("지원하지 않는 QR 종류입니다")
	ErrSizeOutOfRange  = fmt.Errorf("QR 크기는 %d에서 %d 사이여야 합니다", qr.MinRenderSize, qr.MaxRenderSize)
	ErrLevelOutOfRange = errors.New("오류 정정 레벨은 L, M, Q, H 중 하나여야 합니다")
)

// ShortCode normalises a short code. One and two digit inputs are zero padded,
// so typing "7" yields "007".
func ShortCode(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmptyCode
	}
	if len(s) > 3 {
		return "", ErrCodeFormat
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return "", ErrCodeFormat
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 999 {
		return "", ErrCodeFormat
	}
	return fmt.Sprintf("%03d", n), nil
}

// TargetURL accepts only absolute http and https URLs. The destination is
// never fetched, resolved or previewed by the server.
func TargetURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", ErrEmptyURL
	}
	if len(s) > MaxTargetURLLength {
		return "", ErrURLTooLong
	}
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) || r == ' ' {
			return "", ErrURLControlChar
		}
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", ErrURLScheme
	}
	// url.Parse lowercases the scheme, so a "JavaScript:" payload is caught
	// here just like "javascript:", "data:", "file:" and "vbscript:".
	switch u.Scheme {
	case "http", "https":
	default:
		return "", ErrURLScheme
	}
	if u.Host == "" {
		return "", ErrURLHost
	}
	if u.User != nil {
		return "", ErrURLCredentials
	}
	if strings.ContainsAny(u.Host, "\\ ") {
		return "", ErrURLControlChar
	}
	return u.String(), nil
}

// Payload checks arbitrary QR content.
func Payload(raw string) (string, error) {
	if raw == "" {
		return "", ErrEmptyPayload
	}
	if len(raw) > MaxPayloadLength {
		return "", ErrPayloadTooLong
	}
	if !utf8.ValidString(raw) {
		return "", ErrPayloadNotUTF8
	}
	return raw, nil
}

// Note trims and bounds an administrator note.
func Note(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if utf8.RuneCountInString(s) > MaxNoteLength {
		return "", ErrNoteTooLong
	}
	return s, nil
}

// Label trims and bounds a QR label.
func Label(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if utf8.RuneCountInString(s) > MaxLabelLength {
		return "", ErrLabelTooLong
	}
	return s, nil
}

// ECLevel parses an error correction level.
func ECLevel(raw string) (qr.Level, error) {
	level, err := qr.ParseLevel(raw)
	if err != nil {
		return 0, ErrLevelOutOfRange
	}
	return level, nil
}

// Size bounds the requested pixel size of a rendered QR Code.
func Size(raw string, def int) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, ErrSizeOutOfRange
	}
	if n < qr.MinRenderSize || n > qr.MaxRenderSize {
		return 0, ErrSizeOutOfRange
	}
	return n, nil
}

// ExpiryPresets are the choices offered in the user interface.
var ExpiryPresets = []string{"none", "1h", "1d", "7d", "30d", "custom"}

// Expiry turns a preset (and an optional custom timestamp) into an absolute
// expiry. A nil result means the item never expires.
func Expiry(preset, custom string, now time.Time, loc *time.Location) (*time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	switch strings.TrimSpace(preset) {
	case "", "none", "never":
		return nil, nil
	case "1h":
		return offset(now, time.Hour), nil
	case "1d":
		return offset(now, 24*time.Hour), nil
	case "7d":
		return offset(now, 7*24*time.Hour), nil
	case "30d":
		return offset(now, 30*24*time.Hour), nil
	case "custom":
		return CustomExpiry(custom, now, loc)
	default:
		return nil, ErrExpiryPreset
	}
}

// CustomExpiry parses an explicit expiry timestamp. Values without a timezone
// are interpreted in the configured location, which is what the browser's
// datetime-local input produces.
func CustomExpiry(custom string, now time.Time, loc *time.Location) (*time.Time, error) {
	s := strings.TrimSpace(custom)
	if s == "" {
		return nil, ErrExpiryFormat
	}
	layouts := []struct {
		layout string
		local  bool
	}{
		{time.RFC3339, false},
		{"2006-01-02T15:04:05", true},
		{"2006-01-02T15:04", true},
		{"2006-01-02 15:04:05", true},
		{"2006-01-02 15:04", true},
		{"2006-01-02", true},
	}
	for _, l := range layouts {
		var (
			t   time.Time
			err error
		)
		if l.local {
			t, err = time.ParseInLocation(l.layout, s, loc)
		} else {
			t, err = time.Parse(l.layout, s)
		}
		if err != nil {
			continue
		}
		return checkExpiry(t, now)
	}
	return nil, ErrExpiryFormat
}

// Extend moves an expiry further into the future. A currently unlimited item
// gains an expiry measured from now.
func Extend(current *time.Time, d time.Duration, now time.Time) (*time.Time, error) {
	if d <= 0 {
		return nil, ErrExpiryFormat
	}
	base := now
	if current != nil && current.After(now) {
		base = *current
	}
	return checkExpiry(base.Add(d), now)
}

// ParseExtendDuration accepts the durations the interface offers.
func ParseExtendDuration(raw string) (time.Duration, error) {
	switch strings.TrimSpace(raw) {
	case "1h":
		return time.Hour, nil
	case "1d":
		return 24 * time.Hour, nil
	case "7d":
		return 7 * 24 * time.Hour, nil
	case "30d":
		return 30 * 24 * time.Hour, nil
	case "90d":
		return 90 * 24 * time.Hour, nil
	}
	return 0, ErrExpiryPreset
}

func offset(now time.Time, d time.Duration) *time.Time {
	t := now.UTC().Add(d).Truncate(time.Second)
	return &t
}

func checkExpiry(t, now time.Time) (*time.Time, error) {
	t = t.UTC().Truncate(time.Second)
	if !t.After(now) {
		return nil, ErrExpiryPast
	}
	if t.Sub(now) > MaxExpiry {
		return nil, ErrExpiryTooFar
	}
	return &t, nil
}

// QRKind validates the QR type discriminator.
func QRKind(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "url":
		return "url", nil
	case "text":
		return "text", nil
	case "short":
		return "short", nil
	}
	return "", ErrUnknownQRKind
}

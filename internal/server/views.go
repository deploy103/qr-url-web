package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/deploy103/qr-url-web/internal/store"
)

// Status values shared with the browser.
const (
	statusActive   = "active"
	statusDisabled = "disabled"
	statusExpiring = "expiring"
	statusExpired  = "expired"
)

type shortURLView struct {
	ID               int64      `json:"id"`
	Code             string     `json:"code"`
	ShortURL         string     `json:"shortUrl"`
	TargetURL        string     `json:"targetUrl"`
	Note             string     `json:"note"`
	Enabled          bool       `json:"enabled"`
	ClickCount       int64      `json:"clickCount"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	LastAccessedAt   *time.Time `json:"lastAccessedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
	RemainingSeconds *int64     `json:"remainingSeconds"`
	Status           string     `json:"status"`
	QRCount          int        `json:"qrCount"`
}

type qrView struct {
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	Payload          string     `json:"payload"`
	Label            string     `json:"label"`
	ShortCode        *string    `json:"shortCode"`
	ShortURL         string     `json:"shortUrl"`
	ECLevel          string     `json:"ecLevel"`
	Size             int        `json:"size"`
	Version          int        `json:"version"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	ExpiresAt        *time.Time `json:"expiresAt"`
	RemainingSeconds *int64     `json:"remainingSeconds"`
	Status           string     `json:"status"`
	Revocable        bool       `json:"revocable"`
	PNGURL           string     `json:"pngUrl"`
	SVGURL           string     `json:"svgUrl"`
}

func remaining(expiresAt *time.Time, now time.Time) *int64 {
	if expiresAt == nil {
		return nil
	}
	secs := int64(expiresAt.Sub(now) / time.Second)
	if secs < 0 {
		secs = 0
	}
	return &secs
}

func shortURLStatus(s store.ShortURL, now time.Time) string {
	switch {
	case s.IsExpired(now):
		return statusExpired
	case !s.Enabled:
		return statusDisabled
	case s.ExpiresAt != nil && s.ExpiresAt.Sub(now) <= store.ExpiringSoonWindow():
		return statusExpiring
	default:
		return statusActive
	}
}

func qrStatus(q store.QRCode, now time.Time) string {
	switch {
	case q.IsExpired(now):
		return statusExpired
	case q.ExpiresAt != nil && q.ExpiresAt.Sub(now) <= store.ExpiringSoonWindow():
		return statusExpiring
	default:
		return statusActive
	}
}

func (s *Server) shortURLView(link store.ShortURL, now time.Time) shortURLView {
	return shortURLView{
		ID:               link.ID,
		Code:             link.Code,
		ShortURL:         s.cfg.ShortURL(link.Code),
		TargetURL:        link.TargetURL,
		Note:             link.Note,
		Enabled:          link.Enabled,
		ClickCount:       link.ClickCount,
		CreatedAt:        link.CreatedAt,
		UpdatedAt:        link.UpdatedAt,
		LastAccessedAt:   link.LastAccessedAt,
		ExpiresAt:        link.ExpiresAt,
		RemainingSeconds: remaining(link.ExpiresAt, now),
		Status:           shortURLStatus(link, now),
		QRCount:          link.QRCount,
	}
}

func (s *Server) shortURLViews(links []store.ShortURL, now time.Time) []shortURLView {
	out := make([]shortURLView, 0, len(links))
	for _, l := range links {
		out = append(out, s.shortURLView(l, now))
	}
	return out
}

func (s *Server) qrView(q store.QRCode, now time.Time) qrView {
	v := qrView{
		ID:               q.ID,
		Kind:             q.Kind,
		Payload:          q.Payload,
		Label:            q.Label,
		ShortCode:        q.ShortCode,
		ECLevel:          q.ECLevel,
		Size:             q.Size,
		Version:          q.Version,
		CreatedAt:        q.CreatedAt,
		UpdatedAt:        q.UpdatedAt,
		ExpiresAt:        q.ExpiresAt,
		RemainingSeconds: remaining(q.ExpiresAt, now),
		Status:           qrStatus(q, now),
		Revocable:        q.Revocable(),
	}
	if q.ShortCode != nil {
		v.ShortURL = s.cfg.ShortURL(*q.ShortCode)
	}
	if q.PNGPath != "" {
		v.PNGURL = "/adminpage/qr/" + q.PNGPath
	}
	if q.SVGPath != "" {
		v.SVGURL = "/adminpage/qr/" + q.SVGPath
	}
	return v
}

func (s *Server) qrViews(codes []store.QRCode, now time.Time) []qrView {
	out := make([]qrView, 0, len(codes))
	for _, q := range codes {
		out = append(out, s.qrView(q, now))
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

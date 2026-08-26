package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/mvtp/qrurl/internal/httpx"
)

// contentSecurityPolicy is strict on purpose: no inline script, no inline
// style and no third party origin. Every asset the pages need is served from
// this host, which is also why the QR generator is implemented locally.
const contentSecurityPolicy = "default-src 'none'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"font-src 'self'; " +
	"connect-src 'self'; " +
	"form-action 'self'; " +
	"base-uri 'none'; " +
	"frame-ancestors 'none'"

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	h := s.securityHeaders(next)
	h = s.limitBody(h)
	h = s.recoverPanic(h)
	h = s.logRequests(h)
	return h
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Permissions-Policy", "accelerometer=(), camera=(), geolocation=(), gyroscope=(), microphone=(), payment=(), usb=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		if s.cfg.HSTS {
			h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.Method != http.MethodGet && r.Method != http.MethodHead {
			r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) recoverPanic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				if rec == http.ErrAbortHandler {
					panic(rec)
				}
				s.log.Error("server: panic recovered", "path", r.URL.Path, "panic", rec)
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := httpx.NewRecorder(w)
		next.ServeHTTP(rec, r)

		// The query string can carry the payload of a QR request, so only the
		// path is logged. No credential, cookie or token is ever recorded.
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.Status,
			"bytes", rec.Bytes,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", s.clientIP(r),
		)
	})
}

func (s *Server) clientIP(r *http.Request) string {
	return httpx.ClientIP(r, s.cfg.TrustedProxies)
}

// sameOrigin reports whether a state changing request came from this site. It
// is a second line of defence behind the CSRF token.
func (s *Server) sameOrigin(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Older clients omit both headers; the CSRF token still applies.
		return true
	}
	return strings.EqualFold(strings.TrimRight(origin, "/"), s.cfg.BaseURL) ||
		strings.EqualFold(origin, "http://"+r.Host) ||
		strings.EqualFold(origin, "https://"+r.Host)
}

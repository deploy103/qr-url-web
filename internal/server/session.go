package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/deploy103/qr-url-web/internal/auth"
	"github.com/deploy103/qr-url-web/internal/store"
)

// Cookie names.
const (
	sessionCookie = "qrurl_session"
	loginCSRFName = "qrurl_login_csrf"
)

type ctxKey int

const sessionCtxKey ctxKey = iota

// sessionFrom returns the authenticated session attached to the request.
func sessionFrom(ctx context.Context) (store.Session, bool) {
	s, ok := ctx.Value(sessionCtxKey).(store.Session)
	return s, ok
}

// startSession issues a fresh session cookie. A new token is always minted, so
// logging in rotates the identifier and defeats session fixation.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) (store.Session, error) {
	token, err := auth.NewToken()
	if err != nil {
		return store.Session{}, err
	}
	csrfToken, err := auth.NewToken()
	if err != nil {
		return store.Session{}, err
	}
	now := s.now()
	expires := now.Add(s.cfg.SessionTTL)
	hash := auth.HashToken(token)
	if err := s.db.CreateSession(r.Context(), hash, csrfToken, now, expires); err != nil {
		return store.Session{}, err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/adminpage",
		Expires:  expires,
		MaxAge:   int(s.cfg.SessionTTL / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	return store.Session{TokenHash: hash, CSRFToken: csrfToken, CreatedAt: now, ExpiresAt: expires}, nil
}

func (s *Server) clearSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil && c.Value != "" {
		if err := s.db.DeleteSession(r.Context(), auth.HashToken(c.Value)); err != nil {
			s.log.Error("server: delete session", "error", err)
		}
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    "",
		Path:     "/adminpage",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
}

// currentSession resolves the session cookie of a request.
func (s *Server) currentSession(r *http.Request) (store.Session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return store.Session{}, false
	}
	sess, err := s.db.Session(r.Context(), auth.HashToken(c.Value), s.now())
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.log.Error("server: load session", "error", err)
		}
		return store.Session{}, false
	}
	return sess, true
}

// requireAdmin wraps a handler so that only an authenticated administrator can
// reach it. API routes answer with JSON, page routes redirect to the login.
func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.apiLimiter.Allow(s.clientIP(r)) {
			s.tooManyRequests(w, r)
			return
		}
		sess, ok := s.currentSession(r)
		if !ok {
			if isAPIPath(r.URL.Path) {
				writeJSONError(w, http.StatusUnauthorized, "로그인이 필요합니다.")
				return
			}
			http.Redirect(w, r, "/adminpage/login", http.StatusSeeOther)
			return
		}
		ctx := context.WithValue(r.Context(), sessionCtxKey, sess)
		next(w, r.WithContext(ctx))
	}
}

// csrf enforces the session bound CSRF token on state changing requests.
func (s *Server) csrf(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, ok := sessionFrom(r.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "로그인이 필요합니다.")
			return
		}
		if !s.sameOrigin(r) {
			writeJSONError(w, http.StatusForbidden, "요청 출처를 확인할 수 없습니다.")
			return
		}
		token := r.Header.Get("X-CSRF-Token")
		if token == "" {
			token = r.FormValue("csrf_token")
		}
		if token == "" || !auth.ConstantTimeEqual(token, sess.CSRFToken) {
			writeJSONError(w, http.StatusForbidden, "보안 토큰이 올바르지 않습니다. 페이지를 새로고침한 뒤 다시 시도해 주세요.")
			return
		}
		next(w, r)
	}
}

// loginCSRFToken issues (or reuses) the double submit token that protects the
// login form, which has no session to bind to yet.
func (s *Server) loginCSRFToken(w http.ResponseWriter, r *http.Request) (string, error) {
	if c, err := r.Cookie(loginCSRFName); err == nil && len(c.Value) >= 20 {
		return c.Value, nil
	}
	token, err := auth.NewToken()
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFName,
		Value:    token,
		Path:     "/adminpage",
		MaxAge:   int(30 * time.Minute / time.Second),
		HttpOnly: true,
		Secure:   s.cfg.SecureCookie,
		SameSite: http.SameSiteLaxMode,
	})
	return token, nil
}

func (s *Server) checkLoginCSRF(r *http.Request) bool {
	c, err := r.Cookie(loginCSRFName)
	if err != nil || c.Value == "" {
		return false
	}
	return auth.ConstantTimeEqual(r.PostFormValue("csrf_token"), c.Value)
}

func isAPIPath(p string) bool { return strings.HasPrefix(p, "/adminpage/api/") }

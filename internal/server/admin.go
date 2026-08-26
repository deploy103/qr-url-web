package server

import (
	"errors"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/deploy103/qr-url-web/internal/auth"
	"github.com/deploy103/qr-url-web/internal/store"
)

// BuildVersion is stamped at link time with -ldflags.
var BuildVersion = "dev"

func (s *Server) handleAdminRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/adminpage", http.StatusMovedPermanently)
}

func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.currentSession(r)
	if !ok {
		http.Redirect(w, r, "/adminpage/login", http.StatusSeeOther)
		return
	}
	// Viewing the console is one of the moments the specification asks for an
	// expiry check, so nothing stale is ever listed.
	if s.cleaner != nil {
		s.cleaner.Touch(r.Context())
	}
	data := s.basePageData("관리자 · QR & 단축 URL", "단축 URL과 QR 코드를 관리합니다.")
	data.CSRFToken = sess.CSRFToken
	w.Header().Set("Cache-Control", "no-store")
	s.renderPage(w, r, http.StatusOK, "admin", data)
}

func (s *Server) handleLoginForm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.currentSession(r); ok {
		http.Redirect(w, r, "/adminpage", http.StatusSeeOther)
		return
	}
	token, err := s.loginCSRFToken(w, r)
	if err != nil {
		s.log.Error("server: issue login token", "error", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "오류가 발생했습니다", "잠시 후 다시 시도해 주세요.")
		return
	}
	data := s.basePageData("관리자 로그인", "관리자만 접근할 수 있습니다.")
	data.CSRFToken = token
	if r.URL.Query().Get("expired") == "1" {
		data.Message = "세션이 만료되어 다시 로그인해야 합니다."
	}
	w.Header().Set("Cache-Control", "no-store")
	s.renderPage(w, r, http.StatusOK, "login", data)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.loginLimiter.Allow(ip) || !s.globalLoginLimiter.Allow("global") {
		s.auditResult(r, store.AuditLoginFailed, "", "rate limited", false)
		s.renderLoginError(w, r, http.StatusTooManyRequests,
			"로그인 시도가 너무 많습니다. 잠시 후 다시 시도해 주세요.")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderLoginError(w, r, http.StatusBadRequest, "요청을 처리하지 못했습니다.")
		return
	}
	if !s.sameOrigin(r) || !s.checkLoginCSRF(r) {
		s.auditResult(r, store.AuditLoginFailed, "", "csrf check failed", false)
		s.renderLoginError(w, r, http.StatusForbidden,
			"보안 토큰이 올바르지 않습니다. 다시 시도해 주세요.")
		return
	}

	username := strings.TrimSpace(r.PostFormValue("username"))
	password := r.PostFormValue("password")

	admin, err := s.db.Admin(r.Context())
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Error("server: load admin", "error", err)
		s.renderLoginError(w, r, http.StatusInternalServerError, "로그인 처리 중 오류가 발생했습니다.")
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		// Burn the same amount of work as a real check so the absence of an
		// account is not observable through response time.
		_, _ = auth.VerifyPassword(auth.DummyHash(), password)
		s.auditResult(r, store.AuditLoginFailed, username, "no administrator account", false)
		s.renderLoginError(w, r, http.StatusUnauthorized, "아이디 또는 비밀번호가 올바르지 않습니다.")
		return
	}

	okPassword, err := auth.VerifyPassword(admin.PasswordHash, password)
	if err != nil {
		s.log.Error("server: verify password", "error", err)
		s.renderLoginError(w, r, http.StatusInternalServerError, "로그인 처리 중 오류가 발생했습니다.")
		return
	}
	// The username is compared in constant time as well so the two factors
	// cannot be distinguished by timing.
	okUser := auth.ConstantTimeEqual(username, admin.Username)
	if !okUser || !okPassword {
		s.log.Warn("login failed", "ip", ip)
		s.auditResult(r, store.AuditLoginFailed, username, "wrong credentials", false)
		s.renderLoginError(w, r, http.StatusUnauthorized, "아이디 또는 비밀번호가 올바르지 않습니다.")
		return
	}

	if _, err := s.startSession(w, r); err != nil {
		s.log.Error("server: start session", "error", err)
		s.renderLoginError(w, r, http.StatusInternalServerError, "세션을 만들지 못했습니다.")
		return
	}
	s.loginLimiter.Reset(ip)
	// The login token is single use.
	http.SetCookie(w, &http.Cookie{
		Name: loginCSRFName, Value: "", Path: "/adminpage", MaxAge: -1,
		HttpOnly: true, Secure: s.cfg.SecureCookie, SameSite: http.SameSiteLaxMode,
	})
	s.log.Info("login succeeded", "ip", ip)
	s.audit(r, store.AuditLogin, admin.Username, "")
	http.Redirect(w, r, "/adminpage", http.StatusSeeOther)
}

func (s *Server) renderLoginError(w http.ResponseWriter, r *http.Request, status int, message string) {
	token, err := s.loginCSRFToken(w, r)
	if err != nil {
		s.log.Error("server: issue login token", "error", err)
	}
	data := s.basePageData("관리자 로그인", "관리자만 접근할 수 있습니다.")
	data.CSRFToken = token
	data.Error = message
	w.Header().Set("Cache-Control", "no-store")
	s.renderPage(w, r, status, "login", data)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	s.audit(r, store.AuditLogout, "", "")
	s.clearSession(w, r)
	http.Redirect(w, r, "/adminpage/login", http.StatusSeeOther)
}

// handleAdminQRFile serves a stored QR image. The database is consulted first,
// so a deleted or expired record is unreachable even while its file is still
// waiting for the next sweep.
func (s *Server) handleAdminQRFile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != path.Base(name) {
		s.renderNotice(w, r, http.StatusBadRequest, "잘못된 요청", "파일 이름이 올바르지 않습니다.")
		return
	}
	ext := strings.ToLower(path.Ext(name))
	id := strings.TrimSuffix(name, ext)
	var contentType string
	switch ext {
	case ".png":
		contentType = "image/png"
	case ".svg":
		contentType = "image/svg+xml; charset=utf-8"
	default:
		s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다", "지원하지 않는 형식입니다.")
		return
	}

	record, err := s.db.QRCodeByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다", "삭제되었거나 존재하지 않는 QR입니다.")
		return
	}
	if err != nil {
		s.log.Error("server: load qr record", "error", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "오류가 발생했습니다", "잠시 후 다시 시도해 주세요.")
		return
	}
	if record.IsExpired(s.now()) {
		s.renderNotice(w, r, http.StatusGone, "만료된 QR입니다", "이 QR은 만료되어 더 이상 내려받을 수 없습니다.")
		return
	}

	stored := record.PNGPath
	if ext == ".svg" {
		stored = record.SVGPath
	}
	if stored == "" || stored != name {
		s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다", "저장된 파일이 없습니다.")
		return
	}

	data, err := s.files.Read(stored)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.renderNotice(w, r, http.StatusNotFound, "찾을 수 없습니다", "저장된 파일이 없습니다.")
			return
		}
		s.log.Error("server: read qr file", "error", err)
		s.renderNotice(w, r, http.StatusInternalServerError, "오류가 발생했습니다", "파일을 읽지 못했습니다.")
		return
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", contentDisposition(downloadName(record), strings.TrimPrefix(ext, ".")))
	}
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func downloadName(q store.QRCode) string {
	if q.Label != "" {
		return q.Label
	}
	if q.ShortCode != nil {
		return "short-" + *q.ShortCode
	}
	return "qr-" + q.ID[:8]
}

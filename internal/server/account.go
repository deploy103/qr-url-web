package server

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/mvtp/qrurl/internal/auth"
	"github.com/mvtp/qrurl/internal/store"
)

type changePasswordRequest struct {
	CurrentPassword string `json:"currentPassword"`
	NewPassword     string `json:"newPassword"`
	ConfirmPassword string `json:"confirmPassword"`
}

// handleChangePassword rotates the administrator password from the console.
// The current password is required, every other session is signed out and the
// session used to make the change is rotated as well.
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "로그인이 필요합니다.")
		return
	}
	var req changePasswordRequest
	if !s.decodeJSON(w, r, &req) {
		return
	}

	// The password form is a credential endpoint, so it shares the login limit.
	ip := s.clientIP(r)
	if !s.loginLimiter.Allow(ip) {
		writeJSONError(w, http.StatusTooManyRequests, "시도가 너무 많습니다. 잠시 후 다시 시도해 주세요.")
		return
	}

	if req.NewPassword != req.ConfirmPassword {
		writeJSONError(w, http.StatusBadRequest, "새 비밀번호가 서로 일치하지 않습니다.")
		return
	}
	if err := auth.ValidatePassword(req.NewPassword); err != nil {
		switch {
		case errors.Is(err, auth.ErrPasswordTooShort):
			writeJSONError(w, http.StatusBadRequest, "새 비밀번호는 12자 이상이어야 합니다.")
		default:
			writeJSONError(w, http.StatusBadRequest, "새 비밀번호가 너무 깁니다.")
		}
		return
	}

	admin, err := s.db.Admin(r.Context())
	if err != nil {
		s.apiError(w, "load admin", err)
		return
	}
	okCurrent, err := auth.VerifyPassword(admin.PasswordHash, req.CurrentPassword)
	if err != nil {
		s.apiError(w, "verify password", err)
		return
	}
	if !okCurrent {
		s.auditResult(r, store.AuditPasswordChange, admin.Username, "wrong current password", false)
		writeJSONError(w, http.StatusForbidden, "현재 비밀번호가 올바르지 않습니다.")
		return
	}
	if same, _ := auth.VerifyPassword(admin.PasswordHash, req.NewPassword); same {
		writeJSONError(w, http.StatusBadRequest, "이전과 다른 비밀번호를 사용해 주세요.")
		return
	}

	hash, err := auth.HashPassword(req.NewPassword, auth.DefaultParams())
	if err != nil {
		s.apiError(w, "hash password", err)
		return
	}
	revoked, err := s.db.UpdateAdminPassword(r.Context(), hash, sess.TokenHash, s.now())
	if err != nil {
		s.apiError(w, "update password", err)
		return
	}

	// Rotate this session too: the old cookie value existed while the old
	// password did, so it should not outlive it.
	if err := s.db.DeleteSession(r.Context(), sess.TokenHash); err != nil {
		s.log.Error("server: drop rotated session", "error", err)
	}
	fresh, err := s.startSession(w, r)
	if err != nil {
		s.apiError(w, "rotate session", err)
		return
	}

	s.loginLimiter.Reset(ip)
	s.audit(r, store.AuditPasswordChange, admin.Username,
		"other sessions revoked: "+strconv.FormatInt(revoked, 10))
	s.log.Info("administrator password changed", "ip", ip, "revokedSessions", revoked)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                true,
		"revokedSessions":   revoked,
		"csrfToken":         fresh.CSRFToken,
		"passwordChangedAt": s.now(),
	})
}

// handleRevokeSessions signs out every other browser.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	sess, ok := sessionFrom(r.Context())
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "로그인이 필요합니다.")
		return
	}
	revoked, err := s.db.DeleteOtherSessions(r.Context(), sess.TokenHash)
	if err != nil {
		s.apiError(w, "revoke sessions", err)
		return
	}
	s.audit(r, store.AuditSessionsRevoke, "", "revoked "+strconv.FormatInt(revoked, 10))
	writeJSON(w, http.StatusOK, map[string]any{"revokedSessions": revoked})
}

// accountResponse describes the administrator account and the current session.
type accountResponse struct {
	Username        string             `json:"username"`
	CreatedAt       time.Time          `json:"createdAt"`
	PasswordChanged time.Time          `json:"passwordChangedAt"`
	SessionExpires  time.Time          `json:"sessionExpiresAt"`
	ActiveSessions  int64              `json:"activeSessions"`
	FailedLogins24h int64              `json:"failedLogins24h"`
	Audit           []store.AuditEntry `json:"audit"`
	Diagnostics     diagnosticsView    `json:"diagnostics"`
}

type diagnosticsView struct {
	Version        string `json:"version"`
	SQLite         string `json:"sqlite"`
	SchemaVersion  int    `json:"schemaVersion"`
	Timezone       string `json:"timezone"`
	UptimeSeconds  int64  `json:"uptimeSeconds"`
	PublicQR       bool   `json:"publicQr"`
	TrustedProxies string `json:"trustedProxies"`
	QRDirectory    string `json:"qrDirectory"`
}

func (s *Server) handleAccount(w http.ResponseWriter, r *http.Request) {
	sess, _ := sessionFrom(r.Context())
	ctx := r.Context()

	admin, err := s.db.Admin(ctx)
	if err != nil {
		s.apiError(w, "load admin", err)
		return
	}
	sessions, err := s.db.CountSessions(ctx)
	if err != nil {
		s.apiError(w, "count sessions", err)
		return
	}
	failed, err := s.db.FailedLoginsSince(ctx, s.now().Add(-24*time.Hour))
	if err != nil {
		s.apiError(w, "count failed logins", err)
		return
	}
	entries, err := s.db.AuditLog(ctx, 60)
	if err != nil {
		s.apiError(w, "read audit log", err)
		return
	}

	sqliteVersion, _ := s.db.SQLiteVersion(ctx)
	schema, _ := s.db.SchemaVersion(ctx)

	writeJSON(w, http.StatusOK, accountResponse{
		Username:        admin.Username,
		CreatedAt:       admin.CreatedAt,
		PasswordChanged: admin.UpdatedAt,
		SessionExpires:  sess.ExpiresAt,
		ActiveSessions:  sessions,
		FailedLogins24h: failed,
		Audit:           entries,
		Diagnostics: diagnosticsView{
			Version:        BuildVersion,
			SQLite:         sqliteVersion,
			SchemaVersion:  schema,
			Timezone:       s.cfg.Location.String(),
			UptimeSeconds:  int64(time.Since(s.startedAt).Seconds()),
			PublicQR:       s.cfg.PublicQR,
			TrustedProxies: s.cfg.TrustedProxies.String(),
			QRDirectory:    s.files.Dir(),
		},
	})
}

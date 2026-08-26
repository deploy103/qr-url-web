package server

import (
	"context"
	"net/http"
	"time"

	"github.com/mvtp/qrurl/internal/store"
)

// audit records an administrator action. It never fails the request: losing a
// log line is bad, but refusing an operation that already happened is worse.
// The write runs on a detached context so a client that hangs up mid response
// still leaves a trace.
func (s *Server) audit(r *http.Request, action, subject, detail string) {
	s.auditResult(r, action, subject, detail, true)
}

func (s *Server) auditResult(r *http.Request, action, subject, detail string, success bool) {
	entry := store.AuditEntry{
		CreatedAt: s.now(),
		Action:    action,
		Subject:   subject,
		Detail:    detail,
		IP:        s.clientIP(r),
		Success:   success,
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
	defer cancel()
	if err := s.db.RecordAudit(ctx, entry); err != nil {
		s.log.Error("server: record audit entry", "action", action, "error", err)
	}
}

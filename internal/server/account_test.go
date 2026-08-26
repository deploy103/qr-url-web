package server_test

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/deploy103/qr-url-web/internal/auth"
	"github.com/deploy103/qr-url-web/internal/store"
)

func TestAccountOverview(t *testing.T) {
	e := newEnv(t)
	e.login()

	out := e.apiJSON(http.MethodGet, "/adminpage/api/account", nil, http.StatusOK)
	if str(t, out, "username") != testUsername {
		t.Fatalf("username = %v", out["username"])
	}
	if num(t, out, "activeSessions") != 1 {
		t.Errorf("active sessions = %v, want 1", out["activeSessions"])
	}
	if num(t, out, "failedLogins24h") != 0 {
		t.Errorf("failed logins = %v, want 0", out["failedLogins24h"])
	}

	diag, ok := out["diagnostics"].(map[string]any)
	if !ok {
		t.Fatalf("no diagnostics block: %v", out)
	}
	if !strings.HasPrefix(str(t, diag, "sqlite"), "3.") {
		t.Errorf("sqlite version = %v", diag["sqlite"])
	}
	if num(t, diag, "schemaVersion") < 2 {
		t.Errorf("schema version = %v, want the audit migration applied", diag["schemaVersion"])
	}
	if str(t, diag, "timezone") != "UTC" {
		t.Errorf("timezone = %v", diag["timezone"])
	}

	// The login that got us here is already in the audit log.
	entries, ok := out["audit"].([]any)
	if !ok || len(entries) == 0 {
		t.Fatalf("audit log is empty: %v", out["audit"])
	}
	first := entries[0].(map[string]any)
	if str(t, first, "action") != store.AuditLogin {
		t.Errorf("newest audit action = %v, want %s", first["action"], store.AuditLogin)
	}
}

func TestChangePassword(t *testing.T) {
	e := newEnv(t)
	e.login()
	oldCSRF := e.csrf

	// A second browser that must be signed out by the change.
	other := e.newSession()

	const newPassword = "a brand new and long password"
	out := e.apiJSON(http.MethodPost, "/adminpage/api/account/password", map[string]any{
		"currentPassword": testPassword,
		"newPassword":     newPassword,
		"confirmPassword": newPassword,
	}, http.StatusOK)

	if num(t, out, "revokedSessions") != 1 {
		t.Errorf("revoked sessions = %v, want 1", out["revokedSessions"])
	}
	freshCSRF := str(t, out, "csrfToken")
	if freshCSRF == oldCSRF {
		t.Error("the CSRF token was not rotated with the session")
	}
	e.csrf = freshCSRF

	// The old session token no longer works.
	res := other.api(http.MethodGet, "/adminpage/api/overview", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the other session survived the password change: %d", res.StatusCode)
	}

	// The rotated session keeps working with the new token.
	e.apiJSON(http.MethodGet, "/adminpage/api/overview", nil, http.StatusOK)

	// The stored hash really changed and matches the new password.
	admin, err := e.db.Admin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := auth.VerifyPassword(admin.PasswordHash, newPassword); err != nil || !ok {
		t.Fatalf("the new password does not verify: %v, %v", ok, err)
	}
	if ok, _ := auth.VerifyPassword(admin.PasswordHash, testPassword); ok {
		t.Fatal("the old password still verifies")
	}

	// It is recorded.
	account := e.apiJSON(http.MethodGet, "/adminpage/api/account", nil, http.StatusOK)
	entries := account["audit"].([]any)
	found := false
	for _, raw := range entries {
		if raw.(map[string]any)["action"] == store.AuditPasswordChange {
			found = true
		}
	}
	if !found {
		t.Error("the password change was not written to the audit log")
	}
}

func TestChangePasswordRejectsBadInput(t *testing.T) {
	e := newEnv(t)
	e.login()

	cases := []struct {
		name    string
		payload map[string]any
		status  int
	}{
		{"wrong current", map[string]any{
			"currentPassword": "not the password at all",
			"newPassword":     "a perfectly fine new one",
			"confirmPassword": "a perfectly fine new one",
		}, http.StatusForbidden},
		{"mismatch", map[string]any{
			"currentPassword": testPassword,
			"newPassword":     "a perfectly fine new one",
			"confirmPassword": "a different new one entirely",
		}, http.StatusBadRequest},
		{"too short", map[string]any{
			"currentPassword": testPassword,
			"newPassword":     "short",
			"confirmPassword": "short",
		}, http.StatusBadRequest},
		{"unchanged", map[string]any{
			"currentPassword": testPassword,
			"newPassword":     testPassword,
			"confirmPassword": testPassword,
		}, http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := e.api(http.MethodPost, "/adminpage/api/account/password", c.payload)
			got := body(t, res)
			if res.StatusCode != c.status {
				t.Fatalf("status = %d, want %d (%s)", res.StatusCode, c.status, got)
			}
		})
	}

	// The password is unchanged after every rejection.
	admin, err := e.db.Admin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := auth.VerifyPassword(admin.PasswordHash, testPassword); !ok {
		t.Fatal("a rejected request changed the password")
	}

	// The failed attempt is recorded as a failure, not a success.
	entries, err := e.db.AuditLog(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	failure := false
	for _, entry := range entries {
		if entry.Action == store.AuditPasswordChange && !entry.Success {
			failure = true
		}
	}
	if !failure {
		t.Error("the wrong current password was not recorded as a failed attempt")
	}
}

func TestChangePasswordRequiresCSRF(t *testing.T) {
	e := newEnv(t)
	e.login()
	res := e.apiWithToken(http.MethodPost, "/adminpage/api/account/password", map[string]any{
		"currentPassword": testPassword,
		"newPassword":     "another long enough password",
		"confirmPassword": "another long enough password",
	}, "forged")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", res.StatusCode)
	}
}

func TestRevokeOtherSessions(t *testing.T) {
	e := newEnv(t)
	e.login()
	other := e.newSession()

	if n, err := e.db.CountSessions(context.Background()); err != nil || n != 2 {
		t.Fatalf("sessions before revoke = %d, %v", n, err)
	}

	out := e.apiJSON(http.MethodPost, "/adminpage/api/account/sessions/revoke", map[string]any{}, http.StatusOK)
	if num(t, out, "revokedSessions") != 1 {
		t.Fatalf("revoked = %v, want 1", out["revokedSessions"])
	}

	res := other.api(http.MethodGet, "/adminpage/api/overview", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the revoked session still works: %d", res.StatusCode)
	}
	// The session that issued the revoke survives.
	e.apiJSON(http.MethodGet, "/adminpage/api/overview", nil, http.StatusOK)
}

func TestAuditLogRecordsEveryMutation(t *testing.T) {
	e := newEnv(t)
	e.login()

	link := e.createLink(map[string]any{"code": "001", "targetUrl": "https://example.com/audit"})
	id := itoa(int64(num(t, link, "id")))
	record := e.createQR(map[string]any{"kind": "text", "payload": "audited"})
	qrID := str(t, record, "id")

	e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+id, map[string]any{"enabled": false}, http.StatusOK)
	e.apiJSON(http.MethodPost, "/adminpage/api/shorturls/"+id+"/extend", map[string]any{"duration": "7d"}, http.StatusOK)
	e.apiJSON(http.MethodPost, "/adminpage/api/qrcodes/"+qrID+"/regenerate", map[string]any{}, http.StatusOK)
	e.apiJSON(http.MethodDelete, "/adminpage/api/qrcodes/"+qrID, nil, http.StatusOK)
	e.apiJSON(http.MethodDelete, "/adminpage/api/shorturls/"+id, nil, http.StatusOK)
	e.apiJSON(http.MethodPost, "/adminpage/api/cleanup", map[string]any{}, http.StatusOK)

	entries, err := e.db.AuditLog(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Action] = true
	}
	for _, want := range []string{
		store.AuditLogin,
		store.AuditShortCreate, store.AuditShortUpdate, store.AuditShortExtend, store.AuditShortDelete,
		store.AuditQRCreate, store.AuditQRRegenerate, store.AuditQRDelete,
		store.AuditCleanup,
	} {
		if !seen[want] {
			t.Errorf("the audit log has no %s entry", want)
		}
	}

	// The recorded detail names the subject so the log is actually useful.
	for _, entry := range entries {
		if entry.Action == store.AuditShortCreate && entry.Subject != "/001" {
			t.Errorf("shorturl.create subject = %q", entry.Subject)
		}
		if entry.IP == "" {
			t.Errorf("entry %s has no source address", entry.Action)
		}
	}
}

func TestFailedLoginsAreAudited(t *testing.T) {
	e := newEnv(t)
	page := body(t, e.get("/adminpage/login"))
	token := loginTokenRe.FindStringSubmatch(page)[1]

	for i := 0; i < 3; i++ {
		form := "csrf_token=" + token + "&username=admin&password=wrong+password+here"
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := e.do(req)
		res.Body.Close()
	}

	n, err := e.db.FailedLoginsSince(context.Background(), time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("failed logins recorded = %d, want at least 3", n)
	}

	e.login()
	out := e.apiJSON(http.MethodGet, "/adminpage/api/account", nil, http.StatusOK)
	if num(t, out, "failedLogins24h") < 3 {
		t.Fatalf("the console reports %v failed logins", out["failedLogins24h"])
	}
}

func TestExportFormats(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{"code": "001", "targetUrl": "https://example.com/a", "note": "메모"})
	e.createQR(map[string]any{"kind": "text", "payload": "내보내기", "label": "라벨"})

	for _, kind := range []string{"shorturls", "qrcodes", "audit"} {
		// CSV
		res := e.get("/adminpage/api/export/" + kind + "?format=csv&csrf=" + url.QueryEscape(e.csrf))
		raw := rawBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("csv export of %s = %d: %s", kind, res.StatusCode, raw)
		}
		if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
			t.Errorf("%s csv content type = %q", kind, ct)
		}
		if cd := res.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
			t.Errorf("%s csv is not an attachment: %q", kind, cd)
		}
		text := strings.TrimPrefix(string(raw), "\ufeff")
		rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
		if err != nil {
			t.Fatalf("%s csv does not parse: %v", kind, err)
		}
		if len(rows) < 2 {
			t.Fatalf("%s csv has %d rows, want a header and at least one record", kind, len(rows))
		}

		// JSON
		res = e.get("/adminpage/api/export/" + kind + "?format=json&csrf=" + url.QueryEscape(e.csrf))
		raw = rawBody(t, res)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("json export of %s = %d", kind, res.StatusCode)
		}
		var parsed []map[string]any
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("%s json does not parse: %v", kind, err)
		}
	}

	// Unknown target and format.
	res := e.get("/adminpage/api/export/nope?csrf=" + url.QueryEscape(e.csrf))
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown export target = %d", res.StatusCode)
	}
	res = e.get("/adminpage/api/export/shorturls?format=xlsx&csrf=" + url.QueryEscape(e.csrf))
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown format = %d", res.StatusCode)
	}
}

// TestExportNeutralisesSpreadsheetFormulas keeps a note that starts with "="
// from being evaluated when the CSV is opened in a spreadsheet.
func TestExportNeutralisesSpreadsheetFormulas(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{
		"code": "001", "targetUrl": "https://example.com/x",
		"note": `=HYPERLINK("http://evil.example","click")`,
	})

	res := e.get("/adminpage/api/export/shorturls?format=csv&csrf=" + url.QueryEscape(e.csrf))
	text := strings.TrimPrefix(string(rawBody(t, res)), "\ufeff")
	rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	noteCol := -1
	for i, h := range rows[0] {
		if h == "note" {
			noteCol = i
		}
	}
	if noteCol < 0 {
		t.Fatal("no note column in the export")
	}
	note := rows[1][noteCol]
	if !strings.HasPrefix(note, "'=") {
		t.Fatalf("the formula was not neutralised: %q", note)
	}
}

func TestExportRequiresAuthentication(t *testing.T) {
	e := newEnv(t)
	for _, kind := range []string{"shorturls", "qrcodes", "audit"} {
		res := e.get("/adminpage/api/export/" + kind)
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusUnauthorized {
			t.Errorf("anonymous export of %s = %d", kind, res.StatusCode)
		}
	}
}

// TestExportRequiresCSRFToken stops a cross site link from making the
// administrator's browser download the catalogue.
func TestExportRequiresCSRFToken(t *testing.T) {
	e := newEnv(t)
	e.login()

	for _, path := range []string{
		"/adminpage/api/export/shorturls",
		"/adminpage/api/export/shorturls?csrf=",
		"/adminpage/api/export/shorturls?csrf=forged-token",
	} {
		res := e.get(path)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403", path, res.StatusCode)
		}
	}

	// A cross site request is refused even with the right token.
	req, err := http.NewRequest(http.MethodGet,
		e.url("/adminpage/api/export/shorturls?csrf="+url.QueryEscape(e.csrf)), nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	res := e.do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross site export returned %d, want 403", res.StatusCode)
	}

	// The header form still works, which is what a fetch() call would use.
	res = e.api(http.MethodGet, "/adminpage/api/export/shorturls", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the header form returned %d", res.StatusCode)
	}
}

func TestSecurityHeadersIncludeRobotsTag(t *testing.T) {
	e := newEnv(t)
	res := e.get("/")
	res.Body.Close()
	if got := res.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("X-Robots-Tag = %q", got)
	}
}

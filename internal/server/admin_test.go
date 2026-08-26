package server_test

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/qrdecode"
	"github.com/mvtp/qrurl/internal/store"
)

func TestAdminRequiresAuthentication(t *testing.T) {
	e := newEnv(t)

	// Page routes send the visitor to the login form.
	res := e.get("/adminpage")
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("GET /adminpage = %d, want 303", res.StatusCode)
	}
	if loc := res.Header.Get("Location"); loc != "/adminpage/login" {
		t.Fatalf("redirect location = %q", loc)
	}

	// API routes answer with JSON so the browser code can react.
	endpoints := []struct{ method, path string }{
		{http.MethodGet, "/adminpage/api/overview"},
		{http.MethodGet, "/adminpage/api/shorturls"},
		{http.MethodPost, "/adminpage/api/shorturls"},
		{http.MethodPatch, "/adminpage/api/shorturls/1"},
		{http.MethodDelete, "/adminpage/api/shorturls/1"},
		{http.MethodPost, "/adminpage/api/shorturls/1/extend"},
		{http.MethodPost, "/adminpage/api/shorturls/1/qr"},
		{http.MethodGet, "/adminpage/api/qrcodes"},
		{http.MethodPost, "/adminpage/api/qrcodes"},
		{http.MethodPatch, "/adminpage/api/qrcodes/abc"},
		{http.MethodDelete, "/adminpage/api/qrcodes/abc"},
		{http.MethodPost, "/adminpage/api/qrcodes/abc/extend"},
		{http.MethodPost, "/adminpage/api/qrcodes/abc/regenerate"},
		{http.MethodPost, "/adminpage/api/cleanup"},
	}
	for _, ep := range endpoints {
		res := e.apiWithToken(ep.method, ep.path, map[string]any{}, "forged-token")
		got := body(t, res)
		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d, want 401 (%s)", ep.method, ep.path, res.StatusCode, got)
		}
	}

	// The QR file endpoint is behind the same gate.
	res = e.get("/adminpage/qr/0123456789abcdef.png")
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the QR file endpoint returned %d for an anonymous visitor", res.StatusCode)
	}
}

func TestLoginRejectsBadCredentials(t *testing.T) {
	e := newEnv(t)
	page := body(t, e.get("/adminpage/login"))
	m := loginTokenRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSRF token on the login page")
	}

	attempt := func(username, password string) int {
		form := url.Values{"csrf_token": {m[1]}, "username": {username}, "password": {password}}
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := e.do(req)
		res.Body.Close()
		return res.StatusCode
	}

	if got := attempt(testUsername, "wrong password entirely"); got != http.StatusUnauthorized {
		t.Errorf("a wrong password returned %d, want 401", got)
	}
	if got := attempt("root", testPassword); got != http.StatusUnauthorized {
		t.Errorf("a wrong user name returned %d, want 401", got)
	}
	if got := attempt("", ""); got != http.StatusUnauthorized {
		t.Errorf("empty credentials returned %d, want 401", got)
	}

	// A failed login must not create a session.
	res := e.get("/adminpage")
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatal("a failed login produced a usable session")
	}
}

func TestLoginRateLimit(t *testing.T) {
	e := newEnv(t)
	page := body(t, e.get("/adminpage/login"))
	token := loginTokenRe.FindStringSubmatch(page)[1]

	limited := false
	for i := 0; i < 15; i++ {
		form := url.Values{"csrf_token": {token}, "username": {testUsername}, "password": {"nope nope nope"}}
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := e.do(req)
		res.Body.Close()
		if res.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("repeated failed logins were never rate limited")
	}
}

func TestSessionCookieFlagsAndExpiry(t *testing.T) {
	t.Setenv("QRURL_SESSION_TTL", "30m")
	e := newEnv(t)
	e.login()

	var sessionCookie *http.Cookie
	// The cookie is scoped to /adminpage, so the jar only returns it there.
	u, err := url.Parse(e.ts.URL + "/adminpage")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range e.client.Jar.Cookies(u) {
		if c.Name == "qrurl_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie was issued")
	}
	if sessionCookie.Value == "" {
		t.Fatal("the session cookie is empty")
	}

	// The cookie jar drops the attributes, so inspect the raw Set-Cookie.
	res := e.get("/adminpage/login")
	res.Body.Close()
	fresh := newEnv(t)
	freshPage := body(t, fresh.get("/adminpage/login"))
	token := loginTokenRe.FindStringSubmatch(freshPage)[1]
	form := url.Values{"csrf_token": {token}, "username": {testUsername}, "password": {testPassword}}
	req, _ := http.NewRequest(http.MethodPost, fresh.url("/adminpage/login"), strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRes := fresh.do(req)
	loginRes.Body.Close()
	var raw string
	for _, sc := range loginRes.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "qrurl_session=") {
			raw = sc
		}
	}
	if raw == "" {
		t.Fatal("the login response did not set a session cookie")
	}
	if !strings.Contains(raw, "HttpOnly") {
		t.Errorf("the session cookie is not HttpOnly: %s", raw)
	}
	if !strings.Contains(raw, "SameSite=Lax") {
		t.Errorf("the session cookie has no SameSite attribute: %s", raw)
	}
	if !strings.Contains(raw, "Path=/adminpage") {
		t.Errorf("the session cookie is not scoped to /adminpage: %s", raw)
	}

	// The stored value is only a hash of the cookie.
	var stored string
	if err := e.db.SQL().QueryRow("SELECT token_hash FROM sessions LIMIT 1").Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored, sessionCookie.Value) || stored == sessionCookie.Value {
		t.Fatal("the raw session token is stored in the database")
	}

	// Past the lifetime the session stops working.
	e.clock.Advance(31 * time.Minute)
	res = e.api(http.MethodGet, "/adminpage/api/overview", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an expired session returned %d, want 401", res.StatusCode)
	}
}

func TestLogoutInvalidatesTheSession(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.apiJSON(http.MethodGet, "/adminpage/api/overview", nil, http.StatusOK)

	form := url.Values{"csrf_token": {e.csrf}}
	req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/logout"), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := e.do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout returned %d", res.StatusCode)
	}

	res = e.api(http.MethodGet, "/adminpage/api/overview", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the session still works after logout: %d", res.StatusCode)
	}
	if n, err := e.db.CountSessions(context.Background()); err != nil || n != 0 {
		t.Fatalf("the session row was not deleted: %d, %v", n, err)
	}
}

func TestShortURLManagement(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Creation with automatic numbering.
	first := e.createLink(map[string]any{"targetUrl": "https://example.com/one"})
	if got := str(t, first, "code"); got != "001" {
		t.Fatalf("first automatic code = %q", got)
	}

	// Manual numbering, note and expiry.
	second := e.createLink(map[string]any{
		"code": "42", "targetUrl": "https://example.com/two", "note": "두번째", "expiryPreset": "7d",
	})
	if got := str(t, second, "code"); got != "042" {
		t.Fatalf("manual code = %q, want zero padded 042", got)
	}
	if second["expiresAt"] == nil {
		t.Fatal("the expiry was not stored")
	}
	if got := num(t, second, "remainingSeconds"); got < 6*24*3600 {
		t.Fatalf("remaining seconds = %v", got)
	}

	// Editing.
	id := itoa(int64(num(t, second, "id")))
	out := e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+id, map[string]any{
		"targetUrl": "https://example.com/updated", "note": "수정됨",
	}, http.StatusOK)
	item := out["item"].(map[string]any)
	if str(t, item, "targetUrl") != "https://example.com/updated" || str(t, item, "note") != "수정됨" {
		t.Fatalf("the update was not applied: %v", item)
	}

	// Duplicate codes are refused.
	res := e.api(http.MethodPost, "/adminpage/api/shorturls", map[string]any{
		"code": "042", "targetUrl": "https://example.com/clash",
	})
	res.Body.Close()
	if res.StatusCode != http.StatusConflict {
		t.Fatalf("a duplicate code returned %d, want 409", res.StatusCode)
	}

	// Search and status filters.
	if got := e.listLinks("?q=" + url.QueryEscape("수정됨")); len(got) != 1 {
		t.Fatalf("the note search matched %d rows", len(got))
	}
	if got := e.listLinks("?status=expiring"); len(got) != 0 {
		t.Fatalf("nothing should be expiring yet, got %d", len(got))
	}

	// Deletion.
	e.apiJSON(http.MethodDelete, "/adminpage/api/shorturls/"+id, nil, http.StatusOK)
	if got := e.listLinks(""); len(got) != 1 {
		t.Fatalf("after deletion %d links remain, want 1", len(got))
	}
	res = e.api(http.MethodDelete, "/adminpage/api/shorturls/"+id, nil)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("deleting twice returned %d, want 404", res.StatusCode)
	}
}

func TestShortURLValidationRejectsDangerousSchemes(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, target := range []string{
		"javascript:alert(1)",
		"JAVASCRIPT:alert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
		"ftp://example.com",
		"//example.com",
		"",
		"https://user:pass@example.com",
	} {
		res := e.api(http.MethodPost, "/adminpage/api/shorturls", map[string]any{"targetUrl": target})
		got := body(t, res)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("target %q was accepted with status %d: %s", target, res.StatusCode, got)
		}
	}
	if got := e.listLinks(""); len(got) != 0 {
		t.Fatalf("%d rejected targets were stored anyway", len(got))
	}
}

func TestExpiryChangeAndExtension(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{"targetUrl": "https://example.com", "expiryPreset": "1h"})
	id := itoa(int64(num(t, link, "id")))

	// Extending pushes the deadline out from the current expiry.
	out := e.apiJSON(http.MethodPost, "/adminpage/api/shorturls/"+id+"/extend",
		map[string]any{"duration": "7d"}, http.StatusOK)
	extended := out["item"].(map[string]any)
	if num(t, extended, "remainingSeconds") < 7*24*3600 {
		t.Fatalf("extend did not add to the existing expiry: %v", extended["remainingSeconds"])
	}

	// Setting an explicit expiry.
	out = e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+id,
		map[string]any{"expiryPreset": "custom", "expiryCustom": "2026-08-27T09:30"}, http.StatusOK)
	item := out["item"].(map[string]any)
	if got := str(t, item, "expiresAt"); !strings.HasPrefix(got, "2026-08-27T09:30") {
		t.Fatalf("custom expiry = %q", got)
	}

	// Removing the expiry entirely.
	out = e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+id,
		map[string]any{"expiryPreset": "none"}, http.StatusOK)
	item = out["item"].(map[string]any)
	if item["expiresAt"] != nil {
		t.Fatalf("the expiry was not cleared: %v", item["expiresAt"])
	}

	// Extending an unlimited item starts from now.
	out = e.apiJSON(http.MethodPost, "/adminpage/api/shorturls/"+id+"/extend",
		map[string]any{"duration": "1d"}, http.StatusOK)
	item = out["item"].(map[string]any)
	if got := num(t, item, "remainingSeconds"); got > 24*3600 || got < 23*3600 {
		t.Fatalf("remaining after extending an unlimited link = %v", got)
	}

	// A past custom date and an unknown duration are refused.
	res := e.api(http.MethodPatch, "/adminpage/api/shorturls/"+id,
		map[string]any{"expiryPreset": "custom", "expiryCustom": "2020-01-01T00:00"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("a past expiry returned %d", res.StatusCode)
	}
	res = e.api(http.MethodPost, "/adminpage/api/shorturls/"+id+"/extend", map[string]any{"duration": "1y"})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown extension returned %d", res.StatusCode)
	}
}

func TestQRManagementAllKinds(t *testing.T) {
	e := newEnv(t)
	e.login()

	// Direct URL QR.
	direct := e.createQR(map[string]any{
		"kind": "url", "payload": "https://example.com/abc?a=1&b=2", "label": "직접", "ecLevel": "Q", "size": 512,
	})
	if str(t, direct, "payload") != "https://example.com/abc?a=1&b=2" {
		t.Fatalf("direct payload = %q", direct["payload"])
	}
	if direct["revocable"] != false {
		t.Error("a direct QR must not be marked revocable")
	}

	// Text QR.
	text := e.createQR(map[string]any{"kind": "text", "payload": "가나다 ABC 123", "ecLevel": "H"})
	if str(t, text, "payload") != "가나다 ABC 123" {
		t.Fatalf("text payload = %q", text["payload"])
	}

	// Short URL QR that creates its link on the fly.
	short := e.createQR(map[string]any{
		"kind": "short", "targetUrl": "https://food.hsoc.kr", "newCode": "7",
		"expiryPreset": "30d", "shortExpiryUsing": "qr", "ecLevel": "M", "size": 512,
	})
	if str(t, short, "payload") != testBaseURL+"/007" {
		t.Fatalf("short QR payload = %q, want %s/007", short["payload"], testBaseURL)
	}
	if short["revocable"] != true {
		t.Error("a short URL QR must be marked revocable")
	}
	if str(t, short, "shortCode") != "007" {
		t.Fatalf("linked code = %v", short["shortCode"])
	}

	// The linked short URL exists with the same expiry.
	links := e.listLinks("?q=007")
	if len(links) != 1 {
		t.Fatalf("the short URL was not created: %v", links)
	}
	if links[0]["expiresAt"] == nil {
		t.Fatal("the short URL did not inherit the QR expiry")
	}

	// Short URL QR reusing an existing code.
	reuse := e.createQR(map[string]any{"kind": "short", "shortCode": "007", "label": "재사용"})
	if str(t, reuse, "payload") != testBaseURL+"/007" {
		t.Fatalf("reused payload = %q", reuse["payload"])
	}

	// Every stored image decodes back to its payload.
	for _, record := range e.listQRs("") {
		res := e.get(str(t, record, "pngUrl"))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", record["pngUrl"], res.StatusCode)
		}
		decoded, err := qrdecode.DecodePNG(rawBody(t, res))
		if err != nil {
			t.Fatalf("decode stored png: %v", err)
		}
		if decoded.Text != str(t, record, "payload") {
			t.Fatalf("stored png decodes to %q, want %q", decoded.Text, record["payload"])
		}

		res = e.get(str(t, record, "svgUrl"))
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d", record["svgUrl"], res.StatusCode)
		}
		decodedSVG, err := qrdecode.DecodeSVG(rawBody(t, res))
		if err != nil {
			t.Fatalf("decode stored svg: %v", err)
		}
		if decodedSVG.Text != str(t, record, "payload") {
			t.Fatalf("stored svg decodes to %q", decodedSVG.Text)
		}
	}

	// Filters.
	if got := e.listQRs("?kind=short"); len(got) != 2 {
		t.Fatalf("the short filter matched %d records", len(got))
	}
	if got := e.listQRs("?kind=text"); len(got) != 1 {
		t.Fatalf("the text filter matched %d records", len(got))
	}
	if got := e.listQRs("?q=" + url.QueryEscape("가나다")); len(got) != 1 {
		t.Fatalf("the payload search matched %d records", len(got))
	}
	res := e.api(http.MethodGet, "/adminpage/api/qrcodes?kind=bogus", nil)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Errorf("an unknown kind filter returned %d", res.StatusCode)
	}

	// Invalid input.
	for _, payload := range []map[string]any{
		{"kind": "image", "payload": "x"},
		{"kind": "url", "payload": "javascript:alert(1)"},
		{"kind": "text", "payload": ""},
		{"kind": "short"},
		{"kind": "text", "payload": "x", "ecLevel": "Z"},
		{"kind": "text", "payload": "x", "size": 3},
	} {
		res := e.api(http.MethodPost, "/adminpage/api/qrcodes", payload)
		got := body(t, res)
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %v was accepted with %d: %s", payload, res.StatusCode, got)
		}
	}
}

func TestQRRegenerateAndUpdate(t *testing.T) {
	e := newEnv(t)
	e.login()
	record := e.createQR(map[string]any{"kind": "text", "payload": "regenerate me", "ecLevel": "L", "size": 256})
	id := str(t, record, "id")

	out := e.apiJSON(http.MethodPost, "/adminpage/api/qrcodes/"+id+"/regenerate",
		map[string]any{"ecLevel": "H", "size": 1024}, http.StatusOK)
	item := out["item"].(map[string]any)
	if str(t, item, "ecLevel") != "H" || num(t, item, "size") != 1024 {
		t.Fatalf("regenerate did not apply the new settings: %v", item)
	}

	res := e.get(str(t, item, "pngUrl"))
	decoded, err := qrdecode.DecodePNG(rawBody(t, res))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Text != "regenerate me" {
		t.Fatalf("the regenerated image decodes to %q", decoded.Text)
	}
	if decoded.Level != "H" {
		t.Fatalf("the regenerated image uses level %s, want H", decoded.Level)
	}

	out = e.apiJSON(http.MethodPatch, "/adminpage/api/qrcodes/"+id,
		map[string]any{"label": "새 이름", "expiryPreset": "1d"}, http.StatusOK)
	item = out["item"].(map[string]any)
	if str(t, item, "label") != "새 이름" || item["expiresAt"] == nil {
		t.Fatalf("the QR update was not applied: %v", item)
	}
}

func TestDeletingAQRRemovesItsFiles(t *testing.T) {
	e := newEnv(t)
	e.login()
	record := e.createQR(map[string]any{"kind": "text", "payload": "delete me"})
	id := str(t, record, "id")
	pngURL := str(t, record, "pngUrl")

	pngPath := filepath.Join(e.files.Dir(), id+".png")
	svgPath := filepath.Join(e.files.Dir(), id+".svg")
	for _, p := range []string{pngPath, svgPath} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("the rendered file is missing: %v", err)
		}
	}

	e.apiJSON(http.MethodDelete, "/adminpage/api/qrcodes/"+id, nil, http.StatusOK)

	for _, p := range []string{pngPath, svgPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("the file %s survived the deletion", p)
		}
	}
	res := e.get(pngURL)
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("the deleted QR is still served: %d", res.StatusCode)
	}
}

func TestDeletingAShortURLRemovesItsLinkedQRCodes(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{
		"code": "055", "targetUrl": "https://example.com", "createQr": true, "qrEcLevel": "M", "qrSize": 512,
	})
	id := itoa(int64(num(t, link, "id")))

	records := e.listQRs("")
	if len(records) != 1 {
		t.Fatalf("expected the companion QR, got %d records", len(records))
	}
	qrID := str(t, records[0], "id")

	e.apiJSON(http.MethodDelete, "/adminpage/api/shorturls/"+id, nil, http.StatusOK)

	if got := e.listQRs(""); len(got) != 0 {
		t.Fatalf("%d linked QR records survived", len(got))
	}
	for _, ext := range []string{".png", ".svg"} {
		if _, err := os.Stat(filepath.Join(e.files.Dir(), qrID+ext)); !os.IsNotExist(err) {
			t.Errorf("the %s file of the cascaded QR survived", ext)
		}
	}
}

func TestRenamingACodeRerendersTheLinkedQR(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{
		"code": "011", "targetUrl": "https://example.com", "createQr": true, "qrEcLevel": "M", "qrSize": 512,
	})
	id := itoa(int64(num(t, link, "id")))

	e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+id, map[string]any{"code": "222"}, http.StatusOK)

	records := e.listQRs("")
	if len(records) != 1 {
		t.Fatalf("expected one QR, got %d", len(records))
	}
	if got := str(t, records[0], "payload"); got != testBaseURL+"/222" {
		t.Fatalf("the payload was not rewritten: %q", got)
	}

	res := e.get(str(t, records[0], "pngUrl"))
	decoded, err := qrdecode.DecodePNG(rawBody(t, res))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Text != testBaseURL+"/222" {
		t.Fatalf("the stored image still encodes %q", decoded.Text)
	}
}

func TestCreateQRForAnExistingShortURL(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{"code": "088", "targetUrl": "https://example.com", "expiryPreset": "7d"})
	id := itoa(int64(num(t, link, "id")))

	out := e.apiJSON(http.MethodPost, "/adminpage/api/shorturls/"+id+"/qr",
		map[string]any{"ecLevel": "Q", "size": 768, "expiryUsing": "link", "label": "포스터"}, http.StatusCreated)
	item := out["item"].(map[string]any)
	if str(t, item, "payload") != testBaseURL+"/088" {
		t.Fatalf("payload = %q", item["payload"])
	}
	if str(t, item, "expiresAt") != str(t, link, "expiresAt") {
		t.Fatalf("the QR expiry %v does not mirror the link expiry %v", item["expiresAt"], link["expiresAt"])
	}
	if num(t, item, "size") != 768 || str(t, item, "ecLevel") != "Q" {
		t.Fatalf("the render options were ignored: %v", item)
	}
}

func TestOverviewDashboard(t *testing.T) {
	e := newEnv(t)
	e.login()

	e.createLink(map[string]any{"code": "001", "targetUrl": "https://example.com/a"})
	disabled := e.createLink(map[string]any{"code": "002", "targetUrl": "https://example.com/b"})
	e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+itoa(int64(num(t, disabled, "id"))),
		map[string]any{"enabled": false}, http.StatusOK)
	e.createLink(map[string]any{"code": "003", "targetUrl": "https://example.com/c", "expiryPreset": "1h"})
	e.createQR(map[string]any{"kind": "text", "payload": "dash"})
	e.createQR(map[string]any{"kind": "short", "shortCode": "001"})

	res := e.get("/001")
	res.Body.Close()
	res = e.get("/001")
	res.Body.Close()

	out := e.apiJSON(http.MethodGet, "/adminpage/api/overview", nil, http.StatusOK)
	dash := out["dashboard"].(map[string]any)

	checks := map[string]float64{
		"activeShortURLs":   2,
		"totalShortURLs":    3,
		"disabledShortURLs": 1,
		"totalQRCodes":      2,
		"revocableQRCodes":  1,
		"freeCodes":         996,
		"totalClicks":       2,
	}
	for key, want := range checks {
		if got := num(t, dash, key); got != want {
			t.Errorf("dashboard.%s = %v, want %v", key, got, want)
		}
	}
	today := dash["today"].(map[string]any)
	if num(t, today, "redirects") != 2 {
		t.Errorf("today redirects = %v", today["redirects"])
	}
	if num(t, today, "urlsCreated") != 3 || num(t, today, "qrsCreated") != 2 {
		t.Errorf("today creations = %v / %v", today["urlsCreated"], today["qrsCreated"])
	}

	// The 1 hour link is inside the "expiring soon" window.
	expiring := out["expiringShortUrls"].([]any)
	if len(expiring) != 1 {
		t.Fatalf("expiring short urls = %d, want 1", len(expiring))
	}
	if len(out["recentShortUrls"].([]any)) != 3 {
		t.Errorf("recent short urls = %v", out["recentShortUrls"])
	}
	if len(out["recentQrCodes"].([]any)) != 2 {
		t.Errorf("recent qr codes = %v", out["recentQrCodes"])
	}
	if out["timezone"] != "UTC" {
		t.Errorf("timezone = %v", out["timezone"])
	}
}

// TestAutomaticCleanupEndToEnd walks the whole expiry lifecycle over HTTP.
func TestAutomaticCleanupEndToEnd(t *testing.T) {
	e := newEnv(t)
	e.login()

	link := e.createLink(map[string]any{
		"code": "099", "targetUrl": "https://example.com/temporary", "expiryPreset": "1h",
		"createQr": true, "qrEcLevel": "M", "qrSize": 512,
	})
	if link["expiresAt"] == nil {
		t.Fatal("the link has no expiry")
	}
	survivor := e.createQR(map[string]any{"kind": "text", "payload": "survivor"})
	shortLived := e.createQR(map[string]any{"kind": "text", "payload": "short lived", "expiryPreset": "1h"})

	records := e.listQRs("")
	if len(records) != 3 {
		t.Fatalf("expected three QR records, got %d", len(records))
	}
	filesBefore, err := os.ReadDir(e.files.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(filesBefore) != 6 {
		t.Fatalf("expected six rendered files, got %d", len(filesBefore))
	}

	e.clock.Advance(2 * time.Hour)

	// Before the sweep the QR image is already unreachable.
	res := e.get(str(t, shortLived, "pngUrl"))
	res.Body.Close()
	if res.StatusCode != http.StatusGone {
		t.Fatalf("an expired QR image returned %d, want 410", res.StatusCode)
	}

	out := e.apiJSON(http.MethodPost, "/adminpage/api/cleanup", map[string]any{}, http.StatusOK)
	if out["skipped"] == true {
		t.Fatal("the cleanup was skipped")
	}
	if got := num(t, out, "shortURLs"); got != 1 {
		t.Errorf("cleanup removed %v short urls, want 1", got)
	}
	if got := num(t, out, "qrCodes"); got != 2 {
		t.Errorf("cleanup removed %v qr codes, want 2 (the expired one and the cascaded one)", got)
	}

	if got := e.listLinks(""); len(got) != 0 {
		t.Fatalf("%d expired links survived the sweep", len(got))
	}
	remaining := e.listQRs("")
	if len(remaining) != 1 || str(t, remaining[0], "id") != str(t, survivor, "id") {
		t.Fatalf("the wrong records survived: %v", remaining)
	}

	filesAfter, err := os.ReadDir(e.files.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(filesAfter) != 2 {
		names := make([]string, len(filesAfter))
		for i, f := range filesAfter {
			names[i] = f.Name()
		}
		t.Fatalf("the QR directory still holds %v, want only the survivor's two files", names)
	}

	// The survivor is untouched and still decodes.
	res = e.get(str(t, survivor, "pngUrl"))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the surviving QR returned %d", res.StatusCode)
	}
	decoded, err := qrdecode.DecodePNG(rawBody(t, res))
	if err != nil {
		t.Fatalf("decode the surviving QR: %v", err)
	}
	if decoded.Text != "survivor" {
		t.Fatalf("the surviving QR decodes to %q", decoded.Text)
	}
}

func TestCleanupIsIdempotentOverHTTP(t *testing.T) {
	e := newEnv(t)
	e.login()
	for i := 0; i < 3; i++ {
		out := e.apiJSON(http.MethodPost, "/adminpage/api/cleanup", map[string]any{}, http.StatusOK)
		if out["shortURLs"] != float64(0) || out["qrCodes"] != float64(0) {
			t.Fatalf("run %d removed something from an empty database: %v", i, out)
		}
	}
	status, err := e.db.CleanupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Runs < 3 {
		t.Errorf("cleanup runs recorded = %d", status.Runs)
	}
}

func TestQRFileAccessIsScopedToTheRecord(t *testing.T) {
	e := newEnv(t)
	e.login()
	record := e.createQR(map[string]any{"kind": "text", "payload": "scoped"})
	id := str(t, record, "id")

	// An identifier that exists but with the wrong extension, and identifiers
	// that do not exist at all.
	for _, name := range []string{
		id + ".gif",
		id + ".txt",
		"ffffffffffffffffffffffff.png",
		"..%2f..%2fetc%2fpasswd",
		"short.png",
	} {
		res := e.get("/adminpage/qr/" + name)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("GET /adminpage/qr/%s was served", name)
		}
	}

	// A record whose file was removed behind the server's back reports 404
	// rather than a stack trace.
	if err := os.Remove(filepath.Join(e.files.Dir(), id+".png")); err != nil {
		t.Fatal(err)
	}
	res := e.get("/adminpage/qr/" + id + ".png")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a missing file returned %d, want 404", res.StatusCode)
	}
}

func TestStoreAndServerAgreeOnKinds(t *testing.T) {
	// Guards against a rename drifting between the two layers.
	if store.KindURL != "url" || store.KindText != "text" || store.KindShort != "short" {
		t.Fatal("the QR kind constants changed without updating the API contract")
	}
}

// TestListingsReportTheirTotal guards against a page silently hiding rows.
func TestListingsReportTheirTotal(t *testing.T) {
	e := newEnv(t)
	e.login()

	for i := 0; i < 3; i++ {
		e.createLink(map[string]any{"targetUrl": "https://example.com/x"})
		e.createQR(map[string]any{"kind": "text", "payload": "record"})
	}

	out := e.apiJSON(http.MethodGet, "/adminpage/api/shorturls", nil, http.StatusOK)
	if num(t, out, "total") != 3 {
		t.Errorf("short url total = %v, want 3", out["total"])
	}
	if out["truncated"] != false {
		t.Errorf("short urls reported as truncated: %v", out["truncated"])
	}

	out = e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes", nil, http.StatusOK)
	if num(t, out, "total") != 3 {
		t.Errorf("qr total = %v, want 3", out["total"])
	}
	if out["truncated"] != false {
		t.Errorf("qr codes reported as truncated: %v", out["truncated"])
	}

	// The total follows the filter, not the whole table.
	out = e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes?kind=short", nil, http.StatusOK)
	if num(t, out, "total") != 0 {
		t.Errorf("filtered total = %v, want 0", out["total"])
	}
}

// TestQRPaginationWalksTheWholeLibrary checks the offset parameter, which the
// console uses for its "load more" button.
func TestQRPaginationWalksTheWholeLibrary(t *testing.T) {
	e := newEnv(t)
	e.login()

	const count = 7
	for i := 0; i < count; i++ {
		e.createQR(map[string]any{"kind": "text", "payload": "page " + itoa(int64(i))})
	}

	// Walking the offsets must visit every record exactly once.
	seen := map[string]bool{}
	for _, offset := range []int{0, 3, 5} {
		out := e.apiJSON(http.MethodGet,
			"/adminpage/api/qrcodes?offset="+itoa(int64(offset)), nil, http.StatusOK)
		if num(t, out, "total") != count {
			t.Fatalf("total at offset %d = %v", offset, out["total"])
		}
		if num(t, out, "offset") != float64(offset) {
			t.Fatalf("echoed offset = %v, want %d", out["offset"], offset)
		}
		items := toItems(t, out)
		if want := count - offset; len(items) != want {
			t.Fatalf("offset %d returned %d records, want %d", offset, len(items), want)
		}
		for _, item := range items {
			seen[str(t, item, "id")] = true
		}
	}
	if len(seen) != count {
		t.Fatalf("the pages covered %d distinct records, want %d", len(seen), count)
	}

	// A silly offset does not error out.
	out := e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes?offset=-5", nil, http.StatusOK)
	if len(toItems(t, out)) != count {
		t.Error("a negative offset should be treated as zero")
	}
	out = e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes?offset=abc", nil, http.StatusOK)
	if len(toItems(t, out)) != count {
		t.Error("an unparsable offset should be treated as zero")
	}
	out = e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes?offset=999", nil, http.StatusOK)
	if len(toItems(t, out)) != 0 {
		t.Error("an offset past the end should return nothing")
	}
}

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// mutations is the set of state changing endpoints that must all be protected.
func mutations(id, qrID string) []struct{ method, path string } {
	return []struct{ method, path string }{
		{http.MethodPost, "/adminpage/api/shorturls"},
		{http.MethodPatch, "/adminpage/api/shorturls/" + id},
		{http.MethodDelete, "/adminpage/api/shorturls/" + id},
		{http.MethodPost, "/adminpage/api/shorturls/" + id + "/extend"},
		{http.MethodPost, "/adminpage/api/shorturls/" + id + "/qr"},
		{http.MethodPost, "/adminpage/api/qrcodes"},
		{http.MethodPatch, "/adminpage/api/qrcodes/" + qrID},
		{http.MethodDelete, "/adminpage/api/qrcodes/" + qrID},
		{http.MethodPost, "/adminpage/api/qrcodes/" + qrID + "/extend"},
		{http.MethodPost, "/adminpage/api/qrcodes/" + qrID + "/regenerate"},
		{http.MethodPost, "/adminpage/api/cleanup"},
	}
}

func TestCSRFTokenIsRequiredForEveryMutation(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{"code": "500", "targetUrl": "https://example.com"})
	record := e.createQR(map[string]any{"kind": "text", "payload": "csrf"})
	id := itoa(int64(num(t, link, "id")))
	qrID := str(t, record, "id")

	for _, m := range mutations(id, qrID) {
		// No token at all.
		res := e.apiWithToken(m.method, m.path, map[string]any{}, "")
		got := body(t, res)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s without a token = %d, want 403 (%s)", m.method, m.path, res.StatusCode, got)
		}

		// A token that is not this session's.
		res = e.apiWithToken(m.method, m.path, map[string]any{}, "not-the-right-token")
		got = body(t, res)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s with a forged token = %d, want 403 (%s)", m.method, m.path, res.StatusCode, got)
		}
	}

	// Nothing was created, changed or deleted along the way.
	if links := e.listLinks(""); len(links) != 1 {
		t.Fatalf("the short URL list changed: %d rows", len(links))
	}
	if records := e.listQRs(""); len(records) != 1 {
		t.Fatalf("the QR list changed: %d rows", len(records))
	}
}

func TestCSRFRejectsCrossSiteRequests(t *testing.T) {
	e := newEnv(t)
	e.login()

	payload, err := json.Marshal(map[string]any{"targetUrl": "https://evil.example/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range []map[string]string{
		{"Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "cross-site"},
		{"Sec-Fetch-Site": "same-site"},
	} {
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/api/shorturls"), bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", e.csrf) // even a valid token must not help
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		res := e.do(req)
		got := body(t, res)
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("a request with %v returned %d, want 403 (%s)", headers, res.StatusCode, got)
		}
	}
	if links := e.listLinks(""); len(links) != 0 {
		t.Fatalf("a cross site request created %d links", len(links))
	}
}

func TestLoginFormRequiresItsOwnCSRFToken(t *testing.T) {
	e := newEnv(t)
	// Fetch the page so the double submit cookie is set, then post the wrong
	// value.
	body(t, e.get("/adminpage/login"))

	for _, token := range []string{"", "wrong-token"} {
		form := url.Values{"csrf_token": {token}, "username": {testUsername}, "password": {testPassword}}
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res := e.do(req)
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("a login with token %q returned %d, want 403", token, res.StatusCode)
		}
	}

	// A cross site login post is refused as well.
	page := body(t, e.get("/adminpage/login"))
	valid := loginTokenRe.FindStringSubmatch(page)[1]
	form := url.Values{"csrf_token": {valid}, "username": {testUsername}, "password": {testPassword}}
	req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	res := e.do(req)
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a cross site login returned %d, want 403", res.StatusCode)
	}
}

// TestXSSPayloadsAreNeverExecutable stores hostile strings and checks that
// nothing comes back as live markup.
func TestXSSPayloadsAreNeverExecutable(t *testing.T) {
	e := newEnv(t)
	e.login()

	hostile := []string{
		`<script>alert('xss')</script>`,
		`"><script>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`</textarea><svg/onload=alert(1)>`,
		`javascript:alert(1)`,
		`<!--<script>alert(1)</script>-->`,
		`{{.}}`,
		" alert(1) ",
	}

	for i, payload := range hostile {
		// As a note on a short URL.
		e.createLink(map[string]any{
			"targetUrl": "https://example.com/safe", "note": payload,
		})
		// As the content of a text QR and as its label.
		e.createQR(map[string]any{"kind": "text", "payload": payload, "label": payload})
		_ = i
	}

	// The JSON API returns the values verbatim, but as JSON strings.
	res := e.api(http.MethodGet, "/adminpage/api/qrcodes", nil)
	raw := rawBody(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("listing failed: %s", raw)
	}
	if ct := res.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("the API must not be served as HTML: %q", ct)
	}
	if bytes.Contains(raw, []byte("<script>")) || bytes.Contains(raw, []byte("</script>")) {
		t.Fatalf("the JSON response contains raw script tags: %s", raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("the response is not valid JSON: %v", err)
	}
	items := toItems(t, decoded)
	if len(items) != len(hostile) {
		t.Fatalf("stored %d records, want %d", len(items), len(hostile))
	}
	found := map[string]bool{}
	for _, item := range items {
		found[str(t, item, "payload")] = true
	}
	for _, payload := range hostile {
		if !found[payload] {
			t.Errorf("the payload %q was altered on the way through", payload)
		}
	}

	// The rendered HTML pages never inline any of it.
	for _, path := range []string{"/", "/adminpage", "/adminpage/login"} {
		page := body(t, e.get(path))
		for _, payload := range hostile {
			if strings.Contains(page, payload) && strings.Contains(payload, "<") {
				t.Errorf("page %s contains the unescaped payload %q", path, payload)
			}
		}
	}

	// The notice pages escape whatever ends up in them.
	page := body(t, e.get("/999"))
	if strings.Contains(page, "<script>alert") {
		t.Error("the notice page rendered live markup")
	}
	if !strings.Contains(page, "찾을 수 없습니다") {
		t.Error("the notice page did not render its message")
	}
}

// TestTemplateEscapingOfServerRenderedValues checks the one place where server
// data really is written into HTML: the CSRF token attribute.
func TestTemplateEscapingOfServerRenderedValues(t *testing.T) {
	e := newEnv(t)
	e.login()
	page := body(t, e.get("/adminpage"))

	m := adminTokenRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no CSRF token in the admin page")
	}
	if html.UnescapeString(m[1]) != e.csrf {
		t.Fatal("the CSRF token was mangled")
	}
	// The admin shell must not carry inline event handlers or script blocks,
	// which is what the content security policy assumes.
	if strings.Contains(page, "onclick=") || strings.Contains(page, "onerror=") {
		t.Error("the admin page contains inline event handlers")
	}
	if strings.Contains(page, "<script>") {
		t.Error("the admin page contains an inline script block")
	}
}

// TestSQLInjectionThroughTheAPI feeds injection payloads into every parameter
// that reaches a query.
func TestSQLInjectionThroughTheAPI(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{"code": "001", "targetUrl": "https://example.com/one"})
	e.createLink(map[string]any{"code": "002", "targetUrl": "https://example.com/two"})
	e.createQR(map[string]any{"kind": "text", "payload": "keep me"})

	payloads := []string{
		"' OR '1'='1",
		"'; DROP TABLE short_urls; --",
		"'; DELETE FROM qr_codes; --",
		"1' UNION SELECT password_hash FROM admin --",
		"\" OR \"\"=\"",
		"1; UPDATE admin SET password_hash='x'",
		"%' --",
		"') OR ('1'='1",
	}

	for _, p := range payloads {
		q := url.Values{"q": {p}, "status": {p}, "sort": {p}, "order": {p}, "kind": {p}}
		// Unknown status and kind values are rejected outright; the query and
		// sort parameters must simply find nothing.
		for _, path := range []string{
			"/adminpage/api/shorturls?q=" + url.QueryEscape(p),
			"/adminpage/api/shorturls?sort=" + url.QueryEscape(p),
			"/adminpage/api/shorturls?order=" + url.QueryEscape(p),
			"/adminpage/api/qrcodes?q=" + url.QueryEscape(p),
			"/adminpage/api/qrcodes?sort=" + url.QueryEscape(p),
		} {
			res := e.api(http.MethodGet, path, nil)
			got := body(t, res)
			if res.StatusCode != http.StatusOK {
				t.Errorf("GET %s = %d: %s", path, res.StatusCode, got)
			}
		}
		res := e.api(http.MethodGet, "/adminpage/api/shorturls?status="+url.QueryEscape(p), nil)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("an injected status filter returned %d, want 400", res.StatusCode)
		}
		_ = q

		// Path identifiers.
		for _, path := range []string{
			"/adminpage/api/shorturls/" + url.PathEscape(p),
			"/adminpage/api/qrcodes/" + url.PathEscape(p),
		} {
			res := e.api(http.MethodPatch, path, map[string]any{"note": "x"})
			res.Body.Close()
			if res.StatusCode != http.StatusBadRequest && res.StatusCode != http.StatusNotFound {
				t.Errorf("PATCH %s = %d, want 400 or 404", path, res.StatusCode)
			}
		}

		// Body fields.
		res = e.api(http.MethodPost, "/adminpage/api/shorturls", map[string]any{
			"targetUrl": "https://example.com/x", "code": p, "note": p,
		})
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("an injected code returned %d, want 400", res.StatusCode)
		}
	}

	// Everything survived.
	if links := e.listLinks(""); len(links) != 2 {
		t.Fatalf("the short URL table holds %d rows, want 2", len(links))
	}
	if records := e.listQRs(""); len(records) != 1 {
		t.Fatalf("the QR table holds %d rows, want 1", len(records))
	}
	admin, err := e.db.Admin(context.Background())
	if err != nil {
		t.Fatalf("the admin row is damaged: %v", err)
	}
	if !strings.HasPrefix(admin.PasswordHash, "$argon2id$") {
		t.Fatalf("the admin password hash was overwritten: %q", admin.PasswordHash)
	}
}

func TestRequestBodyIsLimited(t *testing.T) {
	e := newEnv(t)
	e.login()

	huge := strings.Repeat("a", int(e.cfg.MaxBodyBytes)+4096)
	res := e.api(http.MethodPost, "/adminpage/api/shorturls", map[string]any{
		"targetUrl": "https://example.com", "note": huge,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusRequestEntityTooLarge && res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an oversized body returned %d, want 413 or 400", res.StatusCode)
	}
	if links := e.listLinks(""); len(links) != 0 {
		t.Fatalf("an oversized request created %d rows", len(links))
	}
}

func TestUnknownJSONFieldsAreRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	res := e.api(http.MethodPost, "/adminpage/api/shorturls", map[string]any{
		"targetUrl": "https://example.com", "isAdmin": true,
	})
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown field returned %d, want 400", res.StatusCode)
	}
}

func TestMalformedJSONIsRejected(t *testing.T) {
	e := newEnv(t)
	e.login()
	for _, raw := range []string{"", "{", "[]", `{"targetUrl": }`, `{"targetUrl":"https://example.com"}{"a":1}`} {
		req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/api/shorturls"), strings.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", e.csrf)
		res := e.do(req)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("body %q returned %d, want 400", raw, res.StatusCode)
		}
	}
}

func TestStaticAssetsAreServedSafely(t *testing.T) {
	e := newEnv(t)

	res := e.get("/static/app.css")
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /static/app.css = %d", res.StatusCode)
	}
	if _, err := io.Copy(io.Discard, res.Body); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"/static/../internal/server/server.go",
		"/static/does-not-exist.js",
		"/static/",
	} {
		res := e.get(path)
		res.Body.Close()
		if res.StatusCode == http.StatusOK {
			t.Errorf("GET %s was served", path)
		}
	}
}

func TestRedirectDoesNotLeakTheSessionCookie(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{"code": "600", "targetUrl": "https://example.com"})

	req, err := http.NewRequest(http.MethodGet, e.url("/600"), nil)
	if err != nil {
		t.Fatal(err)
	}
	res := e.do(req)
	res.Body.Close()
	// The cookie is scoped to /adminpage, so the redirect endpoint never sees
	// it and can never reflect it.
	if got := req.Header.Get("Cookie"); strings.Contains(got, "qrurl_session") {
		t.Fatalf("the session cookie was sent to the public redirect: %q", got)
	}
}

// TestRedirectsAreRateLimited caps how fast a single address can walk the
// 001-999 space, which is the only practical mitigation for a code range that
// is enumerable by design.
func TestRedirectsAreRateLimited(t *testing.T) {
	t.Setenv("QRURL_REDIRECT_RATE", "20")
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{"code": "001", "targetUrl": "https://example.com/rl"})

	limited := 0
	served := 0
	for i := 0; i < 60; i++ {
		res := e.get("/001")
		res.Body.Close()
		switch res.StatusCode {
		case http.StatusFound:
			served++
		case http.StatusTooManyRequests:
			limited++
		default:
			t.Fatalf("unexpected status %d on request %d", res.StatusCode, i)
		}
	}
	if limited == 0 {
		t.Fatal("a sweep of the code space was never rate limited")
	}
	if served > 25 {
		t.Fatalf("%d redirects were served before the limit kicked in", served)
	}
	// Missing codes consume budget too, so enumeration cannot dodge the limit
	// by only probing codes that do not exist.
	res := e.get("/777")
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a probe of a missing code returned %d while limited", res.StatusCode)
	}
}

// TestPublicQRAllowList restricts the anonymous generator to given networks.
func TestPublicQRAllowList(t *testing.T) {
	t.Setenv("QRURL_PUBLIC_QR_ALLOW", "10.0.0.0/24")
	e := newEnv(t)

	// The test client connects from the loopback, which is outside the list.
	res := e.get("/qr.png?data=" + url.QueryEscape("https://example.com"))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("an address outside the allow list got %d, want 403", res.StatusCode)
	}

	// The administrator preview is unaffected: it has its own gate.
	e.login()
	res = e.get("/adminpage/qr/preview.png?data=" + url.QueryEscape("https://example.com"))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the admin preview returned %d", res.StatusCode)
	}
}

func TestPublicQRAllowListEmptyMeansEveryone(t *testing.T) {
	e := newEnv(t)
	res := e.get("/qr.png?data=" + url.QueryEscape("https://example.com"))
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("with no allow list the generator returned %d", res.StatusCode)
	}
}

// TestShortURLPagination mirrors the QR listing: the console must never hide
// rows without saying so.
func TestShortURLPagination(t *testing.T) {
	e := newEnv(t)
	e.login()
	const count = 6
	for i := 0; i < count; i++ {
		e.createLink(map[string]any{"targetUrl": "https://example.com/p"})
	}

	seen := map[string]bool{}
	for _, offset := range []int{0, 2, 4} {
		out := e.apiJSON(http.MethodGet,
			"/adminpage/api/shorturls?offset="+itoa(int64(offset)), nil, http.StatusOK)
		if num(t, out, "total") != count {
			t.Fatalf("total at offset %d = %v", offset, out["total"])
		}
		items := toItems(t, out)
		if want := count - offset; len(items) != want {
			t.Fatalf("offset %d returned %d rows, want %d", offset, len(items), want)
		}
		for _, item := range items {
			seen[str(t, item, "code")] = true
		}
	}
	if len(seen) != count {
		t.Fatalf("the pages covered %d distinct codes, want %d", len(seen), count)
	}
}

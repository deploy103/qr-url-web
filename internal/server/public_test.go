package server_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/qrdecode"
)

func TestHomePageAndHealth(t *testing.T) {
	e := newEnv(t)

	res := e.get("/")
	page := body(t, res)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", res.StatusCode)
	}
	if !strings.Contains(page, "QR 코드 생성기") {
		t.Error("the landing page does not render its heading")
	}
	// The difference between revocable and non revocable QR codes must be
	// stated in the interface, not only in the documentation.
	if !strings.Contains(page, "Short URL QR") || !strings.Contains(page, "무효화할 수 없습니다") {
		t.Error("the landing page does not explain that downloaded direct QR codes cannot be revoked")
	}

	res = e.get("/health")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /health = %d", res.StatusCode)
	}
	health := body(t, res)
	if !strings.Contains(health, `"status":"ok"`) || !strings.Contains(health, `"sqlite"`) {
		t.Fatalf("unexpected health payload: %s", health)
	}

	res = e.get("/robots.txt")
	if got := body(t, res); !strings.Contains(got, "Disallow: /") {
		t.Errorf("robots.txt = %q", got)
	}
}

func TestSecurityHeaders(t *testing.T) {
	e := newEnv(t)
	res := e.get("/")
	defer res.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Strict-Transport-Security":    "max-age=31536000; includeSubDomains",
	}
	for header, value := range want {
		if got := res.Header.Get(header); got != value {
			t.Errorf("%s = %q, want %q", header, got, value)
		}
	}
	csp := res.Header.Get("Content-Security-Policy")
	for _, directive := range []string{"default-src 'none'", "script-src 'self'", "frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, directive) {
			t.Errorf("the content security policy is missing %q: %s", directive, csp)
		}
	}
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("the content security policy allows inline code: %s", csp)
	}
}

// TestPublicQRRoundTrip is the requirement stated in the task: the exact input
// must come back out of the generated image.
func TestPublicQRRoundTrip(t *testing.T) {
	e := newEnv(t)
	payloads := []string{
		"https://example.com/abc?a=1&b=2",
		"https://food.hsoc.kr",
		"단순 텍스트 QR 입니다",
		"https://example.com/test",
	}
	for _, payload := range payloads {
		q := url.Values{"data": {payload}, "ec": {"M"}, "size": {"512"}}
		res := e.get("/qr.png?" + q.Encode())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /qr.png = %d for %q", res.StatusCode, payload)
		}
		if ct := res.Header.Get("Content-Type"); ct != "image/png" {
			t.Errorf("content type = %q", ct)
		}
		decoded, err := qrdecode.DecodePNG(rawBody(t, res))
		if err != nil {
			t.Fatalf("decode png for %q: %v", payload, err)
		}
		if decoded.Text != payload {
			t.Fatalf("png decoded %q, want %q", decoded.Text, payload)
		}

		res = e.get("/qr.svg?" + q.Encode())
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET /qr.svg = %d", res.StatusCode)
		}
		decodedSVG, err := qrdecode.DecodeSVG(rawBody(t, res))
		if err != nil {
			t.Fatalf("decode svg for %q: %v", payload, err)
		}
		if decodedSVG.Text != payload {
			t.Fatalf("svg decoded %q, want %q", decodedSVG.Text, payload)
		}
	}
}

func TestPublicQRDownloadAndValidation(t *testing.T) {
	e := newEnv(t)

	q := url.Values{"data": {"https://example.com"}, "download": {"1"}, "name": {`../../etc/passwd"; rm -rf /`}}
	res := e.get("/qr.png?" + q.Encode())
	defer res.Body.Close()
	cd := res.Header.Get("Content-Disposition")
	const prefix = `attachment; filename="`
	if !strings.HasPrefix(cd, prefix) {
		t.Fatalf("content disposition = %q", cd)
	}
	// Neither form may contain anything that could break out of the header.
	for _, bad := range []string{"\n", "\r", "..", "/", "\\"} {
		if strings.Contains(cd, bad) {
			t.Fatalf("the content disposition %q still contains %q", cd, bad)
		}
	}
	rest := strings.TrimPrefix(cd, prefix)
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("the ascii filename is not quoted: %q", cd)
	}
	filename := rest[:end]
	for _, bad := range []string{";", `"`, " "} {
		if strings.Contains(filename, bad) {
			t.Fatalf("the download filename %q still contains %q", filename, bad)
		}
	}
	if !strings.HasSuffix(filename, ".png") {
		t.Fatalf("the download filename lost its extension: %q", filename)
	}

	// A Korean name survives in the RFC 5987 form, percent encoded.
	q = url.Values{"data": {"https://example.com"}, "download": {"1"}, "name": {"급식 포스터"}}
	res = e.get("/qr.png?" + q.Encode())
	res.Body.Close()
	cd = res.Header.Get("Content-Disposition")
	if !strings.Contains(cd, "filename*=UTF-8''") {
		t.Fatalf("no RFC 5987 filename: %q", cd)
	}
	if !strings.Contains(cd, `filename="qrcode.png"`) {
		t.Fatalf("no ascii fallback for a non ascii name: %q", cd)
	}
	if strings.ContainsAny(cd, "\n\r") {
		t.Fatalf("the header contains a line break: %q", cd)
	}

	bad := []string{
		"/qr.png",
		"/qr.png?data=",
		"/qr.png?data=x&ec=Z",
		"/qr.png?data=x&size=1",
		"/qr.png?data=x&size=99999",
		"/qr.png?data=" + url.QueryEscape(strings.Repeat("a", 3000)),
	}
	for _, path := range bad {
		res := e.get(path)
		res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("GET %s = %d, want 400", path, res.StatusCode)
		}
	}
}

func TestPublicQRIsNotPersisted(t *testing.T) {
	e := newEnv(t)
	res := e.get("/qr.png?data=" + url.QueryEscape("https://example.com/not-saved"))
	res.Body.Close()

	e.login()
	if items := e.listQRs(""); len(items) != 0 {
		t.Fatalf("the public generator stored %d records", len(items))
	}
}

func TestRedirectHappyPath(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{
		"code": "123", "targetUrl": "https://example.com/test", "expiryPreset": "none",
	})
	if got := str(t, link, "code"); got != "123" {
		t.Fatalf("code = %q", got)
	}
	if got := str(t, link, "shortUrl"); got != testBaseURL+"/123" {
		t.Fatalf("short url = %q", got)
	}

	for i := 1; i <= 3; i++ {
		res := e.get("/123")
		res.Body.Close()
		if res.StatusCode != http.StatusFound {
			t.Fatalf("redirect %d = %d, want 302", i, res.StatusCode)
		}
		if loc := res.Header.Get("Location"); loc != "https://example.com/test" {
			t.Fatalf("location = %q", loc)
		}
		if cc := res.Header.Get("Cache-Control"); cc != "no-store" {
			t.Errorf("redirects must not be cached, got %q", cc)
		}
	}

	items := e.listLinks("")
	if len(items) != 1 {
		t.Fatalf("expected one link, got %d", len(items))
	}
	if got := num(t, items[0], "clickCount"); got != 3 {
		t.Fatalf("click count = %v, want 3", got)
	}
	if items[0]["lastAccessedAt"] == nil {
		t.Error("the last access time was not recorded")
	}
}

func TestRedirectRejectsUnknownDisabledAndDeleted(t *testing.T) {
	e := newEnv(t)
	e.login()
	link := e.createLink(map[string]any{"code": "200", "targetUrl": "https://example.com/x"})
	id := int64(num(t, link, "id"))

	// Codes outside the documented shape never reach the database.
	for _, path := range []string{"/000", "/00", "/1234", "/abc", "/12a", "/999"} {
		res := e.get(path)
		res.Body.Close()
		if res.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, res.StatusCode)
		}
	}

	// Disabled.
	e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+itoa(id), map[string]any{"enabled": false}, http.StatusOK)
	res := e.get("/200")
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("a disabled link returned %d, want 403", res.StatusCode)
	}

	// Re-enabled.
	e.apiJSON(http.MethodPatch, "/adminpage/api/shorturls/"+itoa(id), map[string]any{"enabled": true}, http.StatusOK)
	res = e.get("/200")
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("a re-enabled link returned %d, want 302", res.StatusCode)
	}

	// Deleted.
	e.apiJSON(http.MethodDelete, "/adminpage/api/shorturls/"+itoa(id), nil, http.StatusOK)
	res = e.get("/200")
	res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Fatalf("a deleted link returned %d, want 404", res.StatusCode)
	}
}

// TestExpiredLinkIsBlockedBeforeCleanup is the core expiry requirement: the
// redirect must fail the moment the deadline passes, without waiting for the
// background sweep.
func TestExpiredLinkIsBlockedBeforeCleanup(t *testing.T) {
	e := newEnv(t)
	e.login()
	e.createLink(map[string]any{"code": "300", "targetUrl": "https://example.com/temp", "expiryPreset": "1h"})

	res := e.get("/300")
	res.Body.Close()
	if res.StatusCode != http.StatusFound {
		t.Fatalf("a live link returned %d", res.StatusCode)
	}

	// Move past the expiry without running any cleanup.
	e.clock.Advance(61 * time.Minute)

	res = e.get("/300")
	page := body(t, res)
	if res.StatusCode != http.StatusGone {
		t.Fatalf("an expired link returned %d, want 410", res.StatusCode)
	}
	if !strings.Contains(page, "만료") {
		t.Errorf("the expiry page does not explain the situation: %s", page)
	}

	// The redirect path also removes the lapsed row immediately.
	if items := e.listLinks(""); len(items) != 0 {
		t.Fatalf("the expired link is still stored: %v", items)
	}
}

func TestPublicQRCanBeDisabledWhileTheAdminPreviewKeepsWorking(t *testing.T) {
	t.Setenv("QRURL_PUBLIC_QR", "false")
	e := newEnv(t)

	res := e.get("/qr.png?data=" + url.QueryEscape("https://example.com"))
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("the public renderer returned %d, want 403 when it is disabled", res.StatusCode)
	}
	page := body(t, e.get("/"))
	if !strings.Contains(page, "공개 QR 생성이 꺼져 있습니다") {
		t.Error("the landing page does not say the generator is off")
	}

	// The administrator preview is a separate, authenticated endpoint.
	res = e.get("/adminpage/qr/preview.png?data=x")
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("the preview endpoint returned %d for an anonymous visitor", res.StatusCode)
	}

	e.login()
	const payload = "https://url.mvtp.cloud/001"
	res = e.get("/adminpage/qr/preview.png?data=" + url.QueryEscape(payload) + "&ec=H&size=256")
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the admin preview returned %d", res.StatusCode)
	}
	decoded, err := qrdecode.DecodePNG(rawBody(t, res))
	if err != nil {
		t.Fatalf("decode the admin preview: %v", err)
	}
	if decoded.Text != payload {
		t.Fatalf("the admin preview decodes to %q", decoded.Text)
	}
	if decoded.Level != "H" {
		t.Fatalf("the admin preview ignored the error correction level: %s", decoded.Level)
	}

	res = e.get("/adminpage/qr/preview.svg?data=" + url.QueryEscape(payload))
	if res.StatusCode != http.StatusOK {
		t.Fatalf("the svg preview returned %d", res.StatusCode)
	}
	decodedSVG, err := qrdecode.DecodeSVG(rawBody(t, res))
	if err != nil {
		t.Fatalf("decode the svg preview: %v", err)
	}
	if decodedSVG.Text != payload {
		t.Fatalf("the svg preview decodes to %q", decodedSVG.Text)
	}

	// The preview never persists anything.
	if items := e.listQRs(""); len(items) != 0 {
		t.Fatalf("the preview stored %d records", len(items))
	}

	// Bad input is still rejected.
	res = e.get("/adminpage/qr/preview.png?data=")
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("an empty preview payload returned %d", res.StatusCode)
	}
}

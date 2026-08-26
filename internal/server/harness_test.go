package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deploy103/qr-url-web/internal/auth"
	"github.com/deploy103/qr-url-web/internal/cleanup"
	"github.com/deploy103/qr-url-web/internal/config"
	"github.com/deploy103/qr-url-web/internal/qrfile"
	"github.com/deploy103/qr-url-web/internal/server"
	"github.com/deploy103/qr-url-web/internal/store"
)

const (
	testUsername = "admin"
	testPassword = "correct horse battery staple"
	testBaseURL  = "https://url.mvtp.cloud"
)

// clock is a controllable time source shared by the server and the cleaner.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t       *testing.T
	ts      *httptest.Server
	cfg     *config.Config
	db      *store.DB
	files   *qrfile.Store
	cleaner *cleanup.Cleaner
	clock   *clock
	client  *http.Client
	csrf    string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("QRURL_DATA_DIR", dir)
	t.Setenv("QRURL_BASE_URL", testBaseURL)
	t.Setenv("QRURL_SECURE_COOKIE", "false")
	t.Setenv("QRURL_HSTS", "true")
	t.Setenv("QRURL_TIMEZONE", "UTC")
	t.Setenv("QRURL_TRUST_PROXY", "false")
	if _, ok := os.LookupEnv("QRURL_PUBLIC_QR"); !ok {
		t.Setenv("QRURL_PUBLIC_QR", "true")
	}
	// A long session keeps the clock manipulation in the expiry tests from
	// logging the harness out; TestSessionExpiry sets its own short lifetime.
	if _, ok := os.LookupEnv("QRURL_SESSION_TTL"); !ok {
		t.Setenv("QRURL_SESSION_TTL", "720h")
	}
	t.Setenv("QRURL_CLEANUP_INTERVAL", "1h")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	db, err := store.Open(cfg.DBPath, cfg.Location)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	db.SetShortBaseURL(cfg.BaseURL)

	hash, err := auth.HashPassword(testPassword, fastArgonParams())
	if err != nil {
		t.Fatal(err)
	}
	if err := db.SetAdmin(context.Background(), testUsername, hash, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	files, err := qrfile.New(cfg.QRDir)
	if err != nil {
		t.Fatal(err)
	}

	clk := &clock{now: time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cleaner := cleanup.New(db, files, logger, 0).WithClock(clk.Now)

	srv, err := server.New(server.Options{
		Config: cfg, DB: db, Files: files, Cleaner: cleaner, Logger: logger, Now: clk.Now,
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // assert on the redirect itself
		},
		Timeout: 20 * time.Second,
	}

	return &env{t: t, ts: ts, cfg: cfg, db: db, files: files, cleaner: cleaner, clock: clk, client: client}
}

func fastArgonParams() auth.Argon2idParams {
	p := auth.DefaultParams()
	p.Memory = 8 * 1024
	p.Iterations = 1
	return p
}

func (e *env) url(path string) string { return e.ts.URL + path }

// do performs a request without any session handling.
func (e *env) do(req *http.Request) *http.Response {
	e.t.Helper()
	res, err := e.client.Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", req.Method, req.URL.Path, err)
	}
	return res
}

func (e *env) get(path string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(http.MethodGet, e.url(path), nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return e.do(req)
}

func body(t *testing.T, res *http.Response) string {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func rawBody(t *testing.T, res *http.Response) []byte {
	t.Helper()
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var (
	loginTokenRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)
	adminTokenRe = regexp.MustCompile(`data-csrf="([^"]+)"`)
)

// login performs the full form based login and captures the session CSRF token.
func (e *env) login() {
	e.t.Helper()
	page := body(e.t, e.get("/adminpage/login"))
	m := loginTokenRe.FindStringSubmatch(page)
	if m == nil {
		e.t.Fatal("the login page does not contain a CSRF token")
	}

	form := url.Values{
		"csrf_token": {m[1]},
		"username":   {testUsername},
		"password":   {testPassword},
	}
	req, err := http.NewRequest(http.MethodPost, e.url("/adminpage/login"), strings.NewReader(form.Encode()))
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res := e.do(req)
	defer res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		e.t.Fatalf("login returned %d, want 303", res.StatusCode)
	}

	adminPage := body(e.t, e.get("/adminpage"))
	tm := adminTokenRe.FindStringSubmatch(adminPage)
	if tm == nil {
		e.t.Fatal("the admin page does not expose a CSRF token")
	}
	e.csrf = tm[1]
}

// newSession logs in from a second, independent browser sharing the same test
// server. It is used to prove that revoking sessions really cuts them off.
func (e *env) newSession() *env {
	e.t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		e.t.Fatal(err)
	}
	clone := *e
	clone.client = &http.Client{
		Jar:           jar,
		CheckRedirect: e.client.CheckRedirect,
		Timeout:       e.client.Timeout,
	}
	clone.csrf = ""
	clone.login()
	return &clone
}

// api issues an authenticated JSON request with the session CSRF token.
func (e *env) api(method, path string, payload any) *http.Response {
	e.t.Helper()
	return e.apiWithToken(method, path, payload, e.csrf)
}

func (e *env) apiWithToken(method, path string, payload any, token string) *http.Response {
	e.t.Helper()
	var reader io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			e.t.Fatal(err)
		}
		reader = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, e.url(path), reader)
	if err != nil {
		e.t.Fatal(err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("X-CSRF-Token", token)
	}
	return e.do(req)
}

// apiJSON performs a request and decodes the JSON response, failing when the
// status code is unexpected.
func (e *env) apiJSON(method, path string, payload any, wantStatus int) map[string]any {
	e.t.Helper()
	res := e.api(method, path, payload)
	raw := rawBody(e.t, res)
	if res.StatusCode != wantStatus {
		e.t.Fatalf("%s %s returned %d (want %d): %s", method, path, res.StatusCode, wantStatus, raw)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		e.t.Fatalf("%s %s: decode response: %v (%s)", method, path, err, raw)
	}
	return out
}

// createLink is a convenience wrapper around the short URL creation endpoint.
func (e *env) createLink(payload map[string]any) map[string]any {
	e.t.Helper()
	out := e.apiJSON(http.MethodPost, "/adminpage/api/shorturls", payload, http.StatusCreated)
	item, ok := out["item"].(map[string]any)
	if !ok {
		e.t.Fatalf("unexpected create response: %v", out)
	}
	return item
}

// createQR is a convenience wrapper around the QR creation endpoint.
func (e *env) createQR(payload map[string]any) map[string]any {
	e.t.Helper()
	out := e.apiJSON(http.MethodPost, "/adminpage/api/qrcodes", payload, http.StatusCreated)
	item, ok := out["item"].(map[string]any)
	if !ok {
		e.t.Fatalf("unexpected create response: %v", out)
	}
	return item
}

func (e *env) listLinks(query string) []map[string]any {
	e.t.Helper()
	out := e.apiJSON(http.MethodGet, "/adminpage/api/shorturls"+query, nil, http.StatusOK)
	return toItems(e.t, out)
}

func (e *env) listQRs(query string) []map[string]any {
	e.t.Helper()
	out := e.apiJSON(http.MethodGet, "/adminpage/api/qrcodes"+query, nil, http.StatusOK)
	return toItems(e.t, out)
}

func toItems(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	raw, ok := out["items"].([]any)
	if !ok {
		t.Fatalf("response has no items array: %v", out)
	}
	items := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("item is not an object: %v", r)
		}
		items = append(items, m)
	}
	return items
}

func str(t *testing.T, m map[string]any, key string) string {
	t.Helper()
	v, ok := m[key].(string)
	if !ok {
		t.Fatalf("field %q is not a string in %v", key, m)
	}
	return v
}

func num(t *testing.T, m map[string]any, key string) float64 {
	t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		t.Fatalf("field %q is not a number in %v", key, m)
	}
	return v
}

// itoa renders an identifier for use in a request path.
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

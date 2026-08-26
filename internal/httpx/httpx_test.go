package httpx_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/httpx"
)

func mustProxies(t *testing.T, list string) *httpx.TrustedProxies {
	t.Helper()
	p, err := httpx.ParseTrustedProxies(list)
	if err != nil {
		t.Fatalf("ParseTrustedProxies(%q): %v", list, err)
	}
	return p
}

func TestParseTrustedProxies(t *testing.T) {
	none := mustProxies(t, "")
	if none.Enabled() {
		t.Error("an empty list must not trust anything")
	}
	if none.String() != "none" {
		t.Errorf("String() = %q", none.String())
	}

	p := mustProxies(t, "127.0.0.1/32, ::1 , 10.0.0.0/8")
	if !p.Enabled() {
		t.Fatal("the list should be enabled")
	}
	for _, addr := range []string{"127.0.0.1", "::1", "10.4.5.6"} {
		a := netip.MustParseAddr(addr)
		if !p.Contains(a) {
			t.Errorf("%s should be trusted", addr)
		}
	}
	for _, addr := range []string{"127.0.0.2", "192.168.1.1", "2001:db8::1"} {
		a := netip.MustParseAddr(addr)
		if p.Contains(a) {
			t.Errorf("%s must not be trusted", addr)
		}
	}

	if _, err := httpx.ParseTrustedProxies("not-an-address"); err == nil {
		t.Error("garbage should be rejected")
	}
	if _, err := httpx.ParseTrustedProxies("10.0.0.0/64"); err == nil {
		t.Error("an impossible prefix length should be rejected")
	}
}

func TestClientIP(t *testing.T) {
	base := func(peer string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = peer
		r.Header.Set("X-Forwarded-For", "1.2.3.4")
		r.Header.Set("X-Real-IP", "5.6.7.8")
		return r
	}
	loopback := mustProxies(t, "127.0.0.1/32,::1/128")
	none := mustProxies(t, "")

	// Headers are ignored when nothing is trusted.
	if got := httpx.ClientIP(base("127.0.0.1:41234"), none); got != "127.0.0.1" {
		t.Errorf("no trusted proxy: got %q", got)
	}

	// Headers are ignored when the peer is not a trusted proxy, which is what
	// stops a direct caller from forging its own address.
	if got := httpx.ClientIP(base("203.0.113.10:5000"), loopback); got != "203.0.113.10" {
		t.Errorf("untrusted peer: got %q, want the real peer address", got)
	}

	// A trusted peer gets its X-Real-IP believed.
	if got := httpx.ClientIP(base("127.0.0.1:41234"), loopback); got != "5.6.7.8" {
		t.Errorf("trusted peer: got %q, want 5.6.7.8", got)
	}

	// Falling back to the last entry of X-Forwarded-For, which is the one the
	// trusted proxy appended.
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:41234"
	r.Header.Set("X-Forwarded-For", "203.0.113.9, 198.51.100.7")
	if got := httpx.ClientIP(r, loopback); got != "198.51.100.7" {
		t.Errorf("forwarded chain: got %q, want 198.51.100.7", got)
	}

	// Rubbish headers fall through to the peer address.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:41234"
	r.Header.Set("X-Real-IP", "not-an-ip")
	r.Header.Set("X-Forwarded-For", "also-not-an-ip")
	if got := httpx.ClientIP(r, loopback); got != "127.0.0.1" {
		t.Errorf("invalid headers: got %q, want 127.0.0.1", got)
	}

	// An address without a port is returned as is.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "10.0.0.9"
	if got := httpx.ClientIP(r, none); got != "10.0.0.9" {
		t.Errorf("bare address: got %q", got)
	}

	// IPv4 mapped IPv6 peers are normalised, not treated as a separate key.
	r = httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "[::ffff:127.0.0.1]:8080"
	r.Header.Set("X-Real-IP", "9.9.9.9")
	if got := httpx.ClientIP(r, loopback); got != "9.9.9.9" {
		t.Errorf("mapped loopback peer: got %q, want 9.9.9.9", got)
	}
}

func TestIsLoopback(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = "127.0.0.1:1234"
	if !httpx.IsLoopback(r) {
		t.Error("127.0.0.1 should be loopback")
	}
	r.RemoteAddr = "203.0.113.1:1234"
	if httpx.IsLoopback(r) {
		t.Error("a public address is not loopback")
	}
}

func TestLimiterAllowsBurstThenBlocks(t *testing.T) {
	l := httpx.NewLimiter(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !l.Allow("a") {
			t.Fatalf("request %d inside the burst was blocked", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("the fourth request was allowed")
	}
	// A different key has its own bucket.
	if !l.Allow("b") {
		t.Fatal("an unrelated key was blocked")
	}
	// Resetting clears the bucket, which is what a successful login does.
	l.Reset("a")
	if !l.Allow("a") {
		t.Fatal("the bucket was not reset")
	}
}

func TestLimiterRefills(t *testing.T) {
	// One token per 10 milliseconds.
	l := httpx.NewLimiter(2, 20*time.Millisecond)
	for i := 0; i < 2; i++ {
		if !l.Allow("k") {
			t.Fatalf("request %d of the initial burst was blocked", i)
		}
	}
	if l.Allow("k") {
		t.Fatal("the bucket did not run empty")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !l.Allow("k") {
		if time.Now().After(deadline) {
			t.Fatal("the bucket never refilled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestLimiterEvictsInsteadOfLockingOut is the important property: a flood of
// distinct keys must not turn into a denial of service for everybody else.
func TestLimiterEvictsInsteadOfLockingOut(t *testing.T) {
	l := httpx.NewLimiter(1000, time.Minute)
	l.SetMaxKeys(64)

	for i := 0; i < 500; i++ {
		if !l.Allow(fmt.Sprintf("flood-%d", i)) {
			t.Fatalf("flood key %d was blocked; the limiter is failing closed", i)
		}
	}
	if got := l.Len(); got > 64 {
		t.Fatalf("the limiter grew to %d buckets, want at most 64", got)
	}
	if !l.Allow("a-real-visitor") {
		t.Fatal("a fresh key was blocked after a flood of distinct keys")
	}
}

func TestLimiterDefaults(t *testing.T) {
	l := httpx.NewLimiter(0, 0)
	if !l.Allow("k") {
		t.Fatal("a zero burst should still allow one request")
	}
	if l.Allow("k") {
		t.Fatal("a zero burst should allow only one request")
	}
}

func TestResponseRecorder(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httpx.NewRecorder(rec)
	if r.Status != http.StatusOK {
		t.Fatalf("the default status is %d", r.Status)
	}
	r.WriteHeader(http.StatusTeapot)
	n, err := r.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if r.Status != http.StatusTeapot || r.Bytes != 5 {
		t.Fatalf("recorder captured %d / %d bytes", r.Status, r.Bytes)
	}
	if r.Unwrap() != http.ResponseWriter(rec) {
		t.Fatal("Unwrap did not return the wrapped writer")
	}
}

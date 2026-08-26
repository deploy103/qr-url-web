// Package httpx holds small HTTP helpers shared by the server: client address
// resolution, a token bucket rate limiter and a response recorder.
package httpx

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

// TrustedProxies decides whether the forwarding headers of a request may be
// believed. Headers are only honoured when the immediate peer is one of the
// configured networks, so an attacker who reaches the application directly
// cannot spoof an address and slip past the rate limits.
type TrustedProxies struct {
	enabled bool
	nets    []netip.Prefix
}

// ParseTrustedProxies builds the allow list from a comma separated list of
// addresses or CIDR blocks. An empty list disables header trust entirely.
func ParseTrustedProxies(list string) (*TrustedProxies, error) {
	t := &TrustedProxies{}
	for _, raw := range strings.Split(list, ",") {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			t.nets = append(t.nets, prefix)
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("httpx: %q is neither an IP address nor a CIDR block", entry)
		}
		t.nets = append(t.nets, netip.PrefixFrom(addr, addr.BitLen()))
	}
	t.enabled = len(t.nets) > 0
	return t, nil
}

// Enabled reports whether any network is trusted.
func (t *TrustedProxies) Enabled() bool { return t != nil && t.enabled }

// Contains reports whether an address belongs to a trusted network.
func (t *TrustedProxies) Contains(addr netip.Addr) bool {
	if !t.Enabled() {
		return false
	}
	addr = addr.Unmap()
	for _, n := range t.nets {
		if n.Contains(addr) {
			return true
		}
	}
	return false
}

// String renders the allow list for logging.
func (t *TrustedProxies) String() string {
	if !t.Enabled() {
		return "none"
	}
	parts := make([]string, 0, len(t.nets))
	for _, n := range t.nets {
		parts = append(parts, n.String())
	}
	return strings.Join(parts, ",")
}

// peerAddr returns the address of the immediate TCP peer.
func peerAddr(r *http.Request) (netip.Addr, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, host
	}
	return addr.Unmap(), addr.Unmap().String()
}

// ClientIP resolves the address of the caller.
func ClientIP(r *http.Request, trusted *TrustedProxies) string {
	addr, fallback := peerAddr(r)
	if !addr.IsValid() || !trusted.Contains(addr) {
		return fallback
	}

	if v := strings.TrimSpace(r.Header.Get("X-Real-IP")); v != "" {
		if ip, err := netip.ParseAddr(v); err == nil {
			return ip.Unmap().String()
		}
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		parts := strings.Split(v, ",")
		// Walk from the end: the last entry is the one the trusted proxy
		// appended, and everything before it is attacker controlled.
		for i := len(parts) - 1; i >= 0; i-- {
			candidate := strings.TrimSpace(parts[i])
			if ip, err := netip.ParseAddr(candidate); err == nil {
				return ip.Unmap().String()
			}
			// A bracketed IPv6 literal with a port is also accepted.
			if host, _, err := net.SplitHostPort(candidate); err == nil {
				if ip, err := netip.ParseAddr(host); err == nil {
					return ip.Unmap().String()
				}
			}
		}
	}
	return fallback
}

// IsLoopback reports whether the immediate peer is on the local machine.
func IsLoopback(r *http.Request) bool {
	addr, _ := peerAddr(r)
	return addr.IsValid() && addr.IsLoopback()
}

// ResponseRecorder captures the status code and byte count of a response.
type ResponseRecorder struct {
	http.ResponseWriter
	Status int
	Bytes  int
}

// NewRecorder wraps w.
func NewRecorder(w http.ResponseWriter) *ResponseRecorder {
	return &ResponseRecorder{ResponseWriter: w, Status: http.StatusOK}
}

// WriteHeader records the status code.
func (r *ResponseRecorder) WriteHeader(code int) {
	r.Status = code
	r.ResponseWriter.WriteHeader(code)
}

// Write records the payload size.
func (r *ResponseRecorder) Write(b []byte) (int, error) {
	n, err := r.ResponseWriter.Write(b)
	r.Bytes += n
	return n, err
}

// Unwrap exposes the wrapped writer to http.ResponseController.
func (r *ResponseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Limiter is a per key token bucket rate limiter.
type Limiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	rate     float64 // tokens per second
	burst    float64
	lastGC   time.Time
	maxKeys  int
	nowFunc  func() time.Time
	gcPeriod time.Duration
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewLimiter builds a limiter that refills burst tokens over the given window.
func NewLimiter(burst int, window time.Duration) *Limiter {
	if burst < 1 {
		burst = 1
	}
	if window <= 0 {
		window = time.Minute
	}
	return &Limiter{
		buckets:  make(map[string]*bucket),
		rate:     float64(burst) / window.Seconds(),
		burst:    float64(burst),
		maxKeys:  20000,
		nowFunc:  time.Now,
		gcPeriod: 10 * time.Minute,
	}
}

// Allow consumes one token for key and reports whether the request may proceed.
func (l *Limiter) Allow(key string) bool {
	now := l.nowFunc()
	l.mu.Lock()
	defer l.mu.Unlock()

	l.collect(now)

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			// A flood of distinct keys must not lock out the keys that are
			// already tracked, and it must not grow without bound either, so
			// make room by dropping the least recently seen bucket.
			l.evictOldest()
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	elapsed := now.Sub(b.last).Seconds()
	if elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Reset clears the bucket of a key, for example after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

// SetMaxKeys bounds how many buckets are held at once. It exists so tests can
// exercise the eviction path without allocating twenty thousand entries.
func (l *Limiter) SetMaxKeys(n int) {
	if n < 1 {
		n = 1
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.maxKeys = n
}

// Len reports how many buckets are currently tracked.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// collect drops buckets that have been idle for a while. The caller holds the
// lock.
func (l *Limiter) collect(now time.Time) {
	if now.Sub(l.lastGC) < l.gcPeriod {
		return
	}
	l.lastGC = now
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.gcPeriod {
			delete(l.buckets, k)
		}
	}
}

// evictOldest removes the least recently used bucket. The caller holds the
// lock. Map iteration order is random, so a bounded scan is enough to find a
// good victim without walking the whole map on every insert.
func (l *Limiter) evictOldest() {
	const sample = 32
	var victim string
	var oldest time.Time
	seen := 0
	for k, b := range l.buckets {
		if victim == "" || b.last.Before(oldest) {
			victim, oldest = k, b.last
		}
		if seen++; seen >= sample {
			break
		}
	}
	if victim != "" {
		delete(l.buckets, victim)
	}
}

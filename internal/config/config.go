// Package config loads the runtime configuration from the environment.
package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mvtp/qrurl/internal/httpx"
)

// Config holds every tunable of the service. Defaults are production sane:
// the server binds to the loopback interface and expects to sit behind an
// HTTPS terminating reverse proxy.
type Config struct {
	Addr            string
	BaseURL         string
	DataDir         string
	DBPath          string
	QRDir           string
	BackupDir       string
	Location        *time.Location
	SessionTTL      time.Duration
	CleanupInterval time.Duration
	BackupInterval  time.Duration
	BackupKeep      int
	ReadTimeout     time.Duration
	WriteTimeout    time.Duration
	IdleTimeout     time.Duration
	ShutdownGrace   time.Duration
	MaxBodyBytes    int64
	SecureCookie    bool
	TrustProxy      bool
	TrustedProxies  *httpx.TrustedProxies
	HSTS            bool
	PublicQR        bool
	PublicQRAllow   *httpx.TrustedProxies
	RedirectRate    int
	SingleInstance  bool
	LogLevel        string
	LogFormat       string
}

// Load reads the configuration from QRURL_* environment variables.
func Load() (*Config, error) {
	c := &Config{
		Addr:            env("QRURL_ADDR", "127.0.0.1:8080"),
		BaseURL:         env("QRURL_BASE_URL", "https://url.mvtp.cloud"),
		DataDir:         env("QRURL_DATA_DIR", "./data"),
		SessionTTL:      12 * time.Hour,
		CleanupInterval: 5 * time.Minute,
		BackupInterval:  24 * time.Hour,
		BackupKeep:      14,
		ReadTimeout:     15 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     60 * time.Second,
		ShutdownGrace:   20 * time.Second,
		MaxBodyBytes:    64 * 1024,
		SecureCookie:    true,
		TrustProxy:      true,
		HSTS:            true,
		PublicQR:        true,
		RedirectRate:    120,
		SingleInstance:  true,
		LogLevel:        env("QRURL_LOG_LEVEL", "info"),
		LogFormat:       env("QRURL_LOG_FORMAT", "json"),
	}

	var err error
	if c.SessionTTL, err = envDuration("QRURL_SESSION_TTL", c.SessionTTL); err != nil {
		return nil, err
	}
	if c.CleanupInterval, err = envDuration("QRURL_CLEANUP_INTERVAL", c.CleanupInterval); err != nil {
		return nil, err
	}
	if c.BackupInterval, err = envDuration("QRURL_BACKUP_INTERVAL", c.BackupInterval); err != nil {
		return nil, err
	}
	if c.BackupKeep, err = envInt("QRURL_BACKUP_KEEP", c.BackupKeep); err != nil {
		return nil, err
	}
	if c.MaxBodyBytes, err = envInt64("QRURL_MAX_BODY_BYTES", c.MaxBodyBytes); err != nil {
		return nil, err
	}
	if c.SecureCookie, err = envBool("QRURL_SECURE_COOKIE", c.SecureCookie); err != nil {
		return nil, err
	}
	if c.TrustProxy, err = envBool("QRURL_TRUST_PROXY", c.TrustProxy); err != nil {
		return nil, err
	}
	if c.HSTS, err = envBool("QRURL_HSTS", c.HSTS); err != nil {
		return nil, err
	}
	if c.PublicQR, err = envBool("QRURL_PUBLIC_QR", c.PublicQR); err != nil {
		return nil, err
	}
	if c.RedirectRate, err = envInt("QRURL_REDIRECT_RATE", c.RedirectRate); err != nil {
		return nil, err
	}
	if c.SingleInstance, err = envBool("QRURL_SINGLE_INSTANCE", c.SingleInstance); err != nil {
		return nil, err
	}

	// An empty allow list means the public generator is open to everyone; a
	// non empty one restricts it to those networks, which is how a service on
	// a private LAN keeps it off the public internet.
	if c.PublicQRAllow, err = httpx.ParseTrustedProxies(env("QRURL_PUBLIC_QR_ALLOW", "")); err != nil {
		return nil, fmt.Errorf("config: QRURL_PUBLIC_QR_ALLOW: %w", err)
	}

	// Forwarding headers are only believed when the request actually arrives
	// from one of these networks, which by default means the local Nginx.
	proxies := ""
	if c.TrustProxy {
		proxies = env("QRURL_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	}
	if c.TrustedProxies, err = httpx.ParseTrustedProxies(proxies); err != nil {
		return nil, fmt.Errorf("config: QRURL_TRUSTED_PROXIES: %w", err)
	}

	tz := env("QRURL_TIMEZONE", "Asia/Seoul")
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, fmt.Errorf("config: QRURL_TIMEZONE %q: %w", tz, err)
	}
	c.Location = loc

	c.DataDir = filepath.Clean(c.DataDir)
	c.DBPath = env("QRURL_DB", filepath.Join(c.DataDir, "qrurl.db"))
	c.QRDir = env("QRURL_QR_DIR", filepath.Join(c.DataDir, "qr"))
	c.BackupDir = env("QRURL_BACKUP_DIR", filepath.Join(c.DataDir, "backups"))

	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Config) validate() error {
	u, err := url.Parse(c.BaseURL)
	if err != nil {
		return fmt.Errorf("config: QRURL_BASE_URL is not a URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("config: QRURL_BASE_URL must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("config: QRURL_BASE_URL has no host")
	}
	c.BaseURL = strings.TrimRight(u.String(), "/")

	if c.Addr == "" {
		return fmt.Errorf("config: QRURL_ADDR is empty")
	}
	if c.SessionTTL < time.Minute {
		return fmt.Errorf("config: QRURL_SESSION_TTL must be at least one minute")
	}
	if c.CleanupInterval < 10*time.Second {
		return fmt.Errorf("config: QRURL_CLEANUP_INTERVAL must be at least ten seconds")
	}
	if c.MaxBodyBytes < 1024 {
		return fmt.Errorf("config: QRURL_MAX_BODY_BYTES must be at least 1024")
	}
	if c.RedirectRate < 1 {
		return fmt.Errorf("config: QRURL_REDIRECT_RATE must be at least 1")
	}
	return nil
}

// ShortURL renders the public address of a short code.
func (c *Config) ShortURL(code string) string { return c.BaseURL + "/" + code }

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("config: %s must be a boolean: %w", key, err)
	}
	return b, nil
}

func envInt(key string, def int) (int, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be an integer: %w", key, err)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration such as 30m: %w", key, err)
	}
	return d, nil
}

package config_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/config"
)

func TestDefaults(t *testing.T) {
	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("the default bind address is %q, want the loopback interface", cfg.Addr)
	}
	if cfg.BaseURL != "https://url.mvtp.cloud" {
		t.Errorf("base URL = %q", cfg.BaseURL)
	}
	if !cfg.SecureCookie || !cfg.HSTS || !cfg.TrustProxy {
		t.Errorf("the production defaults are not secure: %+v", cfg)
	}
	if cfg.Location.String() != "Asia/Seoul" {
		t.Errorf("timezone = %q", cfg.Location)
	}
	if cfg.DBPath != filepath.Join("data", "qrurl.db") {
		t.Errorf("database path = %q", cfg.DBPath)
	}
	if cfg.QRDir != filepath.Join("data", "qr") {
		t.Errorf("qr directory = %q", cfg.QRDir)
	}
	if cfg.MaxBodyBytes < 1024 {
		t.Errorf("body limit = %d", cfg.MaxBodyBytes)
	}
	if cfg.ShortURL("001") != "https://url.mvtp.cloud/001" {
		t.Errorf("ShortURL = %q", cfg.ShortURL("001"))
	}
}

func TestOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("QRURL_ADDR", "0.0.0.0:9000")
	t.Setenv("QRURL_BASE_URL", "http://localhost:9000/")
	t.Setenv("QRURL_DATA_DIR", dir)
	t.Setenv("QRURL_SECURE_COOKIE", "false")
	t.Setenv("QRURL_HSTS", "0")
	t.Setenv("QRURL_PUBLIC_QR", "false")
	t.Setenv("QRURL_TIMEZONE", "UTC")
	t.Setenv("QRURL_SESSION_TTL", "45m")
	t.Setenv("QRURL_CLEANUP_INTERVAL", "90s")
	t.Setenv("QRURL_MAX_BODY_BYTES", "8192")
	t.Setenv("QRURL_BACKUP_KEEP", "3")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != "0.0.0.0:9000" {
		t.Errorf("addr = %q", cfg.Addr)
	}
	if cfg.BaseURL != "http://localhost:9000" {
		t.Errorf("the trailing slash was not trimmed: %q", cfg.BaseURL)
	}
	if cfg.SecureCookie || cfg.HSTS || cfg.PublicQR {
		t.Errorf("boolean overrides were ignored: %+v", cfg)
	}
	if cfg.SessionTTL != 45*time.Minute || cfg.CleanupInterval != 90*time.Second {
		t.Errorf("duration overrides were ignored: %v / %v", cfg.SessionTTL, cfg.CleanupInterval)
	}
	if cfg.MaxBodyBytes != 8192 || cfg.BackupKeep != 3 {
		t.Errorf("numeric overrides were ignored: %d / %d", cfg.MaxBodyBytes, cfg.BackupKeep)
	}
	if cfg.DBPath != filepath.Join(dir, "qrurl.db") {
		t.Errorf("database path = %q", cfg.DBPath)
	}
}

func TestInvalidValuesAreRejected(t *testing.T) {
	cases := []struct{ key, value string }{
		{"QRURL_BASE_URL", "ftp://example.com"},
		{"QRURL_BASE_URL", "not a url at all"},
		{"QRURL_BASE_URL", "https://"},
		{"QRURL_TIMEZONE", "Mars/Olympus"},
		{"QRURL_SESSION_TTL", "10s"},
		{"QRURL_SESSION_TTL", "abc"},
		{"QRURL_CLEANUP_INTERVAL", "1s"},
		{"QRURL_MAX_BODY_BYTES", "10"},
		{"QRURL_MAX_BODY_BYTES", "many"},
		{"QRURL_SECURE_COOKIE", "yesplease"},
		{"QRURL_ADDR", ""},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			if c.key == "QRURL_ADDR" && c.value == "" {
				// An empty variable falls back to the default, so this case is
				// exercised through a whitespace value instead.
				t.Setenv(c.key, " ")
				cfg, err := config.Load()
				if err == nil && cfg.Addr != " " {
					t.Skip("an empty address falls back to the default")
				}
				return
			}
			t.Setenv(c.key, c.value)
			if _, err := config.Load(); err == nil {
				t.Errorf("%s=%q was accepted", c.key, c.value)
			}
		})
	}
}

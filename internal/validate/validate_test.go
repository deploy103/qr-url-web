package validate_test

import (
	"strings"
	"testing"
	"time"

	"github.com/deploy103/qr-url-web/internal/validate"
)

func TestShortCode(t *testing.T) {
	good := map[string]string{
		"1": "001", "01": "001", "001": "001",
		"7": "007", "42": "042", "123": "123", "999": "999",
		" 123 ": "123",
	}
	for in, want := range good {
		got, err := validate.ShortCode(in)
		if err != nil {
			t.Errorf("ShortCode(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ShortCode(%q) = %q, want %q", in, got, want)
		}
	}

	bad := []string{"", "0", "00", "000", "1000", "0000", "abc", "12a", "-1", "1.0", "１２３", "12 3"}
	for _, in := range bad {
		if got, err := validate.ShortCode(in); err == nil {
			t.Errorf("ShortCode(%q) = %q, want an error", in, got)
		}
	}
}

func TestTargetURLAllowsOnlyHTTPAndHTTPS(t *testing.T) {
	good := []string{
		"http://example.com",
		"https://example.com",
		"https://example.com/path?a=1&b=2#frag",
		"HTTPS://Example.COM/Path",
		"https://food.hsoc.kr",
		"https://xn--h32bi4v.xn--3e0b707e/",
		"http://127.0.0.1:8080/x",
	}
	for _, in := range good {
		if _, err := validate.TargetURL(in); err != nil {
			t.Errorf("TargetURL(%q) rejected: %v", in, err)
		}
	}

	bad := []string{
		"",
		"   ",
		"javascript:alert(1)",
		"JavaScript:alert(1)",
		"  javascript:alert(1)",
		"data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==",
		"DATA:text/html,<h1>x</h1>",
		"file:///etc/passwd",
		"vbscript:msgbox(1)",
		"ftp://example.com/f",
		"mailto:someone@example.com",
		"tel:+8210",
		"//example.com/protocol-relative",
		"/relative/path",
		"example.com",
		"https://user:pass@example.com",
		"https://",
		"http://",
		"java\nscript:alert(1)",
		"https://example.com/\x00",
		"https://exa mple.com",
		"https://example.com\\@evil.com",
		"https://" + strings.Repeat("a", validate.MaxTargetURLLength) + ".com",
	}
	for _, in := range bad {
		if got, err := validate.TargetURL(in); err == nil {
			t.Errorf("TargetURL(%q) = %q, want an error", in, got)
		}
	}
}

func TestPayload(t *testing.T) {
	if _, err := validate.Payload(""); err == nil {
		t.Error("an empty payload should be rejected")
	}
	if _, err := validate.Payload(strings.Repeat("a", validate.MaxPayloadLength+1)); err == nil {
		t.Error("an oversized payload should be rejected")
	}
	if _, err := validate.Payload(string([]byte{0xff, 0xfe})); err == nil {
		t.Error("invalid UTF-8 should be rejected")
	}
	if got, err := validate.Payload("안녕 <script>alert(1)</script>"); err != nil || got != "안녕 <script>alert(1)</script>" {
		t.Errorf("Payload should pass content through verbatim, got %q err %v", got, err)
	}
}

func TestNoteAndLabelLimits(t *testing.T) {
	if _, err := validate.Note(strings.Repeat("가", validate.MaxNoteLength+1)); err == nil {
		t.Error("an oversized note should be rejected")
	}
	if got, err := validate.Note("  memo  "); err != nil || got != "memo" {
		t.Errorf("Note should trim, got %q err %v", got, err)
	}
	if _, err := validate.Label(strings.Repeat("가", validate.MaxLabelLength+1)); err == nil {
		t.Error("an oversized label should be rejected")
	}
}

func TestExpiryPresets(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	loc := time.UTC

	if got, err := validate.Expiry("none", "", now, loc); err != nil || got != nil {
		t.Errorf(`Expiry("none") = %v, %v; want nil, nil`, got, err)
	}
	if got, err := validate.Expiry("", "", now, loc); err != nil || got != nil {
		t.Errorf(`Expiry("") = %v, %v; want nil, nil`, got, err)
	}

	cases := map[string]time.Duration{
		"1h": time.Hour, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour,
	}
	for preset, d := range cases {
		got, err := validate.Expiry(preset, "", now, loc)
		if err != nil {
			t.Fatalf("Expiry(%q): %v", preset, err)
		}
		if want := now.Add(d); !got.Equal(want) {
			t.Errorf("Expiry(%q) = %v, want %v", preset, got, want)
		}
	}

	if _, err := validate.Expiry("2h", "", now, loc); err == nil {
		t.Error("an unknown preset should be rejected")
	}
}

func TestCustomExpiry(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skipf("timezone database unavailable: %v", err)
	}

	got, err := validate.Expiry("custom", "2026-08-27T09:30", now, seoul)
	if err != nil {
		t.Fatalf("custom expiry: %v", err)
	}
	want := time.Date(2026, 8, 27, 9, 30, 0, 0, seoul).UTC()
	if !got.Equal(want) {
		t.Errorf("custom expiry = %v, want %v", got.UTC(), want)
	}

	if _, err := validate.Expiry("custom", "2026-08-27T00:30:00+09:00", now, seoul); err != nil {
		t.Errorf("RFC3339 custom expiry rejected: %v", err)
	}
	if _, err := validate.Expiry("custom", "2026-08-25T12:00", now, time.UTC); err == nil {
		t.Error("an expiry in the past should be rejected")
	}
	if _, err := validate.Expiry("custom", "2099-01-01T00:00", now, time.UTC); err == nil {
		t.Error("an expiry beyond the maximum should be rejected")
	}
	if _, err := validate.Expiry("custom", "not a date", now, time.UTC); err == nil {
		t.Error("an unparsable expiry should be rejected")
	}
	if _, err := validate.Expiry("custom", "", now, time.UTC); err == nil {
		t.Error("an empty custom expiry should be rejected")
	}
}

func TestExtend(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	// An unlimited item starts counting from now.
	got, err := validate.Extend(nil, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("Extend(nil) = %v, want %v", got, want)
	}

	// A live expiry is pushed further out.
	current := now.Add(2 * time.Hour)
	got, err = validate.Extend(&current, 24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := current.Add(24 * time.Hour); !got.Equal(want) {
		t.Errorf("Extend(future) = %v, want %v", got, want)
	}

	// An already lapsed expiry restarts from now, not from the past.
	past := now.Add(-48 * time.Hour)
	got, err = validate.Extend(&past, time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(time.Hour); !got.Equal(want) {
		t.Errorf("Extend(past) = %v, want %v", got, want)
	}

	if _, err := validate.Extend(nil, 0, now); err == nil {
		t.Error("a zero duration should be rejected")
	}
}

func TestParseExtendDuration(t *testing.T) {
	for raw, want := range map[string]time.Duration{
		"1h": time.Hour, "1d": 24 * time.Hour, "7d": 7 * 24 * time.Hour,
		"30d": 30 * 24 * time.Hour, "90d": 90 * 24 * time.Hour,
	} {
		got, err := validate.ParseExtendDuration(raw)
		if err != nil || got != want {
			t.Errorf("ParseExtendDuration(%q) = %v, %v; want %v", raw, got, err, want)
		}
	}
	if _, err := validate.ParseExtendDuration("5y"); err == nil {
		t.Error("an unknown duration should be rejected")
	}
}

func TestSizeAndLevel(t *testing.T) {
	if got, err := validate.Size("", 512); err != nil || got != 512 {
		t.Errorf("Size(\"\") = %d, %v", got, err)
	}
	if got, err := validate.Size("1024", 512); err != nil || got != 1024 {
		t.Errorf("Size(\"1024\") = %d, %v", got, err)
	}
	for _, bad := range []string{"0", "10", "9999", "abc", "-512"} {
		if _, err := validate.Size(bad, 512); err == nil {
			t.Errorf("Size(%q) should fail", bad)
		}
	}
	for _, good := range []string{"L", "m", "Q", "h", ""} {
		if _, err := validate.ECLevel(good); err != nil {
			t.Errorf("ECLevel(%q): %v", good, err)
		}
	}
	if _, err := validate.ECLevel("Z"); err == nil {
		t.Error("an unknown level should be rejected")
	}
}

func TestQRKind(t *testing.T) {
	for _, k := range []string{"url", "text", "short"} {
		if got, err := validate.QRKind(k); err != nil || got != k {
			t.Errorf("QRKind(%q) = %q, %v", k, got, err)
		}
	}
	for _, k := range []string{"", "SHORT", "image", "'; DROP TABLE qr_codes; --"} {
		if _, err := validate.QRKind(k); err == nil {
			t.Errorf("QRKind(%q) should fail", k)
		}
	}
}

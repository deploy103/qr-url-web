package qr_test

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/mvtp/qrurl/internal/qr"
	"github.com/mvtp/qrurl/internal/qrdecode"
)

// TestPayloadIsStoredVerbatim is the requirement stated in the specification:
// what goes in must come out, byte for byte.
func TestPayloadIsStoredVerbatim(t *testing.T) {
	payloads := []string{
		"https://example.com/abc?a=1&b=2",
		"https://example.com/test",
		"https://food.hsoc.kr",
		"https://url.mvtp.cloud/123",
		"HELLO WORLD",
		"0123456789",
		"1",
		"안녕하세요, QR 코드입니다.",
		"line1\nline2\ttab",
		`{"json":"value","n":[1,2,3]}`,
		"https://url.mvtp.cloud/001?utm_source=qr&utm_medium=print#top",
		strings.Repeat("x", 512),
	}
	levels := []qr.Level{qr.LevelL, qr.LevelM, qr.LevelQ, qr.LevelH}

	for _, payload := range payloads {
		for _, level := range levels {
			t.Run(fmt.Sprintf("%.20s/%s", payload, level), func(t *testing.T) {
				code, err := qr.Encode(payload, level)
				if err != nil {
					t.Fatalf("Encode: %v", err)
				}
				if code.Payload() != payload {
					t.Fatalf("Payload() = %q, want %q", code.Payload(), payload)
				}

				png, err := code.PNG(qr.RenderOptions{Size: 512})
				if err != nil {
					t.Fatalf("PNG: %v", err)
				}
				got, err := qrdecode.DecodePNG(png)
				if err != nil {
					t.Fatalf("DecodePNG: %v", err)
				}
				if got.Text != payload {
					t.Errorf("PNG round trip = %q, want %q", got.Text, payload)
				}
				if got.Version != code.Version() {
					t.Errorf("PNG version = %d, want %d", got.Version, code.Version())
				}
				if got.Level != level.String() {
					t.Errorf("PNG level = %s, want %s", got.Level, level)
				}
				if got.Mask != code.Mask() {
					t.Errorf("PNG mask = %d, want %d", got.Mask, code.Mask())
				}
				if got.Corrections != 0 {
					t.Errorf("PNG needed %d corrections on a pristine symbol", got.Corrections)
				}

				svg := code.SVG(qr.RenderOptions{Size: 512})
				gotSVG, err := qrdecode.DecodeSVG(svg)
				if err != nil {
					t.Fatalf("DecodeSVG: %v", err)
				}
				if gotSVG.Text != payload {
					t.Errorf("SVG round trip = %q, want %q", gotSVG.Text, payload)
				}
			})
		}
	}
}

// TestSpecificationExample is the exact case named in the task description.
func TestSpecificationExample(t *testing.T) {
	const payload = "https://example.com/abc?a=1&b=2"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	png, err := code.PNG(qr.RenderOptions{Size: 512})
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	res, err := qrdecode.DecodePNG(png)
	if err != nil {
		t.Fatalf("DecodePNG: %v", err)
	}
	if res.Text != payload {
		t.Fatalf("decoded %q, want %q", res.Text, payload)
	}
}

// TestAllVersionsRoundTrip exercises every version and level combination at
// close to full capacity, which covers the block interleaving of large symbols.
func TestAllVersionsRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(20260826))
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.~/:?&=%#"

	for version := 1; version <= 40; version++ {
		for _, level := range []qr.Level{qr.LevelL, qr.LevelM, qr.LevelQ, qr.LevelH} {
			capacity := qr.ByteCapacity(version, level)
			if capacity <= 0 {
				t.Fatalf("version %d level %s has no capacity", version, level)
			}
			payload := make([]byte, capacity)
			for i := range payload {
				payload[i] = alphabet[rng.Intn(len(alphabet))]
			}
			text := string(payload)

			code, err := qr.EncodeWithOptions(text, qr.Options{Level: level, MinVersion: version, Mask: -1})
			if err != nil {
				t.Fatalf("v%d/%s: Encode: %v", version, level, err)
			}
			if code.Version() != version {
				t.Fatalf("v%d/%s: chose version %d", version, level, code.Version())
			}
			png, err := code.PNG(qr.RenderOptions{Size: 1024})
			if err != nil {
				t.Fatalf("v%d/%s: PNG: %v", version, level, err)
			}
			res, err := qrdecode.DecodePNG(png)
			if err != nil {
				t.Fatalf("v%d/%s: DecodePNG: %v", version, level, err)
			}
			if res.Text != text {
				t.Fatalf("v%d/%s: round trip mismatch", version, level)
			}
			if res.Version != version {
				t.Fatalf("v%d/%s: decoded version %d", version, level, res.Version)
			}
		}
	}
}

// TestByteCapacityMatchesStandard checks a handful of well known capacities
// from the published table of ISO/IEC 18004. The exhaustive comparison lives
// in TestCapacitiesMatchIndependentImplementation.
func TestByteCapacityMatchesStandard(t *testing.T) {
	// version -> [L, M, Q, H]
	want := map[int][4]int{
		1:  {17, 14, 11, 7},
		2:  {32, 26, 20, 14},
		3:  {53, 42, 32, 24},
		4:  {78, 62, 46, 34},
		5:  {106, 84, 60, 44},
		10: {271, 213, 151, 119},
		40: {2953, 2331, 1663, 1273},
	}
	levels := [4]qr.Level{qr.LevelL, qr.LevelM, qr.LevelQ, qr.LevelH}
	for version, expected := range want {
		for i, level := range levels {
			if got := qr.ByteCapacity(version, level); got != expected[i] {
				t.Errorf("ByteCapacity(%d, %s) = %d, want %d", version, level, got, expected[i])
			}
		}
	}
	if got := qr.ByteCapacity(0, qr.LevelM); got != 0 {
		t.Errorf("ByteCapacity(0, M) = %d, want 0", got)
	}
	if got := qr.ByteCapacity(41, qr.LevelM); got != 0 {
		t.Errorf("ByteCapacity(41, M) = %d, want 0", got)
	}
}

// TestErrorCorrectionRecoversDamage confirms that the generated Reed-Solomon
// codewords really can repair a damaged symbol.
func TestErrorCorrectionRecoversDamage(t *testing.T) {
	const payload = "https://url.mvtp.cloud/007"
	code, err := qr.Encode(payload, qr.LevelH)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	grid := gridOf(code)
	// Damage a handful of data modules well away from the finder patterns.
	rng := rand.New(rand.NewSource(7))
	damaged := 0
	for damaged < 12 {
		x := 10 + rng.Intn(code.Size()-12)
		y := 10 + rng.Intn(code.Size()-12)
		if grid.Get(x, y) {
			grid.Set(x, y, false)
		} else {
			grid.Set(x, y, true)
		}
		damaged++
	}
	res, err := qrdecode.DecodeGrid(grid)
	if err != nil {
		t.Fatalf("DecodeGrid on damaged symbol: %v", err)
	}
	if res.Text != payload {
		t.Fatalf("decoded %q, want %q", res.Text, payload)
	}
	if res.Corrections == 0 {
		t.Fatal("expected the decoder to report repaired codewords")
	}
}

func gridOf(code *qr.Code) *qrdecode.Grid {
	g := qrdecode.NewGrid(code.Size())
	for y := 0; y < code.Size(); y++ {
		for x := 0; x < code.Size(); x++ {
			g.Set(x, y, code.Module(x, y))
		}
	}
	return g
}

func TestEncodeRejectsBadInput(t *testing.T) {
	if _, err := qr.Encode("", qr.LevelM); err == nil {
		t.Error("empty payload should fail")
	}
	if _, err := qr.Encode(strings.Repeat("a", qr.MaxPayloadBytes+1), qr.LevelL); err == nil {
		t.Error("oversized payload should fail")
	}
	if _, err := qr.Encode(strings.Repeat("a", 2000), qr.LevelH); err == nil {
		t.Error("payload above the level H capacity should fail")
	}
	if _, err := qr.Encode("ok", qr.Level(9)); err == nil {
		t.Error("invalid level should fail")
	}
	if _, err := qr.EncodeWithOptions("ok", qr.Options{Level: qr.LevelM, Mask: 8}); err == nil {
		t.Error("invalid mask should fail")
	}
}

func TestParseLevel(t *testing.T) {
	cases := map[string]qr.Level{
		"l": qr.LevelL, "L": qr.LevelL,
		"m": qr.LevelM, "": qr.LevelM,
		"q": qr.LevelQ, "H": qr.LevelH,
	}
	for in, want := range cases {
		got, err := qr.ParseLevel(in)
		if err != nil {
			t.Fatalf("ParseLevel(%q): %v", in, err)
		}
		if got != want {
			t.Errorf("ParseLevel(%q) = %s, want %s", in, got, want)
		}
	}
	if _, err := qr.ParseLevel("Z"); err == nil {
		t.Error("ParseLevel(\"Z\") should fail")
	}
}

func TestModeSelection(t *testing.T) {
	// Numeric mode is the most compact, so a numeric payload must fit into a
	// smaller symbol than the same length of mixed case text.
	numeric, err := qr.Encode(strings.Repeat("1", 40), qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	mixed, err := qr.Encode(strings.Repeat("aA", 20), qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	if numeric.Version() >= mixed.Version() {
		t.Errorf("numeric version %d should be smaller than byte version %d", numeric.Version(), mixed.Version())
	}

	upper, err := qr.Encode(strings.Repeat("A", 60), qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	longMixed, err := qr.Encode(strings.Repeat("aA", 30), qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	if upper.Version() >= longMixed.Version() {
		t.Errorf("alphanumeric version %d should be smaller than byte version %d", upper.Version(), longMixed.Version())
	}
}

func TestRenderSizes(t *testing.T) {
	code, err := qr.Encode("https://url.mvtp.cloud/042", qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	img := code.Image(qr.RenderOptions{Size: 512})
	grid := code.Size() + 2*qr.DefaultQuietZone
	scale := 512 / grid
	if got := img.Bounds().Dx(); got != grid*scale {
		t.Errorf("image width = %d, want %d", got, grid*scale)
	}
	// The quiet zone must be light on all four sides.
	for i := 0; i < img.Bounds().Dx(); i++ {
		for _, p := range [][2]int{{i, 0}, {0, i}, {i, img.Bounds().Dy() - 1}, {img.Bounds().Dx() - 1, i}} {
			r, g, b, _ := img.At(p[0], p[1]).RGBA()
			if r != 0xffff || g != 0xffff || b != 0xffff {
				t.Fatalf("quiet zone pixel at %v is not white", p)
			}
		}
	}

	svg := code.SVG(qr.RenderOptions{Size: 300})
	if !strings.Contains(string(svg), `width="300"`) {
		t.Error("svg does not honour the requested size")
	}
	if !strings.Contains(string(svg), fmt.Sprintf(`viewBox="0 0 %d %d"`, grid, grid)) {
		t.Error("svg viewBox does not match the module grid")
	}
}

func TestDeterministicOutput(t *testing.T) {
	a, err := qr.Encode("https://url.mvtp.cloud/999", qr.LevelQ)
	if err != nil {
		t.Fatal(err)
	}
	b, err := qr.Encode("https://url.mvtp.cloud/999", qr.LevelQ)
	if err != nil {
		t.Fatal(err)
	}
	if a.Mask() != b.Mask() || a.Version() != b.Version() {
		t.Fatal("encoding is not deterministic")
	}
	for y := 0; y < a.Size(); y++ {
		for x := 0; x < a.Size(); x++ {
			if a.Module(x, y) != b.Module(x, y) {
				t.Fatalf("module (%d,%d) differs between runs", x, y)
			}
		}
	}
}

// TestKanjiMode covers the fourth encoding mode: Japanese text is packed at
// 13 bits per character instead of the 24 that UTF-8 byte mode would need.
func TestKanjiMode(t *testing.T) {
	cases := []string{
		"漢字",
		"こんにちは世界",
		"データを保存する",
		"ソフトウェア開発",
	}
	for _, payload := range cases {
		code, err := qr.Encode(payload, qr.LevelM)
		if err != nil {
			t.Fatalf("Encode(%q): %v", payload, err)
		}
		png, err := code.PNG(qr.RenderOptions{Size: 512})
		if err != nil {
			t.Fatal(err)
		}
		res, err := qrdecode.DecodePNG(png)
		if err != nil {
			t.Fatalf("DecodePNG(%q): %v", payload, err)
		}
		if res.Text != payload {
			t.Fatalf("round trip = %q, want %q", res.Text, payload)
		}
	}

	// Kanji mode really is chosen, and it really is smaller: the same text in
	// byte mode needs three bytes per character.
	const text = "日本語のテキストです日本語のテキストです日本語のテキストです"
	kanji, err := qr.Encode(text, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	// Mixing in one ASCII character forces byte mode for the whole segment.
	forcedByte, err := qr.Encode(text+"!", qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	if kanji.Version() >= forcedByte.Version() {
		t.Errorf("kanji version %d is not smaller than the byte version %d",
			kanji.Version(), forcedByte.Version())
	}

	// Text outside the Shift-JIS double byte ranges falls back to byte mode
	// rather than failing.
	for _, mixed := range []string{"漢字 and ascii", "한글", "emoji 🙂", "漢字abc"} {
		code, err := qr.Encode(mixed, qr.LevelM)
		if err != nil {
			t.Fatalf("Encode(%q): %v", mixed, err)
		}
		png, err := code.PNG(qr.RenderOptions{Size: 512})
		if err != nil {
			t.Fatal(err)
		}
		res, err := qrdecode.DecodePNG(png)
		if err != nil {
			t.Fatalf("DecodePNG(%q): %v", mixed, err)
		}
		if res.Text != mixed {
			t.Fatalf("round trip = %q, want %q", res.Text, mixed)
		}
	}
}

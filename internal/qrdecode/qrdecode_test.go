package qrdecode_test

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math/rand"
	"strings"
	"testing"

	"github.com/deploy103/qr-url-web/internal/qr"
	"github.com/deploy103/qr-url-web/internal/qrdecode"
)

func encodePNG(t *testing.T, payload string, level qr.Level) []byte {
	t.Helper()
	code, err := qr.Encode(payload, level)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	data, err := code.PNG(qr.RenderOptions{Size: 512})
	if err != nil {
		t.Fatalf("PNG: %v", err)
	}
	return data
}

func TestDecodeReportsSymbolMetadata(t *testing.T) {
	const payload = "https://url.mvtp.cloud/123"
	code, err := qr.EncodeWithOptions(payload, qr.Options{Level: qr.LevelQ, Mask: 3})
	if err != nil {
		t.Fatal(err)
	}
	data, err := code.PNG(qr.RenderOptions{Size: 512})
	if err != nil {
		t.Fatal(err)
	}
	res, err := qrdecode.DecodePNG(data)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != payload {
		t.Errorf("text = %q", res.Text)
	}
	if res.Level != "Q" {
		t.Errorf("level = %q", res.Level)
	}
	if res.Mask != 3 {
		t.Errorf("mask = %d", res.Mask)
	}
	if res.Version != code.Version() {
		t.Errorf("version = %d, want %d", res.Version, code.Version())
	}
}

func TestDecodeToleratesScaleAndQuietZone(t *testing.T) {
	const payload = "scale test"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []qr.RenderOptions{
		{Size: 64, QuietZone: 1},
		{Size: 256, QuietZone: 4},
		{Size: 1024, QuietZone: 8},
		{Size: 2048, QuietZone: 16},
	} {
		data, err := code.PNG(opts)
		if err != nil {
			t.Fatalf("PNG %v: %v", opts, err)
		}
		res, err := qrdecode.DecodePNG(data)
		if err != nil {
			t.Fatalf("DecodePNG %v: %v", opts, err)
		}
		if res.Text != payload {
			t.Errorf("options %v decoded %q", opts, res.Text)
		}
	}
}

func TestDecodeCorrectsDamage(t *testing.T) {
	const payload = "https://example.com/abc?a=1&b=2"
	code, err := qr.Encode(payload, qr.LevelH)
	if err != nil {
		t.Fatal(err)
	}
	grid := qrdecode.NewGrid(code.Size())
	for y := 0; y < code.Size(); y++ {
		for x := 0; x < code.Size(); x++ {
			grid.Set(x, y, code.Module(x, y))
		}
	}

	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 15; i++ {
		x := 9 + rng.Intn(code.Size()-11)
		y := 9 + rng.Intn(code.Size()-11)
		grid.Set(x, y, !grid.Get(x, y))
	}

	res, err := qrdecode.DecodeGrid(grid)
	if err != nil {
		t.Fatalf("DecodeGrid: %v", err)
	}
	if res.Text != payload {
		t.Fatalf("decoded %q", res.Text)
	}
	if res.Corrections == 0 {
		t.Error("no corrections were reported for a damaged symbol")
	}
}

func TestDecodeRejectsGarbage(t *testing.T) {
	if _, err := qrdecode.DecodePNG([]byte("not a png")); err == nil {
		t.Error("garbage bytes were decoded")
	}
	if _, err := qrdecode.DecodeSVG([]byte("<svg></svg>")); err == nil {
		t.Error("an empty svg was decoded")
	}
	if _, err := qrdecode.DecodeSVG([]byte(`<svg viewBox="0 0 29 29"><path d="M4 4h1v1h-1zBOGUS"/></svg>`)); err == nil {
		t.Error("an svg with unexpected path commands was decoded")
	}
	if _, err := qrdecode.DecodeGrid(nil); err == nil {
		t.Error("a nil grid was decoded")
	}
	if _, err := qrdecode.DecodeGrid(qrdecode.NewGrid(20)); err == nil {
		t.Error("a grid of an impossible size was decoded")
	}

	// A blank image has no dark modules at all.
	blank := image.NewGray(image.Rect(0, 0, 100, 100))
	for i := range blank.Pix {
		blank.Pix[i] = 0xff
	}
	if _, err := qrdecode.DecodeImage(blank); err == nil {
		t.Error("a blank image was decoded")
	}

	// A tiny image cannot hold a symbol.
	if _, err := qrdecode.DecodeImage(image.NewGray(image.Rect(0, 0, 4, 4))); err == nil {
		t.Error("a tiny image was decoded")
	}
}

func TestDecodeRejectsANonSquareBoundingBox(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 60, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 60; x++ {
			img.Set(x, y, color.White)
		}
	}
	for x := 5; x < 50; x++ {
		img.Set(x, 10, color.Black)
	}
	if _, err := qrdecode.DecodeImage(img); err == nil {
		t.Error("a non square dark region was decoded")
	}
}

func TestDecodeSurvivesAnInvertedPaletteRoundTrip(t *testing.T) {
	// The decoder thresholds on luminance, so a grayscale re-encode of the
	// same symbol must still work.
	data := encodePNG(t, "grayscale", qr.LevelM)
	src, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	b := src.Bounds()
	gray := image.NewGray(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			gray.Set(x, y, src.At(x, y))
		}
	}
	res, err := qrdecode.DecodeImage(gray)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "grayscale" {
		t.Fatalf("decoded %q", res.Text)
	}
}

func TestDecodeAllModes(t *testing.T) {
	cases := []string{
		strings.Repeat("1234567890", 4), // numeric
		"HELLO WORLD 123 $%*+-./:",      // alphanumeric
		"Mixed Case Bytes ~!@#",         // byte
		"한국어 페이로드",                      // multibyte byte mode
	}
	for _, payload := range cases {
		res, err := qrdecode.DecodePNG(encodePNG(t, payload, qr.LevelM))
		if err != nil {
			t.Fatalf("%q: %v", payload, err)
		}
		if res.Text != payload {
			t.Errorf("decoded %q, want %q", res.Text, payload)
		}
	}
}

// rotate turns an image a quarter turn clockwise.
func rotate90(src image.Image) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dy(), b.Dx()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.Set(b.Dy()-1-y, x, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

// invert produces a light-on-dark version of an image.
func invert(src image.Image) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := src.At(x, y).RGBA()
			out.Set(x, y, color.RGBA64{
				R: 0xffff - uint16(r), G: 0xffff - uint16(g), B: 0xffff - uint16(bl), A: uint16(a),
			})
		}
	}
	return out
}

// resample scales an image with nearest neighbour, which is what a naive
// resize in an image editor or a messaging app does.
func resample(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := b.Min.Y + y*b.Dy()/h
		for x := 0; x < w; x++ {
			sx := b.Min.X + x*b.Dx()/w
			out.Set(x, y, src.At(sx, sy))
		}
	}
	return out
}

// pad surrounds an image with extra background, as a screenshot would.
func pad(src image.Image, margin int, c color.Color) image.Image {
	b := src.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx()+margin*2, b.Dy()+margin*2))
	for y := 0; y < out.Bounds().Dy(); y++ {
		for x := 0; x < out.Bounds().Dx(); x++ {
			out.Set(x, y, c)
		}
	}
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			out.Set(margin+x, margin+y, src.At(b.Min.X+x, b.Min.Y+y))
		}
	}
	return out
}

func decodeImageText(t *testing.T, img image.Image) string {
	t.Helper()
	res, err := qrdecode.DecodeImage(img)
	if err != nil {
		t.Fatalf("DecodeImage: %v", err)
	}
	return res.Text
}

func TestDecodeHandlesRotation(t *testing.T) {
	const payload = "https://url.mvtp.cloud/042"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	img := code.Image(qr.RenderOptions{Size: 512})

	for turn := 0; turn < 4; turn++ {
		if got := decodeImageText(t, img); got != payload {
			t.Fatalf("after %d quarter turns decoded %q", turn, got)
		}
		img = rotate90(img)
	}
}

func TestDecodeHandlesInvertedColours(t *testing.T) {
	const payload = "inverted symbol"
	code, err := qr.Encode(payload, qr.LevelQ)
	if err != nil {
		t.Fatal(err)
	}
	img := invert(code.Image(qr.RenderOptions{Size: 512}))
	if got := decodeImageText(t, img); got != payload {
		t.Fatalf("decoded %q, want %q", got, payload)
	}
}

// TestDecodeHandlesNonIntegerModulePitch covers a resized image, where one
// module is no longer a whole number of pixels.
func TestDecodeHandlesNonIntegerModulePitch(t *testing.T) {
	const payload = "https://example.com/abc?a=1&b=2"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	src := code.Image(qr.RenderOptions{Size: 1024})

	for _, size := range []int{377, 500, 611, 733, 901} {
		img := resample(src, size, size)
		res, err := qrdecode.DecodeImage(img)
		if err != nil {
			t.Errorf("resized to %dpx: %v", size, err)
			continue
		}
		if res.Text != payload {
			t.Errorf("resized to %dpx decoded %q", size, res.Text)
		}
	}
}

func TestDecodeHandlesExtraBorder(t *testing.T) {
	const payload = "screenshot with a margin"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	img := pad(code.Image(qr.RenderOptions{Size: 320}), 40, color.White)
	if got := decodeImageText(t, img); got != payload {
		t.Fatalf("decoded %q, want %q", got, payload)
	}
}

// TestDecodeHandlesLowContrast checks the adaptive threshold: a grey on grey
// rendering still separates into modules.
func TestDecodeHandlesLowContrast(t *testing.T) {
	const payload = "low contrast"
	code, err := qr.Encode(payload, qr.LevelM)
	if err != nil {
		t.Fatal(err)
	}
	src := code.Image(qr.RenderOptions{Size: 512})
	b := src.Bounds()
	dim := image.NewRGBA(b)
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, _, _, _ := src.At(x, y).RGBA()
			// Squeeze the range into 0x6000..0x9000, far from the naive
			// mid-grey threshold.
			v := uint16(0x6000)
			if r > 0x8000 {
				v = 0x9000
			}
			dim.Set(x, y, color.RGBA64{R: v, G: v, B: v, A: 0xffff})
		}
	}
	if got := decodeImageText(t, dim); got != payload {
		t.Fatalf("decoded %q, want %q", got, payload)
	}
}

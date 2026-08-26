package qrfile_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/qr"
	"github.com/mvtp/qrurl/internal/qrdecode"
	"github.com/mvtp/qrurl/internal/qrfile"
)

func newStore(t *testing.T) *qrfile.Store {
	t.Helper()
	s, err := qrfile.New(filepath.Join(t.TempDir(), "qr"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestRenderWritesDecodableFiles(t *testing.T) {
	s := newStore(t)
	const id = "0123456789abcdef"
	const payload = "https://url.mvtp.cloud/001"

	out, err := s.Render(id, payload, qr.LevelQ, 512)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if out.PNGName != id+".png" || out.SVGName != id+".svg" {
		t.Fatalf("unexpected names: %+v", out)
	}
	if out.Version < 1 {
		t.Fatalf("version = %d", out.Version)
	}

	png, err := s.Read(out.PNGName)
	if err != nil {
		t.Fatal(err)
	}
	res, err := qrdecode.DecodePNG(png)
	if err != nil {
		t.Fatalf("DecodePNG: %v", err)
	}
	if res.Text != payload {
		t.Fatalf("decoded %q, want %q", res.Text, payload)
	}
	if res.Level != "Q" {
		t.Fatalf("level = %s, want Q", res.Level)
	}

	svg, err := s.Read(out.SVGName)
	if err != nil {
		t.Fatal(err)
	}
	resSVG, err := qrdecode.DecodeSVG(svg)
	if err != nil {
		t.Fatalf("DecodeSVG: %v", err)
	}
	if resSVG.Text != payload {
		t.Fatalf("svg decoded %q, want %q", resSVG.Text, payload)
	}
}

func TestRenderOverwritesInPlace(t *testing.T) {
	s := newStore(t)
	const id = "aaaabbbbccccdddd"
	if _, err := s.Render(id, "first", qr.LevelM, 256); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Render(id, "second", qr.LevelM, 256); err != nil {
		t.Fatal(err)
	}
	png, err := s.Read(id + ".png")
	if err != nil {
		t.Fatal(err)
	}
	res, err := qrdecode.DecodePNG(png)
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "second" {
		t.Fatalf("decoded %q after re-render", res.Text)
	}

	// No temporary files are left behind.
	entries, err := os.ReadDir(s.Dir())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Fatalf("directory holds %v, want exactly the png and svg", names)
	}
}

func TestPathRejectsTraversal(t *testing.T) {
	s := newStore(t)
	bad := []string{
		"../../etc/passwd",
		"..%2fetc%2fpasswd",
		"/etc/passwd",
		"abc.txt",
		"ABCDEF0123456789.png",
		"short.png",
		"0123456789abcdef.php",
		"0123456789abcdef.png/../../x",
		"",
	}
	for _, name := range bad {
		if _, err := s.Path(name); err == nil {
			t.Errorf("Path(%q) was accepted", name)
		}
		if _, err := s.Read(name); err == nil {
			t.Errorf("Read(%q) was accepted", name)
		}
	}
}

func TestRemoveIsIdempotent(t *testing.T) {
	s := newStore(t)
	const id = "1122334455667788"
	out, err := s.Render(id, "payload", qr.LevelL, 256)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(out.PNGName, out.SVGName); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove(out.PNGName, out.SVGName, ""); err != nil {
		t.Fatalf("removing missing files should not fail: %v", err)
	}
	if _, err := s.Read(out.PNGName); !os.IsNotExist(err) {
		t.Fatalf("the file is still readable: %v", err)
	}
}

func TestSweepRemovesOrphansOnly(t *testing.T) {
	s := newStore(t)
	keep, err := s.Render("aaaa000011112222", "keep", qr.LevelM, 256)
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := s.Render("bbbb000011112222", "orphan", qr.LevelM, 256)
	if err != nil {
		t.Fatal(err)
	}
	// A file that does not belong to this package must be left alone.
	foreign := filepath.Join(s.Dir(), "README.txt")
	if err := os.WriteFile(foreign, []byte("hands off"), 0o600); err != nil {
		t.Fatal(err)
	}

	known := map[string]struct{}{keep.PNGName: {}, keep.SVGName: {}}

	// Within the grace period nothing is touched.
	n, err := s.Sweep(known, time.Hour, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("the sweep removed %d files inside the grace period", n)
	}

	// Once the grace period has passed the orphan goes.
	n, err = s.Sweep(known, 0, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("the sweep removed %d files, want the orphan png and svg", n)
	}
	if _, err := s.Read(keep.PNGName); err != nil {
		t.Fatalf("a referenced file was removed: %v", err)
	}
	if _, err := s.Read(orphan.PNGName); !os.IsNotExist(err) {
		t.Fatal("the orphan survived")
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("an unrelated file was removed: %v", err)
	}
}

func TestRenderRejectsBadIdentifierAndOversizedPayload(t *testing.T) {
	s := newStore(t)
	if _, err := s.Render("../escape", "x", qr.LevelM, 256); err == nil {
		t.Error("a traversal identifier was accepted")
	}
	if _, err := s.Render("0123456789abcdef", "", qr.LevelM, 256); err == nil {
		t.Error("an empty payload was accepted")
	}
	big := make([]byte, qr.MaxPayloadBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := s.Render("0123456789abcdef", string(big), qr.LevelL, 256); err == nil {
		t.Error("an oversized payload was accepted")
	}
}

func TestNewRequiresADirectory(t *testing.T) {
	if _, err := qrfile.New(""); err == nil {
		t.Error("an empty directory should be rejected")
	}
}

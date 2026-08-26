package cleanup_test

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mvtp/qrurl/internal/cleanup"
	"github.com/mvtp/qrurl/internal/qr"
	"github.com/mvtp/qrurl/internal/qrfile"
	"github.com/mvtp/qrurl/internal/store"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type env struct {
	db      *store.DB
	files   *qrfile.Store
	cleaner *cleanup.Cleaner
}

func newEnv(t *testing.T, interval time.Duration) env {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "test.db"), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	db.SetShortBaseURL("https://url.mvtp.cloud")
	files, err := qrfile.New(filepath.Join(dir, "qr"))
	if err != nil {
		t.Fatal(err)
	}
	return env{db: db, files: files, cleaner: cleanup.New(db, files, quietLogger(), interval)}
}

// seedQR renders a QR and stores its record, returning the file names.
func seedQR(t *testing.T, e env, id, payload string, shortCode *string, expiresAt *time.Time, now time.Time) (string, string) {
	t.Helper()
	rendered, err := e.files.Render(id, payload, qr.LevelM, 256)
	if err != nil {
		t.Fatal(err)
	}
	kind := store.KindText
	if shortCode != nil {
		kind = store.KindShort
	}
	if _, err := e.db.CreateQRCode(context.Background(), store.QRCodeInput{
		ID: id, Kind: kind, Payload: payload, ShortCode: shortCode,
		ECLevel: "M", Size: 256, Version: rendered.Version,
		PNGPath: rendered.PNGName, SVGPath: rendered.SVGName, ExpiresAt: expiresAt,
	}, now); err != nil {
		t.Fatal(err)
	}
	return rendered.PNGName, rendered.SVGName
}

func fileExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func TestRunOnceRemovesExpiredRecordsAndFiles(t *testing.T) {
	e := newEnv(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	future := now.Add(time.Hour)

	// An expired link with a linked QR.
	expiredLink, err := e.db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "001", TargetURL: "https://example.com/gone", Enabled: true, ExpiresAt: &past,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	cascadePNG, cascadeSVG := seedQR(t, e, "aaaaaaaaaaaaaaaa", "https://url.mvtp.cloud/001", &expiredLink.Code, nil, now)

	// A QR that expires on its own.
	orphanPNG, orphanSVG := seedQR(t, e, "bbbbbbbbbbbbbbbb", "expired text", nil, &past, now)

	// Everything below must survive.
	liveLink, err := e.db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "002", TargetURL: "https://example.com/live", Enabled: true, ExpiresAt: &future,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	livePNG, _ := seedQR(t, e, "cccccccccccccccc", "https://url.mvtp.cloud/002", &liveLink.Code, nil, now)

	res, err := e.cleaner.RunOnce(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatal("the sweep was skipped")
	}
	if res.ShortURLs != 1 || res.QRCodes != 2 {
		t.Fatalf("removed %d short urls and %d qr codes, want 1 and 2", res.ShortURLs, res.QRCodes)
	}

	dir := e.files.Dir()
	for _, name := range []string{cascadePNG, cascadeSVG, orphanPNG, orphanSVG} {
		if fileExists(t, dir, name) {
			t.Errorf("the file %s of an expired record still exists", name)
		}
	}
	if !fileExists(t, dir, livePNG) {
		t.Error("a live QR file was deleted")
	}
	if _, err := e.db.ShortURLByCode(ctx, "002"); err != nil {
		t.Errorf("the live short URL was deleted: %v", err)
	}
}

func TestRunOnceRemovesOrphanFiles(t *testing.T) {
	e := newEnv(t, 0)
	// A file with no database record at all, aged past the grace period.
	stray := filepath.Join(e.files.Dir(), "deadbeefdeadbeef.png")
	if err := os.WriteFile(stray, []byte("stray"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stray, old, old); err != nil {
		t.Fatal(err)
	}

	res, err := e.cleaner.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Orphans != 1 {
		t.Fatalf("orphans removed = %d, want 1", res.Orphans)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatal("the orphan file survived")
	}
}

// TestConcurrentRunsAreSerialised runs many sweeps at once; the lease must let
// exactly one through per round and the rest must report a clean skip.
func TestConcurrentRunsAreSerialised(t *testing.T) {
	e := newEnv(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)

	for i := 1; i <= 20; i++ {
		if _, err := e.db.CreateShortURL(ctx, store.ShortURLInput{
			TargetURL: "https://example.com/x", Enabled: true, ExpiresAt: &past,
		}, now); err != nil {
			t.Fatal(err)
		}
	}

	const workers = 8
	var wg sync.WaitGroup
	results := make([]cleanup.Result, workers)
	errs := make([]error, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = e.cleaner.RunOnce(ctx)
		}(i)
	}
	wg.Wait()

	removed := 0
	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
		removed += results[i].ShortURLs
	}
	if removed != 20 {
		t.Fatalf("the workers together removed %d links, want 20 with no double counting", removed)
	}
	left, err := e.db.ListShortURLs(ctx, store.ShortURLFilter{}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("%d expired links survived", len(left))
	}
}

func TestRunPerformsAStartupSweepAndStops(t *testing.T) {
	e := newEnv(t, 50*time.Millisecond)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	if _, err := e.db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "001", TargetURL: "https://example.com", Enabled: true, ExpiresAt: &past,
	}, now); err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		e.cleaner.Run(runCtx)
		close(done)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		links, err := e.db.ListShortURLs(ctx, store.ShortURLFilter{}, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(links) == 0 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("the startup sweep did not remove the expired link")
		}
		time.Sleep(20 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestTouchIsThrottledAndAsynchronous(t *testing.T) {
	e := newEnv(t, 0)
	ctx := context.Background()
	now := time.Now().UTC()
	past := now.Add(-time.Minute)
	if _, err := e.db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "001", TargetURL: "https://example.com", Enabled: true, ExpiresAt: &past,
	}, now); err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 5; i++ {
		e.cleaner.Touch(ctx)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		links, err := e.db.ListShortURLs(ctx, store.ShortURLFilter{}, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(links) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Touch never triggered a sweep")
		}
		time.Sleep(20 * time.Millisecond)
	}

	status, err := e.db.CleanupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Five calls in a burst must not turn into five sweeps.
	if status.Runs > 2 {
		t.Fatalf("Touch triggered %d sweeps, throttling is not working", status.Runs)
	}
}

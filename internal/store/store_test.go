package store_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/mvtp/qrurl/internal/store"
)

func newDB(t *testing.T) *store.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.db")
	db, err := store.Open(path, time.UTC)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	db.SetShortBaseURL("https://url.mvtp.cloud")
	return db
}

var baseTime = time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

func mustCreate(t *testing.T, db *store.DB, in store.ShortURLInput, now time.Time) store.ShortURL {
	t.Helper()
	link, err := db.CreateShortURL(context.Background(), in, now)
	if err != nil {
		t.Fatalf("CreateShortURL: %v", err)
	}
	return link
}

func TestMigrateIsIdempotent(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := db.Migrate(ctx); err != nil {
			t.Fatalf("Migrate run %d: %v", i, err)
		}
	}
	v, err := db.SchemaVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v < 1 {
		t.Fatalf("schema version = %d, want at least 1", v)
	}
}

func TestWALIsEnabled(t *testing.T) {
	db := newDB(t)
	var mode string
	if err := db.SQL().QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", mode)
	}
	var fk int
	if err := db.SQL().QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatal(err)
	}
	if fk != 1 {
		t.Fatal("foreign key enforcement is off")
	}
}

func TestAutomaticCodeAllocation(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	for i, want := range []string{"001", "002", "003"} {
		link := mustCreate(t, db, store.ShortURLInput{
			TargetURL: fmt.Sprintf("https://example.com/%d", i), Enabled: true,
		}, baseTime)
		if link.Code != want {
			t.Fatalf("automatic code %d = %q, want %q", i, link.Code, want)
		}
	}

	// Freeing a number makes it available again, and it is the lowest one.
	second, err := db.ShortURLByCode(ctx, "002")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.DeleteShortURL(ctx, second.ID); err != nil {
		t.Fatal(err)
	}
	link := mustCreate(t, db, store.ShortURLInput{TargetURL: "https://example.com/reuse", Enabled: true}, baseTime)
	if link.Code != "002" {
		t.Fatalf("after freeing 002 the next automatic code was %q", link.Code)
	}
}

func TestManualCodeAndBoundaries(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	for _, code := range []string{"001", "999", "500"} {
		link := mustCreate(t, db, store.ShortURLInput{
			Code: code, TargetURL: "https://example.com/" + code, Enabled: true,
		}, baseTime)
		if link.Code != code {
			t.Fatalf("manual code = %q, want %q", link.Code, code)
		}
	}

	// The database itself refuses codes outside the documented range.
	for _, code := range []string{"000", "0000", "12", "abc", "1234"} {
		if _, err := db.CreateShortURL(ctx, store.ShortURLInput{
			Code: code, TargetURL: "https://example.com/bad", Enabled: true,
		}, baseTime); err == nil {
			t.Errorf("the store accepted the invalid code %q", code)
		}
	}
}

func TestDuplicateCodeIsRejected(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustCreate(t, db, store.ShortURLInput{Code: "123", TargetURL: "https://example.com/a", Enabled: true}, baseTime)
	_, err := db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "123", TargetURL: "https://example.com/b", Enabled: true,
	}, baseTime)
	if !errors.Is(err, store.ErrCodeTaken) {
		t.Fatalf("duplicate create error = %v, want ErrCodeTaken", err)
	}
}

func TestCodeExhaustion(t *testing.T) {
	if testing.Short() {
		t.Skip("filling all 999 codes takes a moment")
	}
	db := newDB(t)
	ctx := context.Background()
	for i := 1; i <= 999; i++ {
		if _, err := db.CreateShortURL(ctx, store.ShortURLInput{
			Code: fmt.Sprintf("%03d", i), TargetURL: "https://example.com/x", Enabled: true,
		}, baseTime); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	_, err := db.CreateShortURL(ctx, store.ShortURLInput{TargetURL: "https://example.com/full", Enabled: true}, baseTime)
	if !errors.Is(err, store.ErrNoCodesLeft) {
		t.Fatalf("error after filling the range = %v, want ErrNoCodesLeft", err)
	}
	codes, err := db.UsedCodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(codes) != 999 || codes[0] != "001" || codes[998] != "999" {
		t.Fatalf("used codes span %d entries from %q to %q", len(codes), codes[0], codes[len(codes)-1])
	}
}

func TestUpdateShortURL(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	link := mustCreate(t, db, store.ShortURLInput{
		Code: "007", TargetURL: "https://example.com/old", Note: "old", Enabled: true,
	}, baseTime)

	newTarget := "https://example.com/new"
	disabled := false
	expiry := baseTime.Add(time.Hour)
	expiryPtr := &expiry
	updated, err := db.UpdateShortURL(ctx, link.ID, store.ShortURLUpdate{
		TargetURL: &newTarget,
		Enabled:   &disabled,
		ExpiresAt: &expiryPtr,
	}, baseTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if updated.TargetURL != newTarget || updated.Enabled || updated.ExpiresAt == nil {
		t.Fatalf("update did not apply: %+v", updated)
	}
	if !updated.UpdatedAt.After(link.UpdatedAt) {
		t.Error("updated_at was not refreshed")
	}

	// Clearing the expiry.
	var none *time.Time
	updated, err = db.UpdateShortURL(ctx, link.ID, store.ShortURLUpdate{ExpiresAt: &none}, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ExpiresAt != nil {
		t.Fatal("the expiry was not cleared")
	}

	if _, err := db.UpdateShortURL(ctx, 99999, store.ShortURLUpdate{Note: &newTarget}, baseTime); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("updating a missing row returned %v", err)
	}
	if _, err := db.UpdateShortURL(ctx, link.ID, store.ShortURLUpdate{}, baseTime); !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("an empty update returned %v", err)
	}
}

func TestRenamingACodeRewritesLinkedQRPayloads(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	link := mustCreate(t, db, store.ShortURLInput{Code: "010", TargetURL: "https://example.com", Enabled: true}, baseTime)

	code := link.Code
	if _, err := db.CreateQRCode(ctx, store.QRCodeInput{
		ID: "aabbccdd00112233", Kind: store.KindShort, Payload: db.ShortPayload(code),
		ShortCode: &code, ECLevel: "M", Size: 512, PNGPath: "aabbccdd00112233.png",
	}, baseTime); err != nil {
		t.Fatal(err)
	}

	newCode := "020"
	if _, err := db.UpdateShortURL(ctx, link.ID, store.ShortURLUpdate{Code: &newCode}, baseTime); err != nil {
		t.Fatal(err)
	}
	records, err := db.QRCodesByShortCode(ctx, newCode)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("the foreign key did not follow the rename: %d records", len(records))
	}
	if want := "https://url.mvtp.cloud/020"; records[0].Payload != want {
		t.Fatalf("payload = %q, want %q", records[0].Payload, want)
	}
}

func TestDeleteShortURLCascadesToQRCodes(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	link := mustCreate(t, db, store.ShortURLInput{Code: "042", TargetURL: "https://example.com", Enabled: true}, baseTime)
	code := link.Code
	if _, err := db.CreateQRCode(ctx, store.QRCodeInput{
		ID: "1111222233334444", Kind: store.KindShort, Payload: db.ShortPayload(code), ShortCode: &code,
		ECLevel: "M", Size: 512, PNGPath: "1111222233334444.png", SVGPath: "1111222233334444.svg",
	}, baseTime); err != nil {
		t.Fatal(err)
	}

	deleted, files, err := db.DeleteShortURL(ctx, link.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.Code != "042" {
		t.Fatalf("deleted %q", deleted.Code)
	}
	if len(files) != 2 {
		t.Fatalf("expected two files to unlink, got %v", files)
	}
	if _, err := db.QRCodeByID(ctx, "1111222233334444"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the linked QR survived the cascade: %v", err)
	}
	if _, err := db.ShortURLByCode(ctx, "042"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the short URL survived: %v", err)
	}
}

func TestRecordRedirectAndStats(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	link := mustCreate(t, db, store.ShortURLInput{Code: "100", TargetURL: "https://example.com", Enabled: true}, baseTime)

	for i := 0; i < 3; i++ {
		if err := db.RecordRedirect(ctx, link.ID, baseTime.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := db.ShortURLByCode(ctx, "100")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.ClickCount != 3 {
		t.Fatalf("click count = %d, want 3", reloaded.ClickCount)
	}
	if reloaded.LastAccessedAt == nil {
		t.Fatal("last accessed was not recorded")
	}

	dash, err := db.Stats(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if dash.Today.Redirects != 3 {
		t.Errorf("today redirects = %d, want 3", dash.Today.Redirects)
	}
	if dash.Today.URLsCreated != 1 {
		t.Errorf("today created urls = %d, want 1", dash.Today.URLsCreated)
	}
	if dash.ActiveShortURLs != 1 || dash.TotalShortURLs != 1 || dash.FreeCodes != 998 {
		t.Errorf("unexpected dashboard counters: %+v", dash)
	}
	if dash.TotalClicks != 3 {
		t.Errorf("total clicks = %d, want 3", dash.TotalClicks)
	}
}

// TestStatsSurviveDeletion checks that the daily counters are not derived from
// the rows themselves, so deleting an entry does not rewrite history.
func TestStatsSurviveDeletion(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	link := mustCreate(t, db, store.ShortURLInput{TargetURL: "https://example.com", Enabled: true}, baseTime)
	if err := db.RecordRedirect(ctx, link.ID, baseTime); err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.DeleteShortURL(ctx, link.ID); err != nil {
		t.Fatal(err)
	}
	dash, err := db.Stats(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if dash.Today.Redirects != 1 || dash.Today.URLsCreated != 1 {
		t.Fatalf("history was lost after deletion: %+v", dash.Today)
	}
}

func TestListFiltersAndSorting(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	mustCreate(t, db, store.ShortURLInput{Code: "001", TargetURL: "https://alpha.example.com", Note: "첫번째", Enabled: true}, baseTime)
	mustCreate(t, db, store.ShortURLInput{Code: "002", TargetURL: "https://beta.example.com", Enabled: false}, baseTime)
	soon := baseTime.Add(time.Hour)
	mustCreate(t, db, store.ShortURLInput{Code: "003", TargetURL: "https://gamma.example.com", Enabled: true, ExpiresAt: &soon}, baseTime)
	past := baseTime.Add(-time.Hour)
	mustCreate(t, db, store.ShortURLInput{Code: "004", TargetURL: "https://delta.example.com", Enabled: true, ExpiresAt: &past}, baseTime)

	cases := []struct {
		status string
		want   []string
	}{
		{"", []string{"001", "002", "003", "004"}},
		{"active", []string{"001", "003"}},
		{"disabled", []string{"002"}},
		{"expiring", []string{"003"}},
		{"expired", []string{"004"}},
	}
	for _, c := range cases {
		got, err := db.ListShortURLs(ctx, store.ShortURLFilter{Status: c.status, Sort: "code", Order: "asc"}, baseTime)
		if err != nil {
			t.Fatalf("status %q: %v", c.status, err)
		}
		codes := make([]string, len(got))
		for i, g := range got {
			codes[i] = g.Code
		}
		if fmt.Sprint(codes) != fmt.Sprint(c.want) {
			t.Errorf("status %q gave %v, want %v", c.status, codes, c.want)
		}
	}

	if _, err := db.ListShortURLs(ctx, store.ShortURLFilter{Status: "bogus"}, baseTime); !errors.Is(err, store.ErrInvalidInput) {
		t.Error("an unknown status filter should be rejected")
	}

	// Descending order and an unknown sort key falling back to the code.
	desc, err := db.ListShortURLs(ctx, store.ShortURLFilter{Sort: "not-a-column", Order: "desc"}, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if desc[0].Code != "004" {
		t.Errorf("descending list starts at %q, want 004", desc[0].Code)
	}
}

// TestSearchIsParameterised feeds classic injection payloads through the search
// filter; they must be treated as literal text.
func TestSearchIsParameterised(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustCreate(t, db, store.ShortURLInput{Code: "001", TargetURL: "https://example.com/safe", Enabled: true}, baseTime)
	mustCreate(t, db, store.ShortURLInput{Code: "002", TargetURL: "https://example.com/other", Note: "100% sure", Enabled: true}, baseTime)

	payloads := []string{
		"' OR '1'='1",
		"'; DROP TABLE short_urls; --",
		"\" OR 1=1 --",
		"1) UNION SELECT password_hash FROM admin --",
		"_",
		"\\",
	}
	for _, p := range payloads {
		got, err := db.ListShortURLs(ctx, store.ShortURLFilter{Query: p}, baseTime)
		if err != nil {
			t.Fatalf("search %q returned an error: %v", p, err)
		}
		if len(got) != 0 {
			t.Errorf("search %q matched %d rows; wildcards and quotes must be literal", p, len(got))
		}
	}

	// A bare percent sign is escaped, so it matches the literal character in
	// the note rather than acting as a wildcard that returns everything.
	if got, err := db.ListShortURLs(ctx, store.ShortURLFilter{Query: "%"}, baseTime); err != nil || len(got) != 1 {
		t.Fatalf("searching for %%%% returned %d rows, err %v; want only the row containing a literal percent sign", len(got), err)
	}
	if got, err := db.ListShortURLs(ctx, store.ShortURLFilter{Query: "100% sure"}, baseTime); err != nil || len(got) != 1 {
		t.Fatalf("literal search for a percent sign returned %d rows, err %v", len(got), err)
	}
	all, err := db.ListShortURLs(ctx, store.ShortURLFilter{}, baseTime)
	if err != nil || len(all) != 2 {
		t.Fatalf("the table is damaged: %d rows, err %v", len(all), err)
	}
}

func TestQRCodeLifecycle(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	expires := baseTime.Add(24 * time.Hour)
	record, err := db.CreateQRCode(ctx, store.QRCodeInput{
		ID: "abcdef0123456789", Kind: store.KindText, Payload: "hello", Label: "인사",
		ECLevel: "H", Size: 1024, Version: 2,
		PNGPath: "abcdef0123456789.png", SVGPath: "abcdef0123456789.svg",
		ExpiresAt: &expires,
	}, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if record.Revocable() {
		t.Error("a text QR must not claim it can be revoked")
	}

	label := "새 이름"
	updated, err := db.UpdateQRCode(ctx, record.ID, store.QRCodeUpdate{Label: &label}, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != label {
		t.Fatalf("label = %q", updated.Label)
	}

	deleted, files, err := db.DeleteQRCode(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if deleted.ID != record.ID || len(files) != 2 {
		t.Fatalf("delete returned %+v with files %v", deleted, files)
	}
	if _, err := db.QRCodeByID(ctx, record.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the record survived deletion: %v", err)
	}
	if _, _, err := db.DeleteQRCode(ctx, record.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleting twice returned %v", err)
	}
}

func TestDeleteExpiredSweep(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	past := baseTime.Add(-time.Minute)
	future := baseTime.Add(time.Hour)

	expiredLink := mustCreate(t, db, store.ShortURLInput{Code: "001", TargetURL: "https://example.com/gone", Enabled: true, ExpiresAt: &past}, baseTime)
	liveLink := mustCreate(t, db, store.ShortURLInput{Code: "002", TargetURL: "https://example.com/live", Enabled: true, ExpiresAt: &future}, baseTime)
	mustCreate(t, db, store.ShortURLInput{Code: "003", TargetURL: "https://example.com/forever", Enabled: true}, baseTime)

	// A QR attached to the expired link, one that expires on its own and one
	// that must survive.
	cascade := expiredLink.Code
	live := liveLink.Code
	for _, in := range []store.QRCodeInput{
		{ID: "aaaaaaaaaaaaaaaa", Kind: store.KindShort, Payload: "x", ShortCode: &cascade, ECLevel: "M", Size: 512, PNGPath: "aaaaaaaaaaaaaaaa.png"},
		{ID: "bbbbbbbbbbbbbbbb", Kind: store.KindText, Payload: "y", ECLevel: "M", Size: 512, PNGPath: "bbbbbbbbbbbbbbbb.png", ExpiresAt: &past},
		{ID: "cccccccccccccccc", Kind: store.KindShort, Payload: "z", ShortCode: &live, ECLevel: "M", Size: 512, PNGPath: "cccccccccccccccc.png"},
	} {
		if _, err := db.CreateQRCode(ctx, in, baseTime); err != nil {
			t.Fatal(err)
		}
	}

	sweep, err := db.DeleteExpired(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if !sweep.Removed() {
		t.Fatal("the sweep reported nothing removed")
	}
	if len(sweep.ShortURLs) != 1 || sweep.ShortURLs[0] != "001" {
		t.Fatalf("removed short urls = %v", sweep.ShortURLs)
	}
	if len(sweep.QRCodes) != 2 {
		t.Fatalf("removed qr codes = %v, want two", sweep.QRCodes)
	}
	if len(sweep.Files) != 2 {
		t.Fatalf("files to unlink = %v", sweep.Files)
	}
	if _, err := db.QRCodeByID(ctx, "cccccccccccccccc"); err != nil {
		t.Fatalf("the live QR was removed: %v", err)
	}
	if _, err := db.ShortURLByCode(ctx, "002"); err != nil {
		t.Fatalf("the live short URL was removed: %v", err)
	}

	// Running again removes nothing more.
	again, err := db.DeleteExpired(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if again.Removed() {
		t.Fatalf("the second sweep removed %+v", again)
	}
}

func TestDeleteExpiredShortURLOnAccess(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	past := baseTime.Add(-time.Second)
	mustCreate(t, db, store.ShortURLInput{Code: "055", TargetURL: "https://example.com", Enabled: true, ExpiresAt: &past}, baseTime)

	removed, _, err := db.DeleteExpiredShortURL(ctx, "055", baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("the expired link was not removed")
	}
	removed, _, err = db.DeleteExpiredShortURL(ctx, "055", baseTime)
	if err != nil || removed {
		t.Fatalf("a second removal reported %v, %v", removed, err)
	}

	// A live link is never touched by this path.
	mustCreate(t, db, store.ShortURLInput{Code: "056", TargetURL: "https://example.com", Enabled: true}, baseTime)
	removed, _, err = db.DeleteExpiredShortURL(ctx, "056", baseTime)
	if err != nil || removed {
		t.Fatalf("a live link was removed: %v, %v", removed, err)
	}
}

func TestCleanupLeaseIsExclusive(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	first, err := db.AcquireCleanupLease(ctx, baseTime, time.Minute)
	if err != nil || !first {
		t.Fatalf("the first lease was not granted: %v, %v", first, err)
	}
	second, err := db.AcquireCleanupLease(ctx, baseTime, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if second {
		t.Fatal("a second lease was granted while the first was held")
	}

	if err := db.ReleaseCleanupLease(ctx, baseTime, 2, 3); err != nil {
		t.Fatal(err)
	}
	third, err := db.AcquireCleanupLease(ctx, baseTime, time.Minute)
	if err != nil || !third {
		t.Fatalf("the lease was not released: %v, %v", third, err)
	}

	// A crashed holder cannot block forever.
	later := baseTime.Add(2 * time.Minute)
	fourth, err := db.AcquireCleanupLease(ctx, later, time.Minute)
	if err != nil || !fourth {
		t.Fatalf("an expired lease was not reclaimed: %v, %v", fourth, err)
	}

	status, err := db.CleanupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.RemovedURLs != 2 || status.RemovedQRs != 3 {
		t.Fatalf("cleanup bookkeeping = %+v", status)
	}
}

// TestConcurrentLeaseGrantsExactlyOne mirrors several cleanup workers racing.
func TestConcurrentLeaseGrantsExactlyOne(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	const workers = 12
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			ok, err := db.AcquireCleanupLease(ctx, baseTime, time.Minute)
			if err != nil {
				t.Errorf("AcquireCleanupLease: %v", err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if granted != 1 {
		t.Fatalf("%d workers were granted the lease, want exactly 1", granted)
	}
}

func TestConcurrentCreatesDoNotCollide(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	const workers = 20
	var wg sync.WaitGroup
	codes := make([]string, workers)
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			defer wg.Done()
			link, err := db.CreateShortURL(ctx, store.ShortURLInput{
				TargetURL: fmt.Sprintf("https://example.com/%d", i), Enabled: true,
			}, baseTime)
			if err != nil {
				t.Errorf("worker %d: %v", i, err)
				return
			}
			codes[i] = link.Code
		}(i)
	}
	wg.Wait()

	seen := map[string]bool{}
	for _, c := range codes {
		if c == "" {
			t.Fatal("a worker failed to allocate a code")
		}
		if seen[c] {
			t.Fatalf("code %q was handed out twice", c)
		}
		seen[c] = true
	}
	if len(seen) != workers {
		t.Fatalf("got %d distinct codes, want %d", len(seen), workers)
	}
}

func TestAdminAccount(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	has, err := db.HasAdmin(ctx)
	if err != nil || has {
		t.Fatalf("a fresh database reported an admin: %v, %v", has, err)
	}
	if _, err := db.Admin(ctx); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Admin on an empty database returned %v", err)
	}
	if err := db.SetAdmin(ctx, "", "hash", baseTime); !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("SetAdmin with an empty name returned %v", err)
	}

	if err := db.SetAdmin(ctx, "admin", "hash-1", baseTime); err != nil {
		t.Fatal(err)
	}
	admin, err := db.Admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if admin.Username != "admin" || admin.PasswordHash != "hash-1" {
		t.Fatalf("admin = %+v", admin)
	}

	// Changing the password drops every session.
	if err := db.CreateSession(ctx, "token-hash", "csrf", baseTime, baseTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetAdmin(ctx, "admin", "hash-2", baseTime); err != nil {
		t.Fatal(err)
	}
	if n, err := db.CountSessions(ctx); err != nil || n != 0 {
		t.Fatalf("sessions after a password change = %d, err %v", n, err)
	}

	// Only one row can ever exist.
	var count int
	if err := db.SQL().QueryRow("SELECT COUNT(*) FROM admin").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("the admin table holds %d rows", count)
	}
}

func TestSessions(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	if err := db.CreateSession(ctx, "hash-a", "csrf-a", baseTime, baseTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sess, err := db.Session(ctx, "hash-a", baseTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if sess.CSRFToken != "csrf-a" {
		t.Fatalf("csrf token = %q", sess.CSRFToken)
	}

	if _, err := db.Session(ctx, "unknown", baseTime); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an unknown session returned %v", err)
	}

	// An expired session is reported as missing and cleaned up on the spot.
	if _, err := db.Session(ctx, "hash-a", baseTime.Add(2*time.Hour)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("an expired session was accepted")
	}
	if n, err := db.CountSessions(ctx); err != nil || n != 0 {
		t.Fatalf("the expired session was not removed: %d, %v", n, err)
	}

	if err := db.CreateSession(ctx, "hash-b", "csrf-b", baseTime, baseTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteSession(ctx, "hash-b"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Session(ctx, "hash-b", baseTime); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a deleted session is still usable")
	}

	if err := db.CreateSession(ctx, "hash-c", "csrf-c", baseTime, baseTime.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	n, err := db.DeleteExpiredSessions(ctx, baseTime)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpiredSessions = %d, %v", n, err)
	}
}

func TestBackup(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	mustCreate(t, db, store.ShortURLInput{Code: "321", TargetURL: "https://example.com", Enabled: true}, baseTime)

	target := filepath.Join(t.TempDir(), "backup.db")
	if err := db.Backup(ctx, target); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(target)
	if err != nil || info.Size() == 0 {
		t.Fatalf("the backup file is missing or empty: %v", err)
	}
	if err := db.Backup(ctx, target); err == nil {
		t.Error("overwriting an existing backup should be refused")
	}

	restored, err := store.Open(target, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if _, err := restored.ShortURLByCode(ctx, "321"); err != nil {
		t.Fatalf("the backup does not contain the data: %v", err)
	}
}

func TestKnownQRFiles(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if _, err := db.CreateQRCode(ctx, store.QRCodeInput{
		ID: "0f0f0f0f0f0f0f0f", Kind: store.KindText, Payload: "x", ECLevel: "M", Size: 512,
		PNGPath: "0f0f0f0f0f0f0f0f.png", SVGPath: "0f0f0f0f0f0f0f0f.svg",
	}, baseTime); err != nil {
		t.Fatal(err)
	}
	known, err := db.KnownQRFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(known) != 2 {
		t.Fatalf("known files = %v", known)
	}
	if _, ok := known["0f0f0f0f0f0f0f0f.png"]; !ok {
		t.Fatal("the PNG is missing from the known set")
	}
}

func TestPersistenceAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")
	ctx := context.Background()

	db, err := store.Open(path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	db.SetShortBaseURL("https://url.mvtp.cloud")
	link, err := db.CreateShortURL(ctx, store.ShortURLInput{
		Code: "777", TargetURL: "https://example.com/persist", Enabled: true,
	}, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.RecordRedirect(ctx, link.ID, baseTime); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.Open(path, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := reopened.ShortURLByCode(ctx, "777")
	if err != nil {
		t.Fatalf("the row did not survive the restart: %v", err)
	}
	if got.TargetURL != "https://example.com/persist" || got.ClickCount != 1 {
		t.Fatalf("restored row = %+v", got)
	}
}

func TestAuditLog(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	if entries, err := db.AuditLog(ctx, 10); err != nil || len(entries) != 0 {
		t.Fatalf("a fresh log has %d entries, err %v", len(entries), err)
	}

	for i := 0; i < 5; i++ {
		err := db.RecordAudit(ctx, store.AuditEntry{
			CreatedAt: baseTime.Add(time.Duration(i) * time.Minute),
			Action:    store.AuditShortCreate,
			Subject:   fmt.Sprintf("/%03d", i+1),
			Detail:    "https://example.com",
			IP:        "203.0.113.5",
			Success:   true,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	entries, err := db.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want 5", len(entries))
	}
	// Newest first.
	if entries[0].Subject != "/005" {
		t.Errorf("the log is not ordered newest first: %q", entries[0].Subject)
	}
	if entries[0].IP != "203.0.113.5" || !entries[0].Success {
		t.Errorf("entry not stored faithfully: %+v", entries[0])
	}

	// The limit is honoured and bounded.
	if got, _ := db.AuditLog(ctx, 2); len(got) != 2 {
		t.Errorf("limit 2 returned %d entries", len(got))
	}
	if got, _ := db.AuditLog(ctx, 0); len(got) != 5 {
		t.Errorf("limit 0 returned %d entries, want the default page", len(got))
	}

	// Failures are recorded as failures and counted separately.
	if err := db.RecordAudit(ctx, store.AuditEntry{
		CreatedAt: baseTime, Action: store.AuditLoginFailed, Success: false,
	}); err != nil {
		t.Fatal(err)
	}
	n, err := db.FailedLoginsSince(ctx, baseTime.Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("failed logins = %d, %v", n, err)
	}
	if n, _ := db.FailedLoginsSince(ctx, baseTime.Add(time.Hour)); n != 0 {
		t.Errorf("a later cutoff still counted %d entries", n)
	}
}

func TestAuditFieldsAreTruncated(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	huge := strings.Repeat("가", 5000)
	if err := db.RecordAudit(ctx, store.AuditEntry{
		CreatedAt: baseTime, Action: store.AuditShortCreate, Subject: huge, Detail: huge, Success: true,
	}); err != nil {
		t.Fatal(err)
	}
	entries, err := db.AuditLog(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries[0].Subject) > 400 || len(entries[0].Detail) > 400 {
		t.Fatalf("fields were not truncated: %d / %d bytes",
			len(entries[0].Subject), len(entries[0].Detail))
	}
	if !utf8.ValidString(entries[0].Subject) {
		t.Fatal("truncation produced invalid UTF-8")
	}
}

func TestCleanupPrunesTheAuditLog(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()

	old := baseTime.AddDate(0, 0, -200)
	recent := baseTime.AddDate(0, 0, -1)
	for _, when := range []time.Time{old, recent} {
		if err := db.RecordAudit(ctx, store.AuditEntry{
			CreatedAt: when, Action: store.AuditLogin, Success: true,
		}); err != nil {
			t.Fatal(err)
		}
	}

	sweep, err := db.DeleteExpired(ctx, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	if sweep.AuditRows != 1 {
		t.Fatalf("pruned %d audit rows, want 1", sweep.AuditRows)
	}
	entries, err := db.AuditLog(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !entries[0].CreatedAt.Equal(recent) {
		t.Fatalf("the wrong rows survived: %+v", entries)
	}
}

func TestUpdateAdminPasswordKeepsOneSession(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	if err := db.SetAdmin(ctx, "admin", "old-hash", baseTime); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"keep-me", "drop-me-1", "drop-me-2"} {
		if err := db.CreateSession(ctx, h, "csrf", baseTime, baseTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}

	revoked, err := db.UpdateAdminPassword(ctx, "new-hash", "keep-me", baseTime.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if revoked != 2 {
		t.Fatalf("revoked %d sessions, want 2", revoked)
	}
	if _, err := db.Session(ctx, "keep-me", baseTime.Add(2*time.Minute)); err != nil {
		t.Fatalf("the kept session was dropped: %v", err)
	}
	if _, err := db.Session(ctx, "drop-me-1", baseTime.Add(2*time.Minute)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("a revoked session survived")
	}

	admin, err := db.Admin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if admin.PasswordHash != "new-hash" || admin.Username != "admin" {
		t.Fatalf("admin = %+v", admin)
	}

	if _, err := db.UpdateAdminPassword(ctx, "", "keep-me", baseTime); !errors.Is(err, store.ErrInvalidInput) {
		t.Fatalf("an empty hash returned %v", err)
	}
}

func TestDeleteOtherSessions(t *testing.T) {
	db := newDB(t)
	ctx := context.Background()
	for _, h := range []string{"a", "b", "c"} {
		if err := db.CreateSession(ctx, h, "csrf", baseTime, baseTime.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	n, err := db.DeleteOtherSessions(ctx, "b")
	if err != nil || n != 2 {
		t.Fatalf("DeleteOtherSessions = %d, %v", n, err)
	}
	if left, _ := db.CountSessions(ctx); left != 1 {
		t.Fatalf("%d sessions left, want 1", left)
	}
}

func TestInstanceLockIsExclusive(t *testing.T) {
	dir := t.TempDir()

	first, err := store.AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("the first lock was refused: %v", err)
	}
	if first.Path() == "" {
		t.Error("the lock has no path")
	}

	if _, err := store.AcquireInstanceLock(dir); !errors.Is(err, store.ErrAlreadyRunning) {
		t.Fatalf("a second lock returned %v, want ErrAlreadyRunning", err)
	}
	// The message should name the holder so the operator can act on it.
	_, err = store.AcquireInstanceLock(dir)
	if !strings.Contains(err.Error(), "pid ") {
		t.Errorf("the error does not identify the holder: %v", err)
	}

	// A different directory is unaffected.
	other, err := store.AcquireInstanceLock(t.TempDir())
	if err != nil {
		t.Fatalf("an unrelated directory was locked out: %v", err)
	}
	if err := other.Release(); err != nil {
		t.Fatal(err)
	}

	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	// Once released the directory can be taken again.
	again, err := store.AcquireInstanceLock(dir)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatal(err)
	}
	// Releasing twice, or releasing a nil lock, is harmless.
	if err := again.Release(); err != nil {
		t.Fatalf("a second release failed: %v", err)
	}
	var nilLock *store.InstanceLock
	if err := nilLock.Release(); err != nil {
		t.Fatalf("releasing a nil lock failed: %v", err)
	}
	if nilLock.Path() != "" {
		t.Error("a nil lock reported a path")
	}
}

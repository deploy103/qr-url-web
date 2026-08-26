// Package cleanup removes expired short URLs and QR records from the database
// and deletes the QR files that belonged to them.
package cleanup

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mvtp/qrurl/internal/qrfile"
	"github.com/mvtp/qrurl/internal/store"
)

// leaseDuration bounds how long a crashed run can block the next one.
const leaseDuration = 2 * time.Minute

// orphanGrace protects freshly written files from the orphan sweep while the
// database transaction that will reference them is still open.
const orphanGrace = 10 * time.Minute

// touchInterval throttles the opportunistic cleanups triggered by requests.
const touchInterval = 30 * time.Second

// Result summarises one cleanup pass.
type Result struct {
	ShortURLs int  `json:"shortURLs"`
	QRCodes   int  `json:"qrCodes"`
	Files     int  `json:"files"`
	Orphans   int  `json:"orphans"`
	Sessions  int  `json:"sessions"`
	StatDays  int  `json:"statDays"`
	Skipped   bool `json:"skipped"`
}

// Cleaner runs the expiration sweep on demand and on a timer.
type Cleaner struct {
	db    *store.DB
	files *qrfile.Store
	log   *slog.Logger

	interval  time.Duration
	now       func() time.Time
	mu        sync.Mutex // serialises runs inside this process
	lastTouch atomic.Int64
	inflight  atomic.Bool
}

// New builds a cleaner. A zero interval disables the background loop.
func New(db *store.DB, files *qrfile.Store, log *slog.Logger, interval time.Duration) *Cleaner {
	if log == nil {
		log = slog.Default()
	}
	return &Cleaner{
		db: db, files: files, log: log, interval: interval,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// WithClock replaces the time source, which lets tests move expiry forward
// without waiting. It returns the cleaner so it can be chained onto New.
func (c *Cleaner) WithClock(now func() time.Time) *Cleaner {
	if now != nil {
		c.now = now
	}
	return c
}

// RunOnce performs a single sweep. Concurrent callers, including other
// processes sharing the database, are serialised by an in-process mutex and a
// leased row in the database; the loser returns Result{Skipped: true}.
func (c *Cleaner) RunOnce(ctx context.Context) (Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	acquired, err := c.db.AcquireCleanupLease(ctx, now, leaseDuration)
	if err != nil {
		return Result{}, err
	}
	if !acquired {
		return Result{Skipped: true}, nil
	}

	var res Result
	defer func() {
		// Always drop the lease, even when the sweep failed part way.
		relCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := c.db.ReleaseCleanupLease(relCtx, c.now(), res.ShortURLs, res.QRCodes); err != nil {
			c.log.Error("cleanup: release lease", "error", err)
		}
	}()

	sweep, err := c.db.DeleteExpired(ctx, now)
	if err != nil {
		return res, err
	}
	res.ShortURLs = len(sweep.ShortURLs)
	res.QRCodes = len(sweep.QRCodes)
	res.Sessions = int(sweep.Sessions)
	res.StatDays = int(sweep.StatDays)

	// Files are unlinked after the transaction committed, so a rollback can
	// never leave a record pointing at a deleted image.
	if len(sweep.Files) > 0 && c.files != nil {
		if err := c.files.Remove(sweep.Files...); err != nil {
			c.log.Error("cleanup: remove qr files", "error", err)
		}
		res.Files = len(sweep.Files)
	}

	if c.files != nil {
		known, err := c.db.KnownQRFiles(ctx)
		if err != nil {
			return res, err
		}
		orphans, err := c.files.Sweep(known, orphanGrace, now)
		if err != nil {
			c.log.Error("cleanup: sweep orphan files", "error", err)
		}
		res.Orphans = orphans
	}

	if res.ShortURLs > 0 || res.QRCodes > 0 || res.Orphans > 0 {
		c.log.Info("cleanup completed",
			"shortURLs", res.ShortURLs, "qrCodes", res.QRCodes,
			"files", res.Files, "orphans", res.Orphans, "sessions", res.Sessions)
	}
	return res, nil
}

// Run drives the periodic sweep until the context is cancelled. It performs an
// immediate pass first so that a restart never serves expired data.
func (c *Cleaner) Run(ctx context.Context) {
	if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
		c.log.Error("cleanup: startup sweep", "error", err)
	}
	if c.interval <= 0 {
		return
	}
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := c.RunOnce(ctx); err != nil && ctx.Err() == nil {
				c.log.Error("cleanup: periodic sweep", "error", err)
			}
		}
	}
}

// Touch requests an opportunistic sweep in the background, at most once every
// touchInterval. It is called when the admin pages are viewed so that the
// listing an administrator sees never contains expired rows.
func (c *Cleaner) Touch(ctx context.Context) {
	now := c.now().Unix()
	last := c.lastTouch.Load()
	if now-last < int64(touchInterval/time.Second) {
		return
	}
	if !c.lastTouch.CompareAndSwap(last, now) {
		return
	}
	if !c.inflight.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.inflight.Store(false)
		bg, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		if _, err := c.RunOnce(bg); err != nil {
			c.log.Error("cleanup: opportunistic sweep", "error", err)
		}
	}()
}

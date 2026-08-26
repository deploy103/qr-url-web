// Command qrurl runs the QR Code generator and URL shortener service.
//
// Subcommands:
//
//	qrurl serve              start the HTTP server (default)
//	qrurl setpassword        create or replace the administrator credentials
//	qrurl backup             write a consistent copy of the database
//	qrurl version            print the build version
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/mvtp/qrurl/internal/auth"
	"github.com/mvtp/qrurl/internal/cleanup"
	"github.com/mvtp/qrurl/internal/config"
	"github.com/mvtp/qrurl/internal/qrfile"
	"github.com/mvtp/qrurl/internal/server"
	"github.com/mvtp/qrurl/internal/store"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "qrurl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		command, args = args[0], args[1:]
	}
	server.BuildVersion = version

	switch command {
	case "serve":
		return runServe(args)
	case "setpassword", "set-password":
		return runSetPassword(args)
	case "backup":
		return runBackup(args)
	case "version":
		fmt.Println("qrurl", version)
		return nil
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `qrurl - QR Code generator and URL shortener

Usage:
  qrurl [serve]           start the HTTP server (default)
  qrurl setpassword       create or replace the administrator credentials
  qrurl backup [-out F]   write a consistent database copy
  qrurl version           print the build version

Configuration comes from QRURL_* environment variables; see README.md.
`)
}

// newLogger builds the structured logger.
func newLogger(cfg *config.Config) *slog.Logger {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.LogLevel)); err != nil {
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	if strings.EqualFold(cfg.LogFormat, "text") {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With("service", "qrurl", "version", version)
}

func openStore(cfg *config.Config) (*store.DB, error) {
	db, err := store.Open(cfg.DBPath, cfg.Location)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.Migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	db.SetShortBaseURL(cfg.BaseURL)
	return db, nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := fs.String("addr", "", "override QRURL_ADDR")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if *addr != "" {
		cfg.Addr = *addr
	}
	log := newLogger(cfg)
	slog.SetDefault(log)

	// Only one process may own the data directory: the cleanup worker and the
	// rendered file tree are not designed to be shared.
	if cfg.SingleInstance {
		lock, err := store.AcquireInstanceLock(cfg.DataDir)
		if err != nil {
			if errors.Is(err, store.ErrAlreadyRunning) {
				return fmt.Errorf("%w\n  data directory: %s\n  "+
					"set QRURL_SINGLE_INSTANCE=false only if you know the other process is gone",
					err, cfg.DataDir)
			}
			return err
		}
		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("release instance lock", "error", err)
			}
		}()
		log.Debug("instance lock acquired", "path", lock.Path())
	}

	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	if err := ensureAdmin(context.Background(), db, log); err != nil {
		return err
	}

	files, err := qrfile.New(cfg.QRDir)
	if err != nil {
		return err
	}

	cleaner := cleanup.New(db, files, log, cfg.CleanupInterval)
	srv, err := server.New(server.Options{
		Config: cfg, DB: db, Files: files, Cleaner: cleaner, Logger: log,
	})
	if err != nil {
		return err
	}

	sqliteVersion, _ := db.SQLiteVersion(context.Background())
	log.Info("starting",
		"addr", cfg.Addr, "baseURL", cfg.BaseURL, "sqlite", sqliteVersion,
		"timezone", cfg.Location.String(), "cleanupInterval", cfg.CleanupInterval.String(),
		"publicQR", cfg.PublicQR, "publicQRAllow", cfg.PublicQRAllow.String(),
		"trustedProxies", cfg.TrustedProxies.String(), "redirectRate", cfg.RedirectRate,
		"secureCookie", cfg.SecureCookie, "hsts", cfg.HSTS)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The cleanup worker performs its first sweep immediately, which satisfies
	// the "check on application startup" requirement.
	go cleaner.Run(ctx)
	if cfg.BackupInterval > 0 {
		go runBackupLoop(ctx, db, cfg, log)
	}

	return server.ListenAndServe(ctx, srv, cfg, log)
}

// ensureAdmin provisions the administrator account on first start.
func ensureAdmin(ctx context.Context, db *store.DB, log *slog.Logger) error {
	exists, err := db.HasAdmin(ctx)
	if err != nil {
		return err
	}
	if exists {
		return nil
	}

	username := os.Getenv("QRURL_ADMIN_USERNAME")
	if username == "" {
		username = "admin"
	}
	password := os.Getenv("QRURL_ADMIN_PASSWORD")
	generated := false
	if password == "" {
		if password, err = auth.NewPassword(); err != nil {
			return err
		}
		generated = true
	}
	if err := auth.ValidatePassword(password); err != nil {
		return fmt.Errorf("QRURL_ADMIN_PASSWORD: %w", err)
	}
	hash, err := auth.HashPassword(password, auth.DefaultParams())
	if err != nil {
		return err
	}
	if err := db.SetAdmin(ctx, username, hash, time.Now().UTC()); err != nil {
		return err
	}

	log.Info("administrator account created", "username", username, "generatedPassword", generated)
	if generated {
		// Printed once to the console, never to the structured log.
		fmt.Fprintf(os.Stderr,
			"\n=========================================================\n"+
				" 관리자 계정이 생성되었습니다.\n"+
				"   아이디   : %s\n"+
				"   비밀번호 : %s\n"+
				" 이 값은 다시 표시되지 않습니다. 지금 저장한 뒤\n"+
				" `qrurl setpassword` 로 원하는 비밀번호로 변경하세요.\n"+
				"=========================================================\n\n",
			username, password)
	}
	return nil
}

func runSetPassword(args []string) error {
	fs := flag.NewFlagSet("setpassword", flag.ContinueOnError)
	username := fs.String("username", "", "administrator user name (default: keep current, or admin)")
	stdin := fs.Bool("stdin", false, "read the password from standard input instead of prompting")
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	name := *username
	if name == "" {
		if current, err := db.Admin(ctx); err == nil {
			name = current.Username
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		} else {
			name = "admin"
		}
	}

	var password string
	if *stdin {
		var buf []byte
		buf, err = readAllStdin()
		if err != nil {
			return err
		}
		password = strings.TrimRight(string(buf), "\r\n")
	} else {
		password, err = promptPassword()
		if err != nil {
			return err
		}
	}
	if err := auth.ValidatePassword(password); err != nil {
		return err
	}
	hash, err := auth.HashPassword(password, auth.DefaultParams())
	if err != nil {
		return err
	}
	if err := db.SetAdmin(ctx, name, hash, time.Now().UTC()); err != nil {
		return err
	}
	fmt.Printf("관리자 비밀번호를 변경했습니다. 아이디: %s (기존 세션은 모두 로그아웃되었습니다)\n", name)
	return nil
}

func readAllStdin() ([]byte, error) {
	info, err := os.Stdin.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeCharDevice != 0 {
		return nil, errors.New("standard input is a terminal; omit -stdin to be prompted")
	}
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 256)
	for {
		n, err := os.Stdin.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > auth.MaxPasswordLength+16 {
			break
		}
	}
	return buf, nil
}

func promptPassword() (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("no terminal available; use -stdin to pipe the password")
	}
	fmt.Fprintf(os.Stderr, "새 비밀번호 (%d자 이상): ", auth.MinPasswordLength)
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	fmt.Fprint(os.Stderr, "비밀번호 확인: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("두 비밀번호가 일치하지 않습니다")
	}
	return string(first), nil
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	out := fs.String("out", "", "output file (default: <backup dir>/qrurl-<timestamp>.db)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openStore(cfg)
	if err != nil {
		return err
	}
	defer db.Close()

	target := *out
	if target == "" {
		target = backupPath(cfg, time.Now())
	}
	if err := db.Backup(context.Background(), target); err != nil {
		return err
	}
	fmt.Println("백업을 저장했습니다:", target)
	return nil
}

func backupPath(cfg *config.Config, now time.Time) string {
	return filepath.Join(cfg.BackupDir, "qrurl-"+now.In(cfg.Location).Format("20060102-150405")+".db")
}

// runBackupLoop keeps a rolling set of database snapshots.
func runBackupLoop(ctx context.Context, db *store.DB, cfg *config.Config, log *slog.Logger) {
	ticker := time.NewTicker(cfg.BackupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			target := backupPath(cfg, now)
			if err := db.Backup(ctx, target); err != nil {
				log.Error("backup failed", "error", err)
				continue
			}
			log.Info("backup written", "path", target)
			if err := pruneBackups(cfg.BackupDir, cfg.BackupKeep); err != nil {
				log.Error("backup pruning failed", "error", err)
			}
		}
	}
}

func pruneBackups(dir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "qrurl-") && strings.HasSuffix(e.Name(), ".db") {
			names = append(names, e.Name())
		}
	}
	if len(names) <= keep {
		return nil
	}
	sort.Strings(names) // the timestamp prefix sorts chronologically
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

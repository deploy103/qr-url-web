package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ErrAlreadyRunning reports that another process already owns the data
// directory.
var ErrAlreadyRunning = errors.New("store: another instance is already using this data directory")

// InstanceLock is an advisory lock over a data directory.
//
// SQLite itself tolerates several processes, but this service also owns a
// directory of rendered files and runs a cleanup worker, and that combination
// has never been exercised across processes. Rather than let a second copy
// start and corrupt the operator's mental model, take a lock and fail loudly.
type InstanceLock struct {
	path string
	file *os.File
}

// AcquireInstanceLock takes an exclusive lock on <dir>/qrurl.lock. It returns
// ErrAlreadyRunning when a live process holds it, and takes over a lock left
// behind by a crash.
func AcquireInstanceLock(dir string) (*InstanceLock, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("store: create data directory: %w", err)
	}
	path := filepath.Join(dir, "qrurl.lock")

	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o640)
	if err != nil {
		return nil, fmt.Errorf("store: open lock file: %w", err)
	}

	// A BSD advisory lock is released automatically when the process dies, so
	// a crash never leaves a stale lock behind.
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder := readLockHolder(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			if holder != "" {
				return nil, fmt.Errorf("%w (%s)", ErrAlreadyRunning, holder)
			}
			return nil, ErrAlreadyRunning
		}
		return nil, fmt.Errorf("store: lock data directory: %w", err)
	}

	if err := f.Truncate(0); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: reset lock file: %w", err)
	}
	if _, err := f.WriteAt([]byte(fmt.Sprintf("pid=%d\nsince=%s\n",
		os.Getpid(), time.Now().UTC().Format(time.RFC3339))), 0); err != nil {
		f.Close()
		return nil, fmt.Errorf("store: write lock file: %w", err)
	}
	return &InstanceLock{path: path, file: f}, nil
}

// Release drops the lock and removes the marker file.
func (l *InstanceLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	closeErr := l.file.Close()
	l.file = nil
	// The file is left in place on purpose: removing it would race with a
	// process that has already opened it and is waiting for the lock.
	if err != nil {
		return fmt.Errorf("store: unlock data directory: %w", err)
	}
	return closeErr
}

// Path returns the location of the lock file.
func (l *InstanceLock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// readLockHolder reads the marker written by the process that holds the lock,
// so the error message can name it.
func readLockHolder(f *os.File) string {
	buf := make([]byte, 128)
	n, _ := f.ReadAt(buf, 0)
	if n <= 0 {
		return ""
	}
	var pid, since string
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "pid":
			if _, err := strconv.Atoi(value); err == nil {
				pid = value
			}
		case "since":
			since = value
		}
	}
	switch {
	case pid != "" && since != "":
		return "held by pid " + pid + " since " + since
	case pid != "":
		return "held by pid " + pid
	default:
		return ""
	}
}

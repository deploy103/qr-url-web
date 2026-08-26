// Package qrfile renders QR Codes to files on disk and keeps that directory in
// sync with the database.
package qrfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/deploy103/qr-url-web/internal/qr"
)

// Store owns a directory of rendered QR files.
type Store struct {
	dir string
}

// nameRe restricts stored file names to the shapes this package produces,
// which keeps a corrupted database row from escaping the directory.
var nameRe = regexp.MustCompile(`^[a-f0-9]{8,64}\.(png|svg)$`)

// New prepares the directory that holds the rendered files.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("qrfile: directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("qrfile: resolve directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0o750); err != nil {
		return nil, fmt.Errorf("qrfile: create directory: %w", err)
	}
	return &Store{dir: abs}, nil
}

// Dir returns the absolute directory of the store.
func (s *Store) Dir() string { return s.dir }

// Rendered is the result of writing one QR Code to disk.
type Rendered struct {
	PNGName string
	SVGName string
	Version int
	Size    int
}

// Render encodes payload and writes the PNG and SVG representations under the
// given identifier, replacing any previous files of that identifier.
func (s *Store) Render(id, payload string, level qr.Level, size int) (Rendered, error) {
	code, err := qr.Encode(payload, level)
	if err != nil {
		return Rendered{}, err
	}
	opts := qr.RenderOptions{Size: size}
	png, err := code.PNG(opts)
	if err != nil {
		return Rendered{}, err
	}
	svg := code.SVG(opts)

	out := Rendered{PNGName: id + ".png", SVGName: id + ".svg", Version: code.Version(), Size: size}
	if !nameRe.MatchString(out.PNGName) || !nameRe.MatchString(out.SVGName) {
		return Rendered{}, fmt.Errorf("qrfile: identifier %q is not a valid file name", id)
	}
	if err := s.writeFile(out.PNGName, png); err != nil {
		return Rendered{}, err
	}
	if err := s.writeFile(out.SVGName, svg); err != nil {
		_ = s.Remove(out.PNGName)
		return Rendered{}, err
	}
	return out, nil
}

// writeFile writes atomically so a crash never leaves a half rendered image.
func (s *Store) writeFile(name string, data []byte) error {
	final := filepath.Join(s.dir, name)
	tmp, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("qrfile: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("qrfile: write %s: %w", name, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("qrfile: sync %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("qrfile: close %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("qrfile: chmod %s: %w", name, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("qrfile: install %s: %w", name, err)
	}
	return nil
}

// Path resolves a stored name to an absolute path, rejecting anything that
// does not look like a file this package wrote.
func (s *Store) Path(name string) (string, error) {
	if !nameRe.MatchString(name) {
		return "", fmt.Errorf("qrfile: %q is not a stored file name", name)
	}
	return filepath.Join(s.dir, name), nil
}

// Read returns the contents of a stored file.
func (s *Store) Read(name string) ([]byte, error) {
	p, err := s.Path(name)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// Remove deletes the named files, ignoring the ones that are already gone.
func (s *Store) Remove(names ...string) error {
	var firstErr error
	for _, name := range names {
		if name == "" {
			continue
		}
		p, err := s.Path(name)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Sweep deletes files that no database record references any more. A grace
// period protects files that are being written by a concurrent request.
func (s *Store) Sweep(known map[string]struct{}, grace time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return 0, fmt.Errorf("qrfile: list directory: %w", err)
	}
	removed := 0
	var firstErr error
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if _, ok := known[name]; ok {
			continue
		}
		// Leave anything that is not ours alone, except our own temporary files.
		if !nameRe.MatchString(name) && !strings.HasPrefix(name, ".tmp-") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) < grace {
			continue
		}
		if err := os.Remove(filepath.Join(s.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		removed++
	}
	return removed, firstErr
}

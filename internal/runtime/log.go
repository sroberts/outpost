package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

const (
	defaultMaxLogBytes = 10 << 20 // 10 MiB
	defaultLogBackups  = 3
)

// rotatingWriter writes a service's combined stdout/stderr to
// <dir>/<service>.log, rolling the file over once it exceeds MaxBytes and
// keeping Backups previous generations as <service>.log.1 … .log.N.
//
// It is deliberately small: services are expected to be quiet, and pulling in a
// log-rotation dependency for a v1 daemon that only supervises a handful of
// processes is not a trade worth making.
type rotatingWriter struct {
	path     string
	maxBytes int64
	backups  int

	mu   sync.Mutex
	f    *os.File
	size int64
}

func newRotatingWriter(path string, maxBytes int64, backups int) (*rotatingWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create log dir: %w", err)
	}
	w := &rotatingWriter{path: path, maxBytes: maxBytes, backups: backups}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *rotatingWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open log %s: %w", w.path, err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("stat log %s: %w", w.path, err)
	}
	w.f, w.size = f, fi.Size()
	return nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotate(); err != nil {
			// Rotation failure must not lose the service's output.
			fmt.Fprintf(os.Stderr, "outpost: log rotation for %s failed: %v\n", w.path, err)
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotate shifts <path>.i to <path>.i+1, moves the live log to <path>.1 and
// reopens a fresh one. Callers hold mu.
func (w *rotatingWriter) rotate() error {
	if err := w.f.Close(); err != nil {
		return err
	}
	if w.backups <= 0 {
		// No history kept: truncate in place.
		os.Remove(w.path)
		return w.open()
	}

	backup := func(i int) string { return fmt.Sprintf("%s.%d", w.path, i) }
	os.Remove(backup(w.backups))
	for i := w.backups - 1; i >= 1; i-- {
		if err := os.Rename(backup(i), backup(i+1)); err != nil && !os.IsNotExist(err) {
			w.open()
			return err
		}
	}
	if err := os.Rename(w.path, backup(1)); err != nil && !os.IsNotExist(err) {
		// Reopen regardless so writes keep landing somewhere.
		w.open()
		return err
	}
	return w.open()
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

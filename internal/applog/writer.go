package applog

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// LineWriter is one of a service's streams: it cuts what the service writes
// into lines and stores each through Format on w, which both streams share.
// A line is stored once its newline arrives, or at MaxLineBytes, or on Flush
// when the service exits mid-line.
type LineWriter struct {
	w      io.Writer
	stream string
	now    func() time.Time

	mu  sync.Mutex
	buf []byte
}

// NewLineWriter stores stream's lines on w. A Write to w must be atomic for
// the two streams' lines not to interleave mid-line; the runtime's rotating
// writer holds a lock across each.
func NewLineWriter(w io.Writer, stream string) *LineWriter {
	return &LineWriter{w: w, stream: stream, now: time.Now}
}

func (l *LineWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf = append(l.buf, p...)
	for {
		i := bytes.IndexByte(l.buf, '\n')
		if i < 0 {
			if len(l.buf) < MaxLineBytes {
				break
			}
			i = MaxLineBytes
			if err := l.emit(l.buf[:i]); err != nil {
				return len(p), err
			}
			l.buf = l.buf[i:]
			continue
		}
		if err := l.emit(l.buf[:i]); err != nil {
			return len(p), err
		}
		l.buf = l.buf[i+1:]
	}
	// Don't keep a large backing array alive for a short remainder.
	if cap(l.buf) > 4*MaxLineBytes && len(l.buf) < MaxLineBytes {
		l.buf = append([]byte(nil), l.buf...)
	}
	return len(p), nil
}

// Flush stores a last line that never got its newline.
func (l *LineWriter) Flush() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) == 0 {
		return nil
	}
	err := l.emit(l.buf)
	l.buf = nil
	return err
}

func (l *LineWriter) emit(line []byte) error {
	line = bytes.TrimSuffix(line, []byte("\r"))
	_, err := l.w.Write(Format(l.now(), l.stream, string(line)))
	return err
}

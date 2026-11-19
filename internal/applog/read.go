package applog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Backups is how many rotated files the runtime keeps beside the live one;
// a read reaches back through them.
const Backups = 3

// DefaultLines and MaxLines bound a read.
const (
	DefaultLines = 100
	MaxLines     = 10000
)

// Query is which lines to read: the newest Lines of those that match.
type Query struct {
	Lines  int       // how many; 0 is DefaultLines, and more than MaxLines is MaxLines
	Since  time.Time // only lines written after this; a line with no time is older than any
	Level  string    // only lines at this level or above (see AtLeast)
	Stream string    // only lines from this stream, "out" or "err"
	// Contains keeps lines whose text contains it, ignoring case.
	Contains string
}

func (q Query) limit() int {
	switch {
	case q.Lines <= 0:
		return DefaultLines
	case q.Lines > MaxLines:
		return MaxLines
	}
	return q.Lines
}

func (q Query) matches(r Record) bool {
	if q.Stream != "" && r.Stream != q.Stream {
		return false
	}
	if q.Level != "" && !AtLeast(r.Level, q.Level) {
		return false
	}
	if q.Contains != "" && !strings.Contains(strings.ToLower(r.Line), strings.ToLower(q.Contains)) {
		return false
	}
	return true
}

// ErrNoLog is a service with no log file yet.
var ErrNoLog = errors.New("no log yet")

// Path is where a service's live log is. It refuses a name that could reach
// outside dir: names come from clients, over the socket and the web.
func Path(dir, service string) (string, error) {
	if service == "" || service == "." || service == ".." || strings.ContainsAny(service, "/\\\x00") {
		return "", fmt.Errorf("no such service %q", service)
	}
	return filepath.Join(dir, service+".log"), nil
}

// Read returns a service's newest lines matching q, oldest first.
func Read(dir, service string, q Query) ([]Record, error) {
	path, err := Path(dir, service)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoLog
		}
		return nil, err
	}
	want := q.limit()
	var newestFirst []Record
	// The live file, then each rotated one, newest to oldest.
	files := []string{path}
	for i := 1; i <= Backups; i++ {
		files = append(files, fmt.Sprintf("%s.%d", path, i))
	}
	done := false
	for _, p := range files {
		err := eachLineBackward(p, func(line string) bool {
			r := Parse(line)
			if !q.Since.IsZero() && !r.Time.After(q.Since) {
				done = true
				return false
			}
			if q.matches(r) {
				r.Service = service
				newestFirst = append(newestFirst, r)
				if len(newestFirst) >= want {
					done = true
					return false
				}
			}
			return true
		})
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if done {
			break
		}
	}
	out := make([]Record, len(newestFirst))
	for i, r := range newestFirst {
		out[len(out)-1-i] = r
	}
	return out, nil
}

// ReadAll interleaves several services' newest lines by when they were
// written, and keeps the newest q.Lines of them all. A service with no log
// yet is left out.
func ReadAll(dir string, services []string, q Query) ([]Record, error) {
	var all []Record
	for _, s := range services {
		recs, err := Read(dir, s, q)
		if errors.Is(err, ErrNoLog) {
			continue
		}
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
	}
	// Stable, so one service's lines with the same time keep their order;
	// lines with no time sort first, as the oldest.
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	if n := q.limit(); len(all) > n {
		all = all[len(all)-n:]
	}
	return all, nil
}

const chunk = 64 << 10

// eachLineBackward calls fn with each line of the file at path, last first,
// until fn returns false. It reads from the end in chunks, so the newest
// lines of a large file cost no more than those of a small one.
func eachLineBackward(path string, fn func(string) bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	pos := info.Size()
	var carry []byte // the start of a line whose beginning is in an earlier chunk
	buf := make([]byte, chunk)
	for pos > 0 {
		n := int64(chunk)
		if pos < n {
			n = pos
		}
		pos -= n
		if _, err := f.ReadAt(buf[:n], pos); err != nil && err != io.EOF {
			return err
		}
		block := append(append([]byte{}, buf[:n]...), carry...)
		// Every complete line in block, from the end; whatever comes before
		// the first newline may continue into the chunk before.
		end := len(block)
		for i := len(block) - 1; i >= 0; i-- {
			if block[i] != '\n' {
				continue
			}
			if line := block[i+1 : end]; len(line) > 0 && !fn(string(line)) {
				return nil
			}
			end = i
		}
		carry = block[:end]
		// A single line longer than the whole file so far keeps growing
		// carry; cap it, since nothing reads a line that long.
		if len(carry) > MaxLineBytes {
			carry = carry[len(carry)-MaxLineBytes:]
		}
	}
	if len(carry) > 0 {
		fn(string(carry))
	}
	return nil
}

// MaxLineBytes is the longest line the runtime stores whole; a longer one is
// split.
const MaxLineBytes = 64 << 10

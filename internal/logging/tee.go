package logging

import (
	"bytes"
	"io"
	"regexp"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/log"
)

// NewTee is New writing every line to both term, the stdout or stderr that a
// person or launchd or systemd reads, and file, the log the daemon keeps
// itself. The colour term would get on a terminal is kept for term and left
// out of file. A nil file is New on term alone.
func NewTee(term, file io.Writer, level, format string) *log.Logger {
	if file == nil {
		return New(term, level, format)
	}
	l := New(tee{term: term, file: file}, level, format)
	// The logger decides on colour from the writer it's given, and a tee
	// isn't a terminal, so it is told what term would have said.
	l.SetColorProfile(lipgloss.NewRenderer(term).ColorProfile())
	return l
}

type tee struct{ term, file io.Writer }

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// Write gives the line to both writers whatever happens to the other, so a
// closed stdout doesn't cost the file its lines, or the reverse.
func (t tee) Write(p []byte) (int, error) {
	_, termErr := t.term.Write(p)
	q := p
	if bytes.IndexByte(q, 0x1b) >= 0 {
		q = ansi.ReplaceAll(q, nil)
	}
	_, fileErr := t.file.Write(q)
	if termErr != nil {
		return len(p), termErr
	}
	return len(p), fileErr
}

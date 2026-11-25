package logging

import (
	"fmt"
	"io"
	"strings"

	"github.com/charmbracelet/log"
)

// Line is one package's way to log a line at a level, with fields. It goes
// to a *log.Logger when the package was handed one, which is how the daemon
// runs. A package handed only a plain func(format, v...) instead, as its
// older callers and its tests do, gets every line there with its format and
// arguments untouched and its level and fields dropped, exactly as before
// packages had levels.
type Line struct {
	l    *log.Logger
	logf func(format string, v ...any)
	kv   []any
}

// To is a Line carrying kv as its fields. logf, when set, wins over l: a
// package keeps a plain func only when it wasn't given a logger, or when a
// test has swapped its own in to read what was said.
func To(l *log.Logger, logf func(format string, v ...any), kv ...any) Line {
	return Line{l: l, logf: logf, kv: kv}
}

// With adds fields.
func (p Line) With(kv ...any) Line {
	p.kv = append(append([]any(nil), p.kv...), kv...)
	return p
}

func (p Line) Debugf(format string, v ...any) { p.at(log.DebugLevel, format, v...) }
func (p Line) Infof(format string, v ...any)  { p.at(log.InfoLevel, format, v...) }
func (p Line) Warnf(format string, v ...any)  { p.at(log.WarnLevel, format, v...) }
func (p Line) Errorf(format string, v ...any) { p.at(log.ErrorLevel, format, v...) }

func (p Line) at(level log.Level, format string, v ...any) {
	if p.logf != nil {
		p.logf(format, v...)
		return
	}
	// Checked first so a debug line costs nothing to format when it won't
	// be written, since some are on every request.
	if p.l == nil || p.l.GetLevel() > level {
		return
	}
	// The "outpost: " or "gateway: " a line may begin with predates the
	// logger's prefix, which now says where a line came from.
	msg := strings.TrimPrefix(fmt.Sprintf(format, v...), "outpost: ")
	if prefix := p.l.GetPrefix(); prefix != "" {
		msg = strings.TrimPrefix(msg, prefix+": ")
	}
	p.l.Log(level, msg, p.kv...)
}

// Writer is l as an io.Writer that logs each line written to it at level,
// for code that prints its lines rather than logging them.
func Writer(l *log.Logger, level log.Level) io.Writer { return lineWriter{l, level} }

type lineWriter struct {
	l     *log.Logger
	level log.Level
}

func (w lineWriter) Write(p []byte) (int, error) {
	for _, line := range strings.Split(string(p), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			To(w.l, nil).at(w.level, "%s", line)
		}
	}
	return len(p), nil
}

// Component is root's logger for one part of the daemon: its lines carry
// name as their prefix, and it logs at level ("debug", "info", "warn" or
// "error"), so one part can be turned up without the rest.
func Component(root *log.Logger, name, level string) *log.Logger {
	l := root.WithPrefix(name)
	l.SetLevel(ParseLevel(level))
	return l
}

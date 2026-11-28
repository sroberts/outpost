// Package logging is the daemon's own log: leveled, timestamped lines through
// charmbracelet/log, in text for a person (coloured on a terminal) or json or
// logfmt for a collector.
//
// serve makes one logger and hands each part of the daemon its own Component
// of it, which the part logs through at real levels with fields, by way of a
// Line. A part also still takes a plain func(format string, v ...any), which
// its tests and older callers use; Logf adapts a logger to that for whatever
// hasn't got levels of its own, choosing each line's level from its wording.
package logging

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/charmbracelet/log"
)

// New is a logger writing to w at level ("debug", "info", "warn" or "error")
// in format ("text", "json" or "logfmt"). Unknown values fall back to info and
// text; config validation is where they are refused.
func New(w io.Writer, level, format string) *log.Logger {
	opts := log.Options{
		Level:           ParseLevel(level),
		ReportTimestamp: true,
		TimeFormat:      "2006-01-02 15:04:05",
		Formatter:       log.TextFormatter,
	}
	switch strings.ToLower(format) {
	case "json":
		opts.Formatter, opts.TimeFormat = log.JSONFormatter, time.RFC3339
	case "logfmt":
		opts.Formatter, opts.TimeFormat = log.LogfmtFormatter, time.RFC3339
	}
	return log.NewWithOptions(w, opts)
}

// ParseLevel reads a level name; anything else is info.
func ParseLevel(s string) log.Level {
	l, err := log.ParseLevel(strings.ToLower(strings.TrimSpace(s)))
	if err != nil {
		return log.InfoLevel
	}
	return l
}

// Logf adapts l to the func(format, v...) the daemon's packages log through.
// Each line's level is read from its wording (see Classify), and the
// "outpost: " every line used to start with is dropped, since the logger's
// prefix now says where a line came from.
func Logf(l *log.Logger) func(format string, v ...any) {
	return func(format string, v ...any) {
		msg := strings.TrimPrefix(fmt.Sprintf(format, v...), "outpost: ")
		switch Classify(msg) {
		case log.ErrorLevel:
			l.Error(msg)
		case log.WarnLevel:
			l.Warn(msg)
		default:
			l.Info(msg)
		}
	}
}

// Classify guesses a line's level from its wording. The daemon's lines were
// written for a person, without levels, and saying something failed is how
// they mark trouble: an error is something that didn't happen, a warning is
// something that went ahead in a lesser way, and the rest is information.
func Classify(msg string) log.Level {
	m := strings.ToLower(msg)
	for _, w := range []string{"panic", "error", "failed", "fails", " fail ", "crash"} {
		if strings.Contains(m, w) {
			return log.ErrorLevel
		}
	}
	for _, w := range []string{"could not", "couldn't", "cannot", "can't", "unable", "refus", "retry", "retrying",
		"warning", "skipp", "giving up", "gave up", "timed out", "timeout", "unhealthy", "stale", "not reloaded"} {
		if strings.Contains(m, w) {
			return log.WarnLevel
		}
	}
	return log.InfoLevel
}

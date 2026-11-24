// Package applog is what Outpost knows about a service's own log: the lines
// it writes to stdout and stderr, captured into <logs>/<service>.log.
//
// Each line is stored as it was written, after a UTC timestamp and the stream
// it came on:
//
//	2026-11-18T16:16:38.123456Z out listening on :8080
//	2026-11-18T16:16:39.000001Z err {"level":"error","msg":"db locked"}
//
// so a file still reads with tail or grep, and Outpost can say when each line
// was written and on which stream without the service doing anything. Reading
// a line back, Parse also picks out a level and message where the service
// wrote one it can recognise: a JSON object, a logfmt line, or a line that
// starts with a level word. Lines from before the prefix was added read as
// they were, with no time or stream.
package applog

import (
	"bytes"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The levels a line can be read as, least severe first. A line with no level
// Outpost can recognise is LevelNone, which every filter but "error" and
// "warn" lets through.
const (
	LevelNone  = ""
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Levels is every level a filter can name, least severe first.
var Levels = []string{LevelDebug, LevelInfo, LevelWarn, LevelError}

// Streams.
const (
	Stdout = "out"
	Stderr = "err"
)

// Record is one line of a service's log.
type Record struct {
	Service string    `json:"service,omitempty"`
	Time    time.Time `json:"time,omitzero"`
	Stream  string    `json:"stream,omitempty"` // "out", "err", or "" for a line from before streams were kept
	Level   string    `json:"level,omitempty"`  // one of Levels, or "" when the line doesn't say
	Message string    `json:"message"`          // the line's message: a JSON or logfmt line's msg, else the line
	// Fields are a structured line's other keys, for a JSON or logfmt line.
	Fields map[string]string `json:"fields,omitempty"`
	// Line is the line as the service wrote it.
	Line string `json:"line"`

	// Source is where the line came from: one of Sources.
	Source string `json:"source,omitempty"`
	// Component is the part of the daemon a daemon line came from, such as
	// "gateway" or "tsnet"; "" for the daemon's own lines and every other
	// source.
	Component string `json:"component,omitempty"`
}

const timeLayout = "2006-01-02T15:04:05.000000Z"

// Format is the stored form of a line written at t on stream: what the
// runtime appends to the log file, newline included.
func Format(t time.Time, stream, line string) []byte {
	b := make([]byte, 0, len(timeLayout)+len(stream)+len(line)+3)
	b = t.UTC().AppendFormat(b, timeLayout)
	b = append(b, ' ')
	b = append(b, stream...)
	b = append(b, ' ')
	b = append(b, line...)
	return append(b, '\n')
}

var stored = regexp.MustCompile(`^(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{6}Z) (out|err) `)

// Parse reads one stored line back.
func Parse(stored string) Record {
	var r Record
	line := strings.TrimRight(stored, "\r\n")
	if m := storedPrefix(line); m != nil {
		if t, err := time.Parse(timeLayout, line[m[2]:m[3]]); err == nil {
			r.Time, r.Stream = t, line[m[4]:m[5]]
			line = line[m[1]:]
		}
	}
	r.Line = line
	r.Level, r.Message, r.Fields = interpret(line)
	return r
}

func storedPrefix(line string) []int {
	// The cheap test first: almost every line either has the prefix or is
	// too short to.
	if len(line) < len(timeLayout)+5 || line[4] != '-' || line[10] != 'T' {
		return nil
	}
	return stored.FindStringSubmatchIndex(line)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)`)

// interpret picks the level, message and fields out of a line a service
// wrote, where it can.
func interpret(line string) (level, msg string, fields map[string]string) {
	plain := ansi.ReplaceAllString(line, "")
	trimmed := strings.TrimSpace(plain)
	if strings.HasPrefix(trimmed, "{") {
		if level, msg, fields, ok := fromJSON(trimmed); ok {
			return level, msg, fields
		}
	}
	if level, msg, fields, ok := fromLogfmt(trimmed); ok {
		return level, msg, fields
	}
	return leadingLevel(trimmed), plain, nil
}

func fromJSON(s string) (level, msg string, fields map[string]string, ok bool) {
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &obj) != nil {
		return "", "", nil, false
	}
	fields = map[string]string{}
	for k, raw := range obj {
		var str string
		if json.Unmarshal(raw, &str) != nil {
			str = string(bytes.TrimSpace(raw))
		}
		fields[k] = str
	}
	level = take(fields, levelKeys...)
	msg = take(fields, msgKeys...)
	if level == "" && msg == "" {
		return "", s, fields, true
	}
	if msg == "" {
		msg = s
	}
	return NormalizeLevel(level), msg, fields, true
}

var (
	levelKeys = []string{"level", "lvl", "severity", "loglevel", "log.level"}
	msgKeys   = []string{"msg", "message", "event"}
	// A logfmt line needs a level key to count as one: plenty of prose
	// contains a word=word.
	logfmtLevel = regexp.MustCompile(`(?:^|\s)(?:level|lvl|severity)=`)
	logfmtPair  = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.\-]*)=("(?:[^"\\]|\\.)*"|\S*)`)
)

func fromLogfmt(s string) (level, msg string, fields map[string]string, ok bool) {
	if !logfmtLevel.MatchString(s) {
		return "", "", nil, false
	}
	fields = map[string]string{}
	for _, m := range logfmtPair.FindAllStringSubmatch(s, -1) {
		v := m[2]
		if len(v) >= 2 && v[0] == '"' {
			var unq string
			if json.Unmarshal([]byte(v), &unq) == nil {
				v = unq
			} else {
				v = v[1 : len(v)-1]
			}
		}
		fields[m[1]] = v
	}
	level = take(fields, levelKeys...)
	msg = take(fields, msgKeys...)
	if msg == "" {
		msg = s
	}
	return NormalizeLevel(level), msg, fields, true
}

// take removes and returns the first of keys present in fields.
func take(fields map[string]string, keys ...string) string {
	for _, k := range keys {
		if v, ok := fields[k]; ok {
			delete(fields, k)
			return v
		}
	}
	return ""
}

// A level word at the start of a line, after an optional timestamp, in
// capitals or brackets: "ERROR db locked", "2026/11/18 12:00:00 [warn] slow",
// "time=… W1118 …" is not attempted. Lower-case prose ("error handling
// improved") isn't a level.
var leading = regexp.MustCompile(`^(?:[\d/:.\-T Z+]+\s+)?(?:\[(?i:(trace|debug|info|warn|warning|error|err|fatal|panic|crit|critical))\]|(TRACE|DEBUG|INFO|WARN|WARNING|ERROR|ERR|FATAL|PANIC|CRIT|CRITICAL)\b)`)

// TrimLevel drops what leadingLevel read from the start of a plain line,
// the level word and any timestamp before it, for a view that already shows
// both in their own columns: "ERROR db locked" reads "db locked" beside an
// error tag rather than repeating the word. A line with nothing else after
// the level is left as it is.
func TrimLevel(s string) string {
	m := leading.FindStringIndex(s)
	if m == nil {
		return s
	}
	rest := strings.TrimLeft(s[m[1]:], " \t:-|")
	if rest == "" {
		return s
	}
	return rest
}

func leadingLevel(s string) string {
	m := leading.FindStringSubmatch(s)
	if m == nil {
		return LevelNone
	}
	return NormalizeLevel(m[1] + m[2])
}

// NormalizeLevel maps the many ways of naming a level onto Levels, and
// anything unrecognised onto LevelNone.
func NormalizeLevel(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "trace", "debug", "dbg", "verbose":
		return LevelDebug
	case "info", "inf", "information", "notice":
		return LevelInfo
	case "warn", "warning", "wrn":
		return LevelWarn
	case "error", "err", "fatal", "panic", "crit", "critical", "alert", "emerg", "emergency":
		return LevelError
	}
	return LevelNone
}

// rank orders levels for filtering; LevelNone ranks with info, as most
// unlabelled output is.
func rank(level string) int {
	switch level {
	case LevelDebug:
		return 0
	case LevelWarn:
		return 2
	case LevelError:
		return 3
	}
	return 1
}

// AtLeast reports whether a line at level passes a filter for min ("" lets
// everything through).
func AtLeast(level, min string) bool {
	if min == "" {
		return true
	}
	return rank(level) >= rank(NormalizeLevel(min))
}

// LevelTags are the four-letter tags Text marks a line's level with, as
// charmbracelet/log does for the daemon's own lines.
var LevelTags = map[string]string{LevelDebug: "DEBU", LevelInfo: "INFO", LevelWarn: "WARN", LevelError: "ERRO"}

// Text is a record as one line of plain text: its local time, its level (or
// "err" for an unlabelled stderr line), its message and any fields. A line
// from before streams were kept is shown as it was written.
func Text(r Record) string {
	if r.Time.IsZero() && r.Stream == "" && r.Fields == nil {
		return r.Line
	}
	var b strings.Builder
	if !r.Time.IsZero() {
		b.WriteString(r.Time.Local().Format("15:04:05") + " ")
	}
	switch tag, ok := LevelTags[r.Level]; {
	case ok:
		b.WriteString(tag + " ")
	case r.Stream == Stderr:
		b.WriteString("err  ")
	default:
		b.WriteString("     ")
	}
	b.WriteString(r.Message)
	keys := make([]string, 0, len(r.Fields))
	for k := range r.Fields {
		if k != "time" && k != "ts" && k != "timestamp" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := r.Fields[k]
		if strings.ContainsAny(v, " \t\"") {
			q, _ := json.Marshal(v)
			v = string(q)
		}
		b.WriteString(" " + k + "=" + v)
	}
	return b.String()
}

// TextLevel reads back the level Text marked a line with, for a reader that
// only has the text; "" when it carries none.
func TextLevel(line string) string {
	// "15:04:05 TAG " is 14 bytes; anything shorter has no tag.
	if len(line) < 13 || line[2] != ':' || line[5] != ':' || line[8] != ' ' {
		return LevelNone
	}
	tag := line[9:13]
	for level, t := range LevelTags {
		if t == tag {
			return level
		}
	}
	if tag == "err " {
		return LevelError
	}
	return LevelNone
}

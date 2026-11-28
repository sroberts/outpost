package applog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The sources of lines Outpost keeps, beside apps' own output, and which
// every log view reads the same way. Tailnet is the daemon's own lines from
// its tailnet nodes, which it relays into its log; All is every source at
// once, interleaved by time, which is how one request is followed from the
// gateway to the app by its request id.
const (
	SourceApp     = "app"
	SourceDaemon  = "daemon"
	SourceTailnet = "tailnet"
	SourceAccess  = "access"
	SourceEvents  = "events"
	SourceAll     = "all"
)

// Sources is every source a view can be asked for, in the order a picker
// lists them.
var Sources = []string{SourceApp, SourceDaemon, SourceTailnet, SourceAccess, SourceEvents, SourceAll}

// ValidSource reports whether s names a source; "" is SourceApp.
func ValidSource(s string) bool { return s == "" || contains(Sources, s) }

// The files Outpost keeps its own lines in, beside the apps' logs. A service
// may take neither name (see manifest).
const (
	DaemonLogFile = "outpost.log"
	AccessLogFile = "outpost-access.log"
)

// tailnetComponent is the daemon part a tailnet node's lines are relayed
// under.
const tailnetComponent = "tsnet"

// daemonComponents are the names the daemon's parts log under, as
// config.LogComponents lists them. In the text format a component is only
// told from a message that happens to begin "word: " by being one of these.
var daemonComponents = []string{"supervisor", "gateway", "ipc", "ai", "mail", "tsnet", "stats"}

// daemonText is a line of the daemon's text format: its local time to the
// second, a four-letter level, then the component and message.
var daemonText = regexp.MustCompile(`^(\d{4}-\d\d-\d\d \d\d:\d\d:\d\d) (DEBU|INFO|WARN|ERRO|FATA)\s+(.*)$`)

// trailingPairs is where a text line's fields begin: the earliest point
// from which the rest is nothing but key=value pairs.
var trailingPairs = regexp.MustCompile(`(?:\s+[A-Za-z_][A-Za-z0-9_.\-]*=(?:"(?:[^"\\]|\\.)*"|\S*))+$`)

var textLevels = map[string]string{"DEBU": LevelDebug, "INFO": LevelInfo, "WARN": LevelWarn, "ERRO": LevelError, "FATA": LevelError}

// ParseDaemon reads one entry of the daemon's own log, in whichever of its
// formats it was written: text, json or logfmt. A text entry whose value
// had newlines in it spans lines, and comes here whole.
func ParseDaemon(line string) Record {
	r := Record{Source: SourceDaemon, Line: line}
	first, more, _ := strings.Cut(line, "\n")
	var fields map[string]string
	switch {
	case strings.HasPrefix(first, "{"):
		var ok bool
		if r.Level, r.Message, fields, ok = fromJSON(first); !ok {
			r.Message = line
			return r
		}
	case strings.HasPrefix(first, "time="):
		var ok bool
		if r.Level, r.Message, fields, ok = fromLogfmt(first); !ok {
			r.Message = line
			return r
		}
	default:
		m := daemonText.FindStringSubmatch(first)
		if m == nil {
			r.Message = line
			return r
		}
		if t, err := time.ParseInLocation("2006-01-02 15:04:05", m[1], time.Local); err == nil {
			r.Time = t
		}
		r.Level = textLevels[m[2]]
		rest := m[3]
		if c, after, ok := strings.Cut(rest, ": "); ok && contains(daemonComponents, c) {
			r.Component, rest = c, after
		}
		fields = map[string]string{}
		if loc := trailingPairs.FindStringIndex(rest); loc != nil {
			pairs(rest[loc[0]:], fields)
			rest = rest[:loc[0]]
		}
		r.Message = rest
		continuation(more, fields)
		r.Service = take(fields, "service")
		if len(fields) > 0 {
			r.Fields = fields
		}
		return r
	}
	if ts := take(fields, "time"); ts != "" {
		if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
			r.Time = t
		}
	}
	r.Component = take(fields, "prefix")
	r.Service = take(fields, "service")
	if len(fields) > 0 {
		r.Fields = fields
	}
	return r
}

// pairs adds the key=value pairs in s to fields.
func pairs(s string, fields map[string]string) (last string) {
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
		last = m[1]
	}
	return last
}

// continuation reads the lines a text entry carried on past its first: the
// text format puts a value with newlines in it on lines of its own, each
// after a "│", below its key.
func continuation(more string, fields map[string]string) {
	if more == "" {
		return
	}
	key := ""
	for _, l := range strings.Split(more, "\n") {
		t := strings.TrimSpace(l)
		if v, ok := strings.CutPrefix(t, "│"); ok {
			if key != "" {
				if fields[key] != "" {
					fields[key] += "\n"
				}
				fields[key] += strings.TrimPrefix(v, " ")
			}
			continue
		}
		if k := pairs(t, fields); k != "" {
			key = k
		}
	}
}

// ReadDaemon returns the daemon's own newest lines matching q, oldest first.
// q.Component "tsnet" reads the tailnet nodes' lines.
func ReadDaemon(dir string, q Query) ([]Record, error) {
	return readOwn(filepath.Join(dir, DaemonLogFile), ParseDaemon, q, true)
}

// ReadAccess returns the gateway's newest access log entries matching q,
// oldest first.
func ReadAccess(dir string, q Query) ([]Record, error) {
	return readOwn(filepath.Join(dir, AccessLogFile), ParseAccess, q, false)
}

// readOwn reads one of Outpost's own logs, which reads as empty until the
// daemon has written it.
func readOwn(path string, parse func(string) Record, q Query, indented bool) ([]Record, error) {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return []Record{}, nil
		}
		return nil, err
	}
	return readFile(path, parse, q, indented)
}

// AccessEntry is one request in the gateway's access log, a JSON object per
// line. It is built from the request line and what the gateway decided, and
// never from a header or a body, so no cookie, token or password can reach
// it; the path is kept without its query, which can carry a sign-in code.
type AccessEntry struct {
	Time      time.Time `json:"time"`
	RequestID string    `json:"request_id"`
	Method    string    `json:"method"`
	Host      string    `json:"host"`
	Path      string    `json:"path"`
	// App is the service the request was for; "" for the portal's own pages.
	App    string `json:"app,omitempty"`
	Status int    `json:"status"`
	// Bytes is the size of the response body as the gateway sent it.
	Bytes int64 `json:"bytes"`
	// Millis is how long the gateway took to answer, to the microsecond.
	Millis float64 `json:"ms"`
	IP     string  `json:"ip"`
	// User is who the request went through as, or the session it carried.
	User string `json:"user,omitempty"`
	// Token is the id of the access token the request presented, as `outpost
	// token list` shows it; never the token.
	Token string `json:"token,omitempty"`
	// Refused is why the gateway turned the request away, when it did.
	Refused string `json:"refused,omitempty"`
	// Public marks a request from the internet, through Funnel.
	Public bool `json:"public,omitempty"`
}

// ParseAccess reads one line of the access log.
func ParseAccess(line string) Record {
	r := Record{Source: SourceAccess, Line: line, Message: line}
	var e AccessEntry
	if json.Unmarshal([]byte(line), &e) != nil {
		return r
	}
	r.Time, r.Service = e.Time, e.App
	r.Level = StatusLevel(e.Status)
	r.Message = fmt.Sprintf("%s %s%s %d", e.Method, e.Host, e.Path, e.Status)
	f := map[string]string{
		"request_id": e.RequestID, "method": e.Method, "host": e.Host, "path": e.Path,
		"status": strconv.Itoa(e.Status), "bytes": strconv.FormatInt(e.Bytes, 10),
		"ms": strconv.FormatFloat(e.Millis, 'f', -1, 64), "ip": e.IP,
	}
	for k, v := range map[string]string{"user": e.User, "token": e.Token, "refused": e.Refused} {
		if v != "" {
			f[k] = v
		}
	}
	if e.Public {
		f["public"] = "true"
	}
	r.Fields = f
	return r
}

// StatusLevel is the level a request reads at: an error for a server error,
// a warning for a refusal, otherwise information.
func StatusLevel(status int) string {
	switch {
	case status >= 500:
		return LevelError
	case status >= 400:
		return LevelWarn
	}
	return LevelInfo
}

// eventWarnings are the activity feed's kinds that read as warnings: things
// that went wrong without being a failure of Outpost's own.
var eventWarnings = []string{"exited", "health-fail", "sign-in-fail", "password-fail", "denied", "token-fail"}

// FromEvent is an activity-feed entry as a record, so it can be filtered and
// shown beside the logs.
func FromEvent(service, kind, message string, at time.Time) Record {
	level := LevelInfo
	switch {
	case kind == "error":
		level = LevelError
	case contains(eventWarnings, kind):
		level = LevelWarn
	}
	return Record{
		Source: SourceEvents, Service: service, Time: at, Level: level,
		Message: message, Fields: map[string]string{"kind": kind}, Line: kind + " " + message,
	}
}

// Where is where a line came from, as a plain-text view marks it: an app's
// own output by the app, and Outpost's own lines by what kind they are
// (the daemon's part, "access" or "events") and then the app they're about.
func Where(r Record) string {
	what := ""
	switch r.Source {
	case SourceDaemon:
		what = "outpost"
		if r.Component != "" {
			what = r.Component
		}
	case SourceAccess, SourceEvents:
		what = r.Source
	default:
		return r.Service
	}
	if r.Service == "" {
		return what
	}
	return what + " " + r.Service
}

// Merge interleaves records from several sources by time and keeps the
// newest n of them; n of 0 is DefaultLines.
func Merge(n int, lists ...[]Record) []Record {
	all := []Record{}
	for _, l := range lists {
		all = append(all, l...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	if n = (Query{Lines: n}).limit(); len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// Reader reads any source: the files under Dir directly, and the activity
// feed through Events, which only the daemon has.
type Reader struct {
	Dir string
	// Services lists the declared services, for reading every app's output.
	Services func(ctx context.Context) ([]string, error)
	// Events reads the activity feed, newest first, at most limit entries;
	// nil when there's no feed to read.
	Events func(ctx context.Context, service string, limit int) ([]Record, error)
}

// ErrNoEvents is a reader asked for the activity feed without one.
var ErrNoEvents = errors.New("the activity feed comes from the daemon, which isn't running")

// eventsRead is how much of the feed is read to find the newest matching
// entries: the feed is in the state store, and is read whole, not backward.
const eventsRead = 2000

// Read returns source's newest lines matching q, oldest first. service, when
// set, keeps lines about that service; for apps' output it names whose.
func (rd Reader) Read(ctx context.Context, source, service string, q Query) ([]Record, error) {
	if service != "" {
		q.Service = service
	}
	switch source {
	case "", SourceApp:
		if service != "" {
			if q.Apps != nil && !contains(q.Apps, service) {
				return []Record{}, nil
			}
			recs, err := Read(rd.Dir, service, q)
			if errors.Is(err, ErrNoLog) {
				return []Record{}, nil
			}
			return recs, err
		}
		names, err := rd.Services(ctx)
		if err != nil {
			return nil, err
		}
		if q.Apps != nil {
			var mine []string
			for _, n := range names {
				if contains(q.Apps, n) {
					mine = append(mine, n)
				}
			}
			names = mine
		}
		sort.Strings(names)
		recs, err := ReadAll(rd.Dir, names, q)
		if recs == nil && err == nil {
			recs = []Record{}
		}
		return recs, err
	case SourceDaemon:
		return ReadDaemon(rd.Dir, q)
	case SourceTailnet:
		q.Component = tailnetComponent
		return ReadDaemon(rd.Dir, q)
	case SourceAccess:
		return ReadAccess(rd.Dir, q)
	case SourceEvents:
		return rd.readEvents(ctx, q)
	case SourceAll:
		lists := make([][]Record, 0, 4)
		for _, s := range []string{SourceApp, SourceDaemon, SourceAccess, SourceEvents} {
			if s == SourceEvents && rd.Events == nil {
				continue
			}
			recs, err := rd.Read(ctx, s, service, q)
			if err != nil {
				return nil, err
			}
			lists = append(lists, recs)
		}
		return Merge(q.limit(), lists...), nil
	}
	return nil, fmt.Errorf("no such source %q: use one of %s", source, strings.Join(Sources, ", "))
}

func (rd Reader) readEvents(ctx context.Context, q Query) ([]Record, error) {
	if rd.Events == nil {
		return nil, ErrNoEvents
	}
	newestFirst, err := rd.Events(ctx, q.Service, eventsRead)
	if err != nil {
		return nil, err
	}
	want := q.limit()
	out := []Record{}
	for _, r := range newestFirst {
		if !q.Since.IsZero() && !r.Time.After(q.Since) {
			break
		}
		if q.matches(r) {
			out = append(out, r)
			if len(out) >= want {
				break
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

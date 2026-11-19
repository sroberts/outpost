package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/auth"
)

// LogSource reads services' captured output; service "" reads every service, interleaved.
type LogSource interface {
	Logs(ctx context.Context, service string, q applog.Query) ([]applog.Record, error)
}

const (
	// logsAPIPath is the JSON form of the Logs page, for an admin's scripts.
	// It is under /.outpost/ so no app path or portal page can collide with it.
	logsAPIPath = "/.outpost/api/logs"

	// logsDefaultLines is what the page and the API show when not asked for
	// a number; logsMaxLines is the most either reads at once. Lines can be up
	// to 64 KiB each, and a reply is built whole in memory.
	logsDefaultLines = 200
	logsMaxLines     = 2000
	// logsMaxBytes bounds the text in one reply; the newest lines are kept.
	logsMaxBytes = 4 << 20
	// logsMaxContains bounds the text filter, which is matched against every
	// line read.
	logsMaxContains = 1000
)

// isOwnAPIPath reports whether p is one of Outpost's own endpoints on the
// portal that take an admin's access token. They are answered before the
// portal refuses tokens.
func isOwnAPIPath(p string) bool {
	return p == logsAPIPath || p == ownMCPPath
}

// logsRequest is which lines a page, API or MCP caller asked for.
type logsRequest struct {
	service string // "" is every service the caller may read
	q       applog.Query
}

// errBadLogs is a request that names something impossible; its text is
// safe to show the caller.
type errBadLogs struct{ msg string }

func (e errBadLogs) Error() string { return e.msg }

func badLogs(format string, v ...any) error { return errBadLogs{fmt.Sprintf(format, v...)} }

// parseLogsQuery reads the page's and the API's query parameters.
func parseLogsQuery(v url.Values) (logsRequest, error) {
	lr := logsRequest{service: strings.TrimSpace(v.Get("service"))}
	lines := logsDefaultLines
	if s := strings.TrimSpace(v.Get("lines")); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			return lr, badLogs("lines must be a whole number above 0")
		}
		lines = n
	}
	var err error
	if lr.q, err = logsQuery(lines, v.Get("level"), v.Get("stream"), v.Get("q"), v.Get("since")); err != nil {
		return lr, err
	}
	return lr, nil
}

// logsQuery checks and assembles a query from what a caller sent, the same
// way for every way in.
func logsQuery(lines int, level, stream, contains, since string) (applog.Query, error) {
	q := applog.Query{Lines: min(lines, logsMaxLines)}
	if level = strings.TrimSpace(level); level != "" {
		if q.Level = applog.NormalizeLevel(level); q.Level == applog.LevelNone {
			return q, badLogs("level must be one of %s", strings.Join(applog.Levels, ", "))
		}
	}
	switch stream = strings.TrimSpace(stream); stream {
	case "", applog.Stdout, applog.Stderr:
		q.Stream = stream
	default:
		return q, badLogs("stream must be %q or %q", applog.Stdout, applog.Stderr)
	}
	if len(contains) > logsMaxContains {
		return q, badLogs("the text to look for can be at most %d bytes", logsMaxContains)
	}
	q.Contains = contains
	if since = strings.TrimSpace(since); since != "" {
		t, err := parseSince(since, time.Now())
		if err != nil {
			return q, err
		}
		q.Since = t
	}
	return q, nil
}

// parseSince takes an RFC 3339 time, or a duration such as "15m" meaning that
// long before now, which is what a person or a model usually has in mind.
func parseSince(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(-d), nil
	}
	return time.Time{}, badLogs("since must be an RFC 3339 time, such as 2026-11-18T12:00:00Z, or a duration, such as 15m")
}

// logsAccess is who is reading logs, and which services they may read.
type logsAccess struct {
	user auth.User
	// scope is the access token's apps, when it was given some; nil means
	// every service.
	scope []string
	// reject turns a token away as if it were bad, for a service outside its
	// scope; nil for a session.
	reject func(subject string)
	// used records the token's use, once every check has passed; nil for a
	// session, which logsCaller has already touched.
	used func()
}

// passed records that the caller got what they asked for.
func (a logsAccess) passed() {
	if a.used != nil {
		a.used()
	}
}

func (a logsAccess) reaches(service string) bool {
	if a.scope == nil {
		return true
	}
	for _, s := range a.scope {
		if s == service {
			return true
		}
	}
	return false
}

// logsCaller decides who an API or MCP request comes from. It reports false
// once it has answered the request itself.
//
// A request presenting an access token stands or falls by it: a wrong one is
// the shared 401, counted against the address, and never falls back to the
// session cookie beside it. A token works here when its user is an admin
// (LookupAccessToken has already turned away a disabled user, an expired or
// revoked token), and was not made for the internet: those exist for one
// app's Funnel and are no business of the portal's. A token scoped to apps
// reads only those apps' logs.
//
// Without a token, the session cookie counts only when cookies is set: the API is read-only
// and GET, so a cross-site page can make a browser send it but can't read
// the answer. The MCP endpoint takes POSTs, so it never reads a cookie.
func (g *Gateway) logsCaller(w http.ResponseWriter, r *http.Request, what string, cookies bool) (logsAccess, bool) {
	presented := presentedAccessTokens(r.Header)
	// Nothing here is proxied, but the token goes no further either way.
	removeAccessTokens(r.Header)
	if len(presented) > 0 {
		pt, ok := g.lookupAccessToken(w, r, presented, what)
		if !ok {
			return logsAccess{}, false
		}
		if pt.token.Internet {
			pt.reject(fmt.Sprintf("for %s to %s from %s with an internet token", pt.user.Username, what, pt.ip))
			return logsAccess{}, false
		}
		if !pt.user.Admin {
			g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", pt.user.Username, what), false)
			http.Error(w, "admins only", http.StatusForbidden)
			return logsAccess{}, false
		}
		a := logsAccess{user: pt.user, reject: pt.reject, used: func() { g.touchAccessToken(pt.token) }}
		if len(pt.token.Apps) > 0 {
			a.scope = pt.token.Apps
		}
		return a, true
	}
	if !cookies {
		g.unauthorizedToken(w)
		return logsAccess{}, false
	}
	tok, s, u, ok := g.session(r)
	// An app's session is for that app, not the portal.
	if !ok || s.App != "" {
		g.unauthorizedToken(w)
		return logsAccess{}, false
	}
	g.touch(tok, s)
	if !u.Admin {
		http.Error(w, "admins only", http.StatusForbidden)
		return logsAccess{}, false
	}
	return logsAccess{user: u}, true
}

// logsService is a declared service as the log views name it, and as
// list_services gives it.
type logsService struct {
	Name   string `json:"name"`
	Title  string `json:"title"`
	Status string `json:"status"`
}

// logsServices lists the declared services a caller may read, by title.
func (g *Gateway) logsServices(ctx context.Context, a logsAccess) ([]logsService, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sts, err := g.services.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]logsService, 0, len(sts))
	for _, st := range sts {
		if !a.reaches(st.Name) {
			continue
		}
		title := st.Title
		if title == "" {
			title = titleCase(st.Name)
		}
		out = append(out, logsService{Name: st.Name, Title: title, Status: st.Status})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

// errNoSuchService is a service that isn't declared, or isn't the caller's
// to read: the two read alike.
var errNoSuchService = errors.New("no such service")

// readLogs reads what lr asks for, from the services a caller may read. A
// service is read only once it's known to be declared, so nothing a caller
// names is ever taken as a path.
func (g *Gateway) readLogs(ctx context.Context, a logsAccess, svcs []logsService, lr logsRequest, maxBytes int) ([]applog.Record, error) {
	if lr.service != "" {
		found := false
		for _, s := range svcs {
			found = found || s.Name == lr.service
		}
		if !found {
			return nil, errNoSuchService
		}
		recs, err := g.logs.Logs(ctx, lr.service, lr.q)
		if err != nil {
			return nil, err
		}
		return capRecords(recs, maxBytes), nil
	}
	if a.scope == nil {
		recs, err := g.logs.Logs(ctx, "", lr.q)
		if err != nil {
			return nil, err
		}
		return capRecords(recs, maxBytes), nil
	}
	// A scoped token reads its own services, interleaved as "" would be.
	var all []applog.Record
	for _, s := range svcs {
		recs, err := g.logs.Logs(ctx, s.Name, lr.q)
		if err != nil {
			return nil, err
		}
		all = append(all, recs...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].Time.Before(all[j].Time) })
	if n := lr.q.Lines; n > 0 && len(all) > n {
		all = all[len(all)-n:]
	}
	return capRecords(all, maxBytes), nil
}

// capRecords keeps the newest records whose text fits in max bytes.
func capRecords(recs []applog.Record, max int) []applog.Record {
	total := 0
	for i := len(recs) - 1; i >= 0; i-- {
		total += len(recs[i].Line) + len(recs[i].Message)
		if total > max {
			return recs[i+1:]
		}
	}
	if recs == nil {
		return []applog.Record{}
	}
	return recs
}

// jsonErrors turns a plain-text error, from http.Error here or in the token
// code this shares with the rest of the gateway, into {"error": "..."}, so a
// script reading the API gets JSON whatever went wrong.
type jsonErrors struct {
	http.ResponseWriter
	failed bool
}

func (j *jsonErrors) WriteHeader(status int) {
	if status >= 400 {
		j.failed = true
		j.Header().Set("Content-Type", "application/json")
	}
	j.ResponseWriter.WriteHeader(status)
}

func (j *jsonErrors) Write(b []byte) (int, error) {
	if !j.failed {
		return j.ResponseWriter.Write(b)
	}
	// http.Error writes its message, and a newline, in one Write.
	out, err := json.Marshal(map[string]string{"error": strings.TrimSpace(string(b))})
	if err != nil {
		return 0, err
	}
	if _, err := j.ResponseWriter.Write(append(out, '\n')); err != nil {
		return 0, err
	}
	return len(b), nil
}

// apiHeaders are on every answer from Outpost's own API: it is never a page,
// never stored, and never anything an app or a browser could treat as one.
func apiHeaders(h http.Header) {
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'; frame-ancestors 'none'")
}

// serveLogsAPI answers GET /.outpost/api/logs with {"logs": [...]}, oldest
// first. See logsCaller for who may ask.
func (g *Gateway) serveLogsAPI(rw http.ResponseWriter, r *http.Request) {
	w := &jsonErrors{ResponseWriter: rw}
	apiHeaders(w.Header())
	if g.logs == nil {
		removeAccessTokens(r.Header)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		removeAccessTokens(r.Header)
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a, ok := g.logsCaller(w, r, "the logs API", true)
	if !ok {
		return
	}
	lr, err := parseLogsQuery(r.URL.Query())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if lr.service != "" && !a.reaches(lr.service) {
		// As a bad token answers, so a scoped token can't be used to learn
		// which other services there are.
		a.reject(fmt.Sprintf("for %s to the logs of %s from %s", a.user.Username, lr.service, g.clientIP(r)))
		return
	}
	a.passed()
	svcs, err := g.logsServices(r.Context(), a)
	if err != nil {
		g.logf("gateway: logs: list services: %v", err)
		http.Error(w, "couldn't list services", http.StatusInternalServerError)
		return
	}
	recs, err := g.readLogs(r.Context(), a, svcs, lr, logsMaxBytes)
	if errors.Is(err, errNoSuchService) {
		http.Error(w, "no such service", http.StatusNotFound)
		return
	}
	if err != nil {
		g.logf("gateway: logs: %v", err)
		http.Error(w, "couldn't read the logs", http.StatusInternalServerError)
		return
	}
	body, err := json.Marshal(struct {
		Logs []applog.Record `json:"logs"`
	}{recs})
	if err != nil {
		g.logf("gateway: logs: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(append(body, '\n')) // a client that hung up has nothing to be told
}

// logsView is the Logs page's state, for the template. Every string in it is
// text: html/template escapes it, and none may ever become template.HTML,
// since a log line is whatever a service chose to print.
type logsView struct {
	Services []logsService
	Service  string
	Levels   []string
	Level    string
	Stream   string
	Contains string
	Lines    int
	Rows     []logRow
	All      bool // reading every service, so each row names its own
	More     bool // a filter behind "More filters" is set, so it opens
	Error    string
	Self     string // this view's URL, to refresh it
}

type logRow struct {
	When, Stamp     string // shown, and machine-readable
	Service, Title  string
	Level, Stream   string
	Message, Fields string
}

// logsPage shows an admin the newest lines of one service's output, or of
// every service's, oldest at the top.
func (g *Gateway) logsPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminUser(w, r, "/logs", "Only an admin can read apps' logs.")
	if !ok {
		return
	}
	a := logsAccess{user: u}
	v := logsView{Levels: applog.Levels, Lines: logsDefaultLines, Self: "/logs?" + r.URL.Query().Encode()}
	status := http.StatusOK
	svcs, err := g.logsServices(r.Context(), a)
	if err != nil {
		g.logf("gateway: logs: list services: %v", err)
	}
	v.Services = svcs
	titles := map[string]string{}
	for _, s := range svcs {
		titles[s.Name] = s.Title
	}

	q := r.URL.Query()
	v.Service, v.Level, v.Stream, v.Contains = q.Get("service"), q.Get("level"), q.Get("stream"), q.Get("q")
	v.More = v.Stream != "" || v.Contains != "" || q.Has("lines")
	lr, perr := parseLogsQuery(q)
	switch {
	case perr != nil:
		status, v.Error = http.StatusBadRequest, perr.Error()
	case err != nil:
		status, v.Error = http.StatusInternalServerError, "Couldn't list the apps. Try again."
	default:
		v.Lines, v.Level = lr.q.Lines, lr.q.Level
		v.All = lr.service == ""
		recs, rerr := g.readLogs(r.Context(), a, svcs, lr, logsMaxBytes)
		switch {
		case errors.Is(rerr, errNoSuchService):
			status, v.Error = http.StatusNotFound, "There's no app by that name."
		case rerr != nil:
			g.logf("gateway: logs: %v", rerr)
			status, v.Error = http.StatusInternalServerError, "Couldn't read the logs. Try again."
		default:
			v.Rows = logRows(recs, titles)
		}
	}
	g.render(w, r, status, "logs", pageData{
		Title: "Logs",
		Who:   displayName(u),
		Admin: u.Admin,
		Logs:  &v,
	})
}

func logRows(recs []applog.Record, titles map[string]string) []logRow {
	rows := make([]logRow, len(recs))
	for i, rec := range recs {
		row := logRow{Service: rec.Service, Title: titles[rec.Service], Level: rec.Level, Stream: rec.Stream, Message: rec.Message}
		if row.Title == "" {
			row.Title = rec.Service
		}
		if !rec.Time.IsZero() {
			lt := rec.Time.Local()
			row.When, row.Stamp = lt.Format("Jan 2 15:04:05"), lt.Format(time.RFC3339Nano)
		}
		if row.Message == "" {
			row.Message = rec.Line
		}
		// The level has its own column; a plain line that opened with it
		// would otherwise say it twice.
		if row.Level != "" && row.Message == rec.Line {
			row.Message = applog.TrimLevel(row.Message)
		}
		keys := make([]string, 0, len(rec.Fields))
		for k := range rec.Fields {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for j, k := range keys {
			val := rec.Fields[k]
			if strings.ContainsAny(val, " \t\"") {
				val = strconv.Quote(val)
			}
			parts[j] = k + "=" + val
		}
		row.Fields = strings.Join(parts, " ")
		rows[i] = row
	}
	return rows
}

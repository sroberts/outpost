package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"github.com/charmbracelet/log"

	"github.com/sroberts/outpost/internal/applog"
	"github.com/sroberts/outpost/internal/logging"
)

// RequestIDHeader carries the id the gateway gives every request: back to
// the client, and on to the app with its identity, so one request can be
// followed from the gateway's logs into the app's. A client's own is
// stripped with the rest of X-Outpost-*, so none can choose it.
const RequestIDHeader = "X-Outpost-Request-Id"

// lg is where a line goes, with kv as its fields.
func (g *Gateway) lg(kv ...any) logging.Line { return logging.To(g.logger, g.logf, kv...) }

// logsRequests is whether each request gets a line: only with a logger at
// debug, so a gateway at info wraps nothing for it.
func (g *Gateway) logsRequests() bool {
	return g.logf == nil && g.logger != nil && g.logger.GetLevel() <= log.DebugLevel
}

// requestNote is what the handlers learn about a request that its log line
// and its access log entry should say: who it went through as, or why it was
// turned away.
type requestNote struct {
	id      string
	user    string
	refused string
	// session is the user whose session or token the request carried, named
	// when nothing more definite was learned and the request wasn't refused.
	session string
	token   string // the id of the access token presented, never the token
	app     string // the service the request was for
	bytes   int64
}

type requestNoteKey struct{}

func noteOf(r *http.Request) *requestNote {
	n, _ := r.Context().Value(requestNoteKey{}).(*requestNote)
	return n
}

// noteUser and noteRefused fill in the request's line, if it has one.
func noteUser(r *http.Request, user string) {
	if n := noteOf(r); n != nil {
		n.user = user
	}
}

func noteRefused(r *http.Request, why string) {
	if n := noteOf(r); n != nil {
		n.refused = why
	}
}

// noteSession, noteToken and noteApp fill in the access log's entry.
func noteSession(r *http.Request, user string) {
	if n := noteOf(r); n != nil {
		n.session = user
	}
}

func noteToken(r *http.Request, id string) {
	if n := noteOf(r); n != nil {
		n.token = id
	}
}

func noteApp(r *http.Request, app string) {
	if n := noteOf(r); n != nil {
		n.app = app
	}
}

// requestIDOf is the id the gateway gave r.
func requestIDOf(r *http.Request) string {
	if n := noteOf(r); n != nil {
		return n.id
	}
	return ""
}

// who is the user a request's entry names.
func (n *requestNote) who() string {
	if n.user == "" && n.refused == "" {
		return n.session
	}
	return n.user
}

// loggedResponse is a statusRecorder that a ResponseController can see
// through, so a proxied WebSocket can still take over the connection while
// its request is being logged. It counts the body for the access log.
type loggedResponse struct {
	*statusRecorder
	note *requestNote
}

func (l loggedResponse) Write(b []byte) (int, error) {
	n, err := l.statusRecorder.Write(b)
	l.note.bytes += int64(n)
	return n, err
}

func (l loggedResponse) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// observe serves r through next under an id of its own, and records it: in
// the access log, when there is one, and at debug in the daemon's log. ip is
// the client's address as the listener that took the request knows it.
func (g *Gateway) observe(w http.ResponseWriter, r *http.Request, host string, public bool, ip func(*http.Request) string, next func(http.ResponseWriter, *http.Request)) {
	note := &requestNote{id: requestID()}
	w.Header().Set(RequestIDHeader, note.id)
	r = r.WithContext(context.WithValue(r.Context(), requestNoteKey{}, note))
	debug := g.logsRequests()
	if !debug && g.access == nil {
		next(w, r)
		return
	}
	g.logRequest(w, r, host, public, ip(r), note, debug, next)
}

// logRequest serves r through next and records it: the host, method, path
// (never the query, which can carry a sign-in code or the setup link's
// token), status, time taken, the client, and who it went through as or why
// it was refused. Nothing from the headers or the body is recorded, so no
// cookie or token can reach a log by this route.
func (g *Gateway) logRequest(w http.ResponseWriter, r *http.Request, host string, public bool, ip string, note *requestNote, debug bool, next func(http.ResponseWriter, *http.Request)) {
	start := time.Now()
	method, path := r.Method, r.URL.Path
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next(loggedResponse{rec, note}, r)
	took := time.Since(start)

	if g.access != nil {
		g.writeAccess(applog.AccessEntry{
			Time: start.UTC(), RequestID: note.id, Method: method, Host: host, Path: clip(path, maxLoggedPath),
			App: note.app, Status: rec.status, Bytes: note.bytes, Millis: float64(took.Microseconds()) / 1000,
			IP: ip, User: note.who(), Token: note.token, Refused: note.refused, Public: public,
		})
	}
	if !debug {
		return
	}
	kv := []any{"request_id", note.id, "host", host, "method", method, "path", path,
		"status", rec.status, "took", took.Round(time.Microsecond), "ip", ip}
	if note.user != "" {
		kv = append(kv, "user", note.user)
	}
	if note.refused != "" {
		kv = append(kv, "refused", note.refused)
	}
	g.lg(kv...).Debugf("%s %s%s -> %d", method, host, path, rec.status)
}

// maxLoggedPath bounds a path in the access log: a client chooses it, and
// every view reads an entry whole.
const maxLoggedPath = 2048

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// writeAccess appends one entry to the access log, in one write so that
// concurrent requests' entries never interleave. A log that can't be
// written is said once, not on every request.
func (g *Gateway) writeAccess(e applog.AccessEntry) {
	b, err := json.Marshal(e)
	if err == nil {
		_, err = g.access.Write(append(b, '\n'))
	}
	if err != nil && g.accessFailed.CompareAndSwap(false, true) {
		g.lg("err", err).Errorf("gateway: access log: %v; requests aren't being recorded", err)
	}
}

// requestID tells one request from another, across the gateway's logs and
// the app's.
func requestID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // never fails; see crypto/rand
	return hex.EncodeToString(b)
}

package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/charmbracelet/log"

	"github.com/sroberts/outpost/internal/logging"
)

// lg is where a line goes, with kv as its fields.
func (g *Gateway) lg(kv ...any) logging.Line { return logging.To(g.logger, g.logf, kv...) }

// logsRequests is whether each request gets a line: only with a logger at
// debug, so a gateway at info wraps nothing and pays nothing.
func (g *Gateway) logsRequests() bool {
	return g.logf == nil && g.logger != nil && g.logger.GetLevel() <= log.DebugLevel
}

// requestNote is what the handlers learn about a request that its log line
// should say: who it went through as, or why it was turned away.
type requestNote struct {
	user    string
	refused string
}

type requestNoteKey struct{}

// noteUser and noteRefused fill in the request's line, if it has one.
func noteUser(r *http.Request, user string) {
	if n, ok := r.Context().Value(requestNoteKey{}).(*requestNote); ok {
		n.user = user
	}
}

func noteRefused(r *http.Request, why string) {
	if n, ok := r.Context().Value(requestNoteKey{}).(*requestNote); ok {
		n.refused = why
	}
}

// loggedResponse is a statusRecorder that a ResponseController can see
// through, so a proxied WebSocket can still take over the connection while
// its request is being logged.
type loggedResponse struct{ *statusRecorder }

func (l loggedResponse) Unwrap() http.ResponseWriter { return l.ResponseWriter }

// logRequest serves r through next and logs a line about it at debug: the
// host, method, path (never the query, which can carry a sign-in code or
// the setup link's token), status, time taken, the client, and who it went
// through as or why it was refused. Nothing from the headers or the body is
// logged, so no cookie or token can reach the log by this route.
func (g *Gateway) logRequest(w http.ResponseWriter, r *http.Request, host string, next func(http.ResponseWriter, *http.Request)) {
	start := time.Now()
	method, path, ip := r.Method, r.URL.Path, g.clientIP(r)
	note := &requestNote{}
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next(loggedResponse{rec}, r.WithContext(context.WithValue(r.Context(), requestNoteKey{}, note)))

	kv := []any{"request_id", requestID(), "host", host, "method", method, "path", path,
		"status", rec.status, "took", time.Since(start).Round(time.Microsecond), "ip", ip}
	if note.user != "" {
		kv = append(kv, "user", note.user)
	}
	if note.refused != "" {
		kv = append(kv, "refused", note.refused)
	}
	g.lg(kv...).Debugf("%s %s%s -> %d", method, host, path, rec.status)
}

// requestID tells one request's line from another's when a client makes the
// same request twice; nothing else keeps it.
func requestID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b) // never fails; see crypto/rand
	return hex.EncodeToString(b)
}

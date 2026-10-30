package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/sroberts/outpost/internal/auth"
)

// accessTokenHeader is the header for a client that needs Authorization for
// the app's own credential. It isn't X-Outpost-*, because those are stripped
// before anything can read them.
const accessTokenHeader = "Outpost-Token"

// bearerAccessToken returns the token an Authorization value carries, if it is
// one of ours. Anything else, another scheme or a bearer token without the
// prefix, belongs to the app and is none of the gateway's business.
func bearerAccessToken(v string) (string, bool) {
	v = strings.TrimSpace(v)
	i := strings.IndexAny(v, " \t")
	if i < 0 || !strings.EqualFold(v[:i], "Bearer") {
		return "", false
	}
	// Whatever follows is kept whole, so a value with a stray extra word is
	// still treated as ours and fails, rather than reaching the app with a
	// token in it.
	tok := strings.TrimSpace(v[i+1:])
	return tok, strings.HasPrefix(tok, auth.AccessTokenPrefix)
}

// presentedAccessTokens lists every token a request presents. Every value of
// both headers is looked at, not just the first: a second Authorization line
// is sent on to the app like the first, so it can't be allowed to carry a token
// the gateway didn't see. Anything in the dedicated header counts, prefix or
// not, because it has no other meaning, and an empty one still counts as trying.
func presentedAccessTokens(h http.Header) []string {
	presented := append([]string(nil), h.Values(accessTokenHeader)...)
	for _, v := range h.Values("Authorization") {
		if tok, ok := bearerAccessToken(v); ok {
			presented = append(presented, tok)
		}
	}
	return presented
}

// removeAccessTokens takes every token out of a request's headers, leaving an
// app's own Authorization exactly as sent.
func removeAccessTokens(h http.Header) {
	h.Del(accessTokenHeader)
	var kept []string
	for _, v := range h.Values("Authorization") {
		if _, ok := bearerAccessToken(v); !ok {
			kept = append(kept, v)
		}
	}
	h.Del("Authorization")
	if len(kept) > 0 {
		h["Authorization"] = kept
	}
}

// serveWithAccessToken handles a request that presented a token, whose headers
// no longer carry it. Whatever else the request has, such as a good session
// cookie or a public path, nothing here falls back on it: a client that sent a
// token wants to know it failed, and a wrong one mustn't read as anonymous.
func (g *Gateway) serveWithAccessToken(w http.ResponseWriter, r *http.Request, rt route, presented []string) {
	// The gateway's own endpoints on an app host are about browser sessions,
	// and the callback would mint one.
	if strings.HasPrefix(r.URL.Path, reservedPrefix) {
		g.refuseAccessToken(w, r)
		return
	}

	ip := g.clientIP(r)
	key := "ip:" + ip
	now := g.now()
	// Before the lookup, so a good token is turned away too while its address
	// is backing off, and the answer can't be used to test one.
	if d := g.tokenThrottle.wait(now, key); d > 0 {
		g.activity.failed(EventTokenFail, "from "+ip, true)
		w.Header().Set("Retry-After", strconv.Itoa(int(d.Seconds()+0.999)))
		http.Error(w, "too many rejected access tokens; try again later", http.StatusTooManyRequests)
		return
	}

	reject := func(subject string) {
		g.tokenThrottle.fail(now, key)
		g.activity.failed(EventTokenFail, subject, false)
		g.unauthorizedToken(w)
	}
	// Two tokens could only be a mistake or an attempt to confuse something,
	// and there is no right one to pick.
	if len(presented) != 1 {
		reject("from " + ip)
		return
	}
	t, u, err := g.auth.LookupAccessToken(presented[0])
	if errors.Is(err, auth.ErrNoSuchAccessToken) {
		reject("from " + ip)
		return
	}
	if err != nil {
		g.logf("gateway: %s: access token lookup: %v", rt.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The same answer as a bad token, so a token can't be used to map what
	// else exists beyond what it was given.
	if !t.Reaches(rt.Name) {
		reject(fmt.Sprintf("for %s to %s from %s", u.Username, rt.Name, ip))
		return
	}
	if !allowed(u, rt) {
		g.activity.failed(EventDenied, fmt.Sprintf("for %s to %s", u.Username, rt.Name), false)
		http.Error(w, "this account can't open this app", http.StatusForbidden)
		return
	}

	g.tokenThrottle.reset(key)
	if err := g.auth.TouchAccessToken(t); err != nil {
		g.logf("gateway: %v", err)
	}
	// A public path works the way it does for a signed-in browser, with no
	// identity, so an app can go on assuming nothing there has one.
	if g.isPublic(r, rt) {
		g.proxyPublic(w, r, rt)
		return
	}
	g.proxyAs(w, r, rt, u)
}

// unauthorizedToken answers a token that doesn't work here. It is a plain
// status and never a redirect: the client is a script with no one to sign in.
func (g *Gateway) unauthorizedToken(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "invalid access token", http.StatusUnauthorized)
}

// refuseAccessToken turns away a token presented somewhere tokens don't work.
// It isn't looked up and nothing is learned about it, so it doesn't count
// against the address.
func (g *Gateway) refuseAccessToken(w http.ResponseWriter, r *http.Request) {
	g.activity.failed(EventTokenFail, "outside an app from "+g.clientIP(r), false)
	g.unauthorizedToken(w)
}

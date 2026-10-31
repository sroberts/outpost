package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/manifest"
)

// reservedPrefix is the gateway's own corner of every app host.
const reservedPrefix = "/.outpost/"

func (g *Gateway) serveApp(w http.ResponseWriter, r *http.Request, rt route) {
	// Ahead of everything below, public paths included: a request presenting a
	// token stands or falls by it, and the token never goes any further.
	if presented := presentedAccessTokens(r.Header); len(presented) > 0 {
		removeAccessTokens(r.Header)
		g.serveWithAccessToken(w, r, rt, presented)
		return
	}
	switch r.URL.Path {
	case "/.outpost/callback":
		g.callback(w, r, rt)
	case "/.outpost/logout":
		g.appLogout(w, r)
	case cssPath:
		g.serveCSS(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, reservedPrefix) {
			http.NotFound(w, r)
			return
		}
		if g.isPublic(r, rt) {
			g.proxyPublic(w, r, rt)
			return
		}
		g.proxyApp(w, r, rt)
	}
}

// isPublic reports whether the request is for one of the app's public_paths.
// Only a path already in its simplest form counts: "/webhook/../admin" names
// /admin to the app, so it gets no exemption, and neither does an escaped
// slash or backslash that the app might decode into one.
func (g *Gateway) isPublic(r *http.Request, rt route) bool {
	if len(rt.PublicPaths) == 0 {
		return false
	}
	p := r.URL.Path
	clean := path.Clean(p)
	if strings.HasSuffix(p, "/") && clean != "/" {
		clean += "/"
	}
	raw := strings.ToLower(r.URL.EscapedPath())
	if clean != p || strings.Contains(raw, "%2f") || strings.Contains(raw, "%5c") || strings.Contains(p, "\\") {
		return false
	}
	return manifest.MatchPublicPath(rt.PublicPaths, p)
}

// proxyPublic passes a request on with no identity at all: whatever the
// client sent as X-Outpost-* is already gone, and the Outpost cookie goes
// too, so the app can't mistake it for a signed-in one.
func (g *Gateway) proxyPublic(w http.ResponseWriter, r *http.Request, rt route) {
	removeCookie(r.Header, g.cookieName)
	ctx := context.WithValue(r.Context(), targetKey{}, target{
		name:   rt.Name,
		port:   rt.Port,
		host:   g.rawHost(r),
		client: g.clientIP(r),
	})
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// callback trades a one-time code from the portal for a session on this
// app's host, then goes on to where the user was headed.
func (g *Gateway) callback(w http.ResponseWriter, r *http.Request, rt route) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	next := safeNext(q.Get("next"))
	again := g.loginURL(rt.Name, next)
	// The portal sends someone still signed in straight on, so the way out
	// of a stale link reads as continuing, not as signing in.
	onward := "Continue to " + g.titleOf(rt.Name)

	key := g.clientIP(r)
	if d := g.codeThrottle.wait(g.now(), key); d > 0 {
		g.message(w, r, http.StatusTooManyRequests, "Slow down",
			fmt.Sprintf("Too many sign-in attempts from here. Try again in %d seconds.", int(d.Seconds()+0.999)),
			again, onward)
		return
	}
	p, ok := g.codes.redeem(q.Get("code"), rt.Name, g.now())
	if !ok {
		g.codeThrottle.fail(g.now(), key)
		g.message(w, r, http.StatusBadRequest, "Sign-in link expired",
			"This sign-in link has expired or was already used.", again, onward)
		return
	}
	// The allow list may have changed in the minute since the code was
	// minted.
	if u, err := g.auth.GetUser(p.user); err == nil && !allowed(u, rt) {
		g.denied(w, r, rt, u)
		return
	}
	tok, err := g.auth.CreateSessionFrom(p.user, rt.Name, p.parent, r.UserAgent(), g.clientIP(r))
	if err != nil {
		// Most likely the portal session ended, or the user was disabled,
		// in the minute since the code was minted.
		g.logf("gateway: %s: callback for %s: %v", rt.Name, p.user, err)
		g.message(w, r, http.StatusBadRequest, "Sign in again",
			"Your sign-in ended before it reached this app.", again, "Sign in")
		return
	}
	if old, err := r.Cookie(g.cookieName); err == nil {
		_ = g.auth.RevokeSession(old.Value) // replaced either way; a stale row is pruned later
	}
	g.setCookie(w, tok)
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// appLogout, from any app host, ends the whole login: the portal session and
// every app session under it.
func (g *Gateway) appLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.message(w, r, http.StatusMethodNotAllowed, "Sign out from the button",
			"Signing out takes a button, so a link elsewhere can't sign you out.", g.portalLink(), "Back to apps")
		return
	}
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	// Looked up before the revoke ends it; recorded only once it has, so
	// the feed never says someone signed out who is still signed in.
	_, _, u, signedIn := g.session(r)
	if c, err := r.Cookie(g.cookieName); err == nil {
		if err := g.auth.RevokeFamily(c.Value); err != nil {
			g.logf("gateway: logout: %v", err)
		} else if signedIn {
			g.activity.record(EventSignOut, "%s signed out from %s", u.Username, g.clientIP(r))
		}
	}
	g.clearCookie(w)
	http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
}

// proxyApp sends a logged-in request on to the service, saying who it's
// from.
func (g *Gateway) proxyApp(w http.ResponseWriter, r *http.Request, rt route) {
	tok, s, u, ok := g.session(r)
	if !ok || s.App != rt.Name {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, g.loginURL(rt.Name, safeNext(r.URL.RequestURI())), http.StatusFound)
			return
		}
		// A redirect would turn a POST into a GET and lose its body.
		g.message(w, r, http.StatusUnauthorized, "Sign in first",
			"This needs you to be signed in. Sign in, then try it again.", g.loginURL(rt.Name, "/"), "Sign in")
		return
	}
	g.touch(tok, s)
	if !allowed(u, rt) {
		g.denied(w, r, rt, u)
		return
	}
	g.proxyAs(w, r, rt, u)
}

// proxyAs sends a request on to the service as u, who has already been
// through the allow check. Every way of being signed in comes through here, so
// an app can't tell them apart.
func (g *Gateway) proxyAs(w http.ResponseWriter, r *http.Request, rt route, u auth.User) {
	idTok, err := g.identityToken(u, rt.Name)
	if err != nil {
		g.logf("gateway: %s: identity token: %v", rt.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The app never sees Outpost's own credential.
	removeCookie(r.Header, g.cookieName)

	ctx := context.WithValue(r.Context(), targetKey{}, target{
		name:   rt.Name,
		port:   rt.Port,
		host:   g.rawHost(r),
		client: g.clientIP(r),
		admin:  u.Admin,
		user:   &u,
		token:  idTok,
	})
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

// setIdentity sets the headers an app reads to learn who is calling.
func setIdentity(h http.Header, u auth.User, token string) {
	h.Set("X-Outpost-User", u.Username)
	h.Set("X-Outpost-Name", u.DisplayName)
	h.Set("X-Outpost-Email", u.Email)
	h.Set("X-Outpost-Groups", strings.Join(u.Groups, ","))
	h.Set("X-Outpost-Identity", token)
}

type targetKey struct{}

// target is where one proxied request goes, and what to tell the app about
// where it came from.
type target struct {
	name   string
	port   int
	host   string // as the client asked for it, port included
	client string
	// admin is whether the caller may be told how to look closer if the
	// app doesn't answer. The cookie is gone by then, so it's decided here.
	admin bool
	// user and token are who the request is from, for the identity headers;
	// nil for a public path, which carries none.
	user  *auth.User
	token string
}

func (g *Gateway) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(targetKey{}).(target)
			pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(t.port))})
			// The host the browser used, so an app that builds absolute URLs
			// builds reachable ones.
			pr.Out.Host = t.host
			// Identity goes on the outgoing copy, here and not before the
			// proxy is called: the proxy first drops every header the
			// client's own Connection header names, so anything set
			// earlier could be taken back by naming it there. Stripped
			// again first, since a look-alike name survives the clone.
			stripIdentity(pr.Out.Header)
			if t.user != nil {
				setIdentity(pr.Out.Header, *t.user, t.token)
			}
			// Rewrite has already dropped any X-Forwarded-* the client sent;
			// these are the gateway's own account of the request, under
			// every name an app might take a client address from.
			pr.Out.Header.Set("X-Forwarded-For", t.client)
			pr.Out.Header.Set("X-Forwarded-Host", t.host)
			pr.Out.Header.Set("X-Forwarded-Proto", g.scheme)
			pr.Out.Header.Set("X-Real-IP", t.client)
			pr.Out.Header.Del("True-Client-IP")
			pr.Out.Header.Del("X-Client-IP")
		},
		// Flush as data arrives, so server-sent events stream.
		FlushInterval: -1,
		Transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			IdleConnTimeout:     90 * time.Second,
			MaxIdleConnsPerHost: 8,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			t, _ := r.Context().Value(targetKey{}).(target)
			if errors.Is(err, context.Canceled) {
				return // the client went away; nobody to tell
			}
			g.logf("gateway: %s: %v", t.name, err)
			g.unavailable(w, r, t.name, t.admin)
		},
	}
}

// unavailable is the page for an app the proxy couldn't reach. It says what
// the supervisor knows, so a service that has stopped isn't described as
// merely slow.
func (g *Gateway) unavailable(w http.ResponseWriter, r *http.Request, name string, admin bool) {
	title := g.titleOf(name)
	d := pageData{
		Title:    title + " is unavailable",
		Message:  title + " didn't answer. It may be busy or starting; try again in a moment.",
		Link:     g.portalLink(),
		LinkText: "Back to apps",
	}
	if rt, ok := g.routes.get(name); ok {
		if state, label, reason := g.describe(rt); state != stateUp {
			d.Message = ""
			st := station{Name: rt.Name, Title: title, State: state, Label: label, Reason: reason}
			if admin {
				st.Command = "outpost status " + rt.Name
			}
			d.Station = &st
		}
	}
	g.render(w, r, http.StatusBadGateway, "message", d)
}

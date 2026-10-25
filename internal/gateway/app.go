package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// reservedPrefix is the gateway's own corner of every app host.
const reservedPrefix = "/.outpost/"

func (g *Gateway) serveApp(w http.ResponseWriter, r *http.Request, rt route) {
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
		g.proxyApp(w, r, rt)
	}
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

	key := g.clientIP(r)
	if d := g.codeThrottle.wait(g.now(), key); d > 0 {
		g.message(w, r, http.StatusTooManyRequests, "Slow down",
			fmt.Sprintf("Too many sign-in attempts from here. Try again in %d seconds.", int(d.Seconds()+0.999)),
			again, "Sign in again")
		return
	}
	p, ok := g.codes.redeem(q.Get("code"), rt.Name, g.now())
	if !ok {
		g.codeThrottle.fail(g.now(), key)
		g.message(w, r, http.StatusBadRequest, "Sign-in link expired",
			"This sign-in link has expired or was already used.", again, "Sign in again")
		return
	}
	tok, err := g.auth.CreateSession(p.user, rt.Name, p.parent, r.UserAgent())
	if err != nil {
		// Most likely the portal session ended, or the user was disabled,
		// in the minute since the code was minted.
		g.logf("gateway: %s: callback for %s: %v", rt.Name, p.user, err)
		g.message(w, r, http.StatusBadRequest, "Sign in again",
			"Your sign-in ended before it reached this app.", again, "Sign in again")
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !g.sameOrigin(r) {
		http.Error(w, "cross-origin request refused", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(g.cookieName); err == nil {
		if err := g.auth.RevokeFamily(c.Value); err != nil {
			g.logf("gateway: logout: %v", err)
		}
	}
	g.clearCookie(w)
	http.Redirect(w, r, g.portalOrigin+"/login", http.StatusSeeOther)
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
		http.Error(w, "sign in at "+g.portalOrigin, http.StatusUnauthorized)
		return
	}
	g.touch(tok, s)

	idTok, err := g.identityToken(u, rt.Name)
	if err != nil {
		g.logf("gateway: %s: identity token: %v", rt.Name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	// The app never sees Outpost's own credential.
	removeCookie(r.Header, g.cookieName)
	setIdentity(r.Header, u, idTok)

	ctx := context.WithValue(r.Context(), targetKey{}, target{
		name:   rt.Name,
		port:   rt.Port,
		host:   g.rawHost(r),
		client: g.clientIP(r),
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
}

func (g *Gateway) newProxy() *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			t := pr.In.Context().Value(targetKey{}).(target)
			pr.SetURL(&url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(t.port))})
			// The host the browser used, so an app that builds absolute URLs
			// builds reachable ones.
			pr.Out.Host = t.host
			// Rewrite has already dropped any X-Forwarded-* the client sent;
			// these are the gateway's own account of the request.
			pr.Out.Header.Set("X-Forwarded-For", t.client)
			pr.Out.Header.Set("X-Forwarded-Host", t.host)
			pr.Out.Header.Set("X-Forwarded-Proto", g.scheme)
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
			http.Error(w, t.name+" isn't responding right now. Try again in a moment.", http.StatusBadGateway)
		},
	}
}

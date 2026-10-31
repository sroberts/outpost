package gateway

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// cookieMaxAge only has to outlive the session: auth.db decides when a
// session ends, and it slides with use, so the cookie is given the longest
// life browsers allow rather than a copy of an expiry that moves.
const cookieMaxAge = 400 * 24 * time.Hour

// session returns the live session the request's cookie names.
func (g *Gateway) session(r *http.Request) (string, auth.Session, auth.User, bool) {
	c, err := r.Cookie(g.cookieName)
	if err != nil || c.Value == "" {
		return "", auth.Session{}, auth.User{}, false
	}
	s, u, err := g.auth.LookupSession(c.Value)
	if err != nil {
		if !errors.Is(err, auth.ErrNoSuchSession) {
			g.logf("gateway: session lookup: %v", err)
		}
		return "", auth.Session{}, auth.User{}, false
	}
	return c.Value, s, u, true
}

// touch slides a session that's due, and leaves it alone otherwise, so a
// busy app isn't a database write per request.
func (g *Gateway) touch(tok string, s auth.Session) {
	if !s.ExpiresAt.Before(g.now().Add(auth.PortalSessionTTL - auth.RefreshAfter)) {
		return
	}
	if err := g.auth.TouchFamily(tok); err != nil && !errors.Is(err, auth.ErrNoSuchSession) {
		g.logf("gateway: touch session: %v", err)
	}
}

func (g *Gateway) setCookie(w http.ResponseWriter, tok string) {
	http.SetCookie(w, &http.Cookie{
		Name:     g.cookieName,
		Value:    tok,
		Path:     "/",
		MaxAge:   int(cookieMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// stateMaxAge is how long a browser's stamp on an app handshake lasts: two
// redirects, with room for a slow link.
const stateMaxAge = 5 * time.Minute

// stateCookieName is the cookie an app's host stamps a browser with in the
// middle of a handshake, so the session at the end goes to the browser the
// handshake started in. It takes the session cookie's prefix, and so its
// protection, under https.
func (g *Gateway) stateCookieName() string { return g.cookieName + "-state" }

func (g *Gateway) setStateCookie(w http.ResponseWriter, stamp string) {
	http.SetCookie(w, &http.Cookie{
		Name:     g.stateCookieName(),
		Value:    stamp,
		Path:     "/",
		MaxAge:   int(stateMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (g *Gateway) clearStateCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     g.stateCookieName(),
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func (g *Gateway) clearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     g.cookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   g.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// removeCookie drops one cookie from a request's Cookie headers, leaving the
// rest exactly as sent. It works on the text rather than re-encoding parsed
// cookies, which would drop any the standard library considers malformed.
func removeCookie(h http.Header, name string) {
	var kept []string
	for _, v := range h.Values("Cookie") {
		for _, part := range strings.Split(v, ";") {
			p := strings.TrimSpace(part)
			if p == "" {
				continue
			}
			if n, _, _ := strings.Cut(p, "="); strings.TrimSpace(n) == name {
				continue
			}
			kept = append(kept, p)
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// sessionCookieNames are every name the gateway's own cookies can have:
// the session, and the stamp a sign-in handshake leaves on an app's host.
// None may come from an app, whichever ones this gateway uses: an app
// setting the session could replace or clear it, or, under http with a
// Domain, plant its own on sibling apps; one setting the stamp could clear
// a handshake in flight.
var sessionCookieNames = []string{"outpost", "__Host-outpost", "outpost-state", "__Host-outpost-state"}

// removeSetCookies drops a response's Set-Cookie headers for any of names,
// keeping the rest in order and exactly as sent.
func removeSetCookies(h http.Header, names []string) {
	var kept []string
	dropped := false
	for _, v := range h.Values("Set-Cookie") {
		pair, _, _ := strings.Cut(v, ";")
		n, rest, _ := strings.Cut(pair, "=")
		n = strings.TrimSpace(n)
		if n == "" {
			// A browser sends a nameless cookie as its bare value, so
			// "=outpost=x" would arrive as outpost=x.
			n, _, _ = strings.Cut(rest, "=")
			n = strings.TrimSpace(n)
		}
		if slices.Contains(names, n) {
			dropped = true
			continue
		}
		kept = append(kept, v)
	}
	if !dropped {
		return
	}
	h.Del("Set-Cookie")
	for _, v := range kept {
		h.Add("Set-Cookie", v)
	}
}

// maxNext bounds a return path carried through the login.
const maxNext = 2048

// safeNext returns s if it's a path on the same host, and "/" otherwise. The
// login round trip carries it, so anything that could make a browser leave
// the host is refused: "//host", backslashes (which browsers read as
// slashes), and control characters (which they strip, turning "/\t/host"
// into "//host").
func safeNext(s string) string {
	if s == "" || len(s) > maxNext || s[0] != '/' || strings.HasPrefix(s, "//") {
		return "/"
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return "/"
	}
	return s
}

// knownApp returns name if it's a routed service, and "" otherwise, so a
// redirect is only ever to a host the gateway serves.
func (g *Gateway) knownApp(name string) string {
	if _, ok := g.routes.get(name); ok {
		return name
	}
	return ""
}

// loginURL is the portal's login page, coming back to app and next.
func (g *Gateway) loginURL(app, next string) string {
	q := url.Values{}
	if app != "" {
		q.Set("app", app)
	}
	if next != "" && next != "/" {
		q.Set("next", next)
	}
	if len(q) == 0 {
		return g.portalOrigin + "/login"
	}
	return g.portalOrigin + "/login?" + q.Encode()
}

// continueTo finishes a login: on to the app through a one-time code, or to
// next on the portal.
func (g *Gateway) continueTo(w http.ResponseWriter, r *http.Request, portalTok string, u auth.User, app, next string) {
	if app == "" {
		http.Redirect(w, r, next, http.StatusSeeOther)
		return
	}
	// Checked before a code exists, so someone not on the list never gets a
	// session on the app's host at all.
	if rt, ok := g.routes.get(app); ok && !allowed(u, rt) {
		g.denied(w, r, rt, u)
		return
	}
	code, err := g.codes.mint(pendingCode{user: u.Username, app: app, parent: portalTok}, g.now())
	if err != nil {
		g.logf("gateway: %v", err)
		g.message(w, r, http.StatusServiceUnavailable, "Try again", err.Error(), g.loginURL(app, next), "Try again")
		return
	}
	q := url.Values{"code": {code}}
	if next != "/" {
		q.Set("next", next)
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, g.appOrigin(app)+"/.outpost/callback?"+q.Encode(), http.StatusSeeOther)
}

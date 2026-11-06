package gateway

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/sroberts/outpost/internal/auth"
)

// maxForm bounds a form body. Nothing the portal takes comes near it.
const maxForm = 64 << 10

// passwordChecks bounds concurrent argon2id work. Each check holds 64 MiB,
// so a burst of logins must queue rather than all run at once.
const passwordChecks = 4

func (g *Gateway) servePortal(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/":
		g.home(w, r)
	case "/login":
		if r.Method == http.MethodPost {
			g.loginSubmit(w, r)
		} else {
			g.loginPage(w, r)
		}
	case "/continue":
		g.continueApp(w, r)
	case "/logout":
		g.logout(w, r)
	case "/account":
		if r.Method == http.MethodPost {
			g.accountSubmit(w, r)
		} else {
			g.accountPage(w, r)
		}
	case "/apps":
		switch {
		case g.control == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.appsSubmit(w, r)
		default:
			w.Header().Set("Allow", "POST")
			g.message(w, r, http.StatusMethodNotAllowed, "Use the buttons",
				"Apps are started, stopped and backed up from the buttons in the catalog, so a link elsewhere can't do it.", g.portalLink(), "Back to apps")
		}
	case "/updates":
		switch {
		case g.updater == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.updatesSubmit(w, r)
		default:
			g.updatesPage(w, r)
		}
	case "/add":
		switch {
		case g.adder == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.addSubmit(w, r)
		default:
			g.addPage(w, r)
		}
	case "/secrets":
		switch {
		case g.secrets == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.secretsSubmit(w, r)
		default:
			g.secretsPage(w, r)
		}
	case "/public":
		switch {
		case g.exposure == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.publicSubmit(w, r)
		default:
			g.publicPage(w, r)
		}
	case "/ai":
		switch {
		case g.ai == nil:
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
		case r.Method == http.MethodPost:
			g.aiSubmit(w, r)
		default:
			g.aiPage(w, r)
		}
	case "/updates/self":
		if g.updater == nil || g.selfUpdater == nil {
			g.message(w, r, http.StatusNotFound, "Nothing here",
				"There's no page at this address.", g.portalLink(), "Back to apps")
			return
		}
		g.selfStatusJSON(w, r)
	case "/setup":
		if r.Method == http.MethodPost {
			g.setupSubmit(w, r)
		} else {
			g.setupPage(w, r)
		}
	case jwksPath:
		g.serveJWKS(w, r)
	case cssPath:
		g.serveCSS(w, r)
	case jsPath:
		g.serveJS(w, r)
	case pollPath:
		g.servePoll(w, r)
	case returnPath:
		g.serveAsset(w, r, "static/return.js", "text/javascript; charset=utf-8")
	case menusPath:
		g.serveAsset(w, r, "static/menus.js", "text/javascript; charset=utf-8")
	case selfJSPath:
		g.serveAsset(w, r, "static/selfupdate.js", "text/javascript; charset=utf-8")
	case artPath:
		g.serveArt(w, r)
	default:
		g.message(w, r, http.StatusNotFound, "Nothing here",
			"There's no page at this address.", g.portalLink(), "Back to apps")
	}
}

// home is the portal: the apps there are, for someone logged in.
func (g *Gateway) home(w http.ResponseWriter, r *http.Request) {
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	g.touch(tok, s)

	sts := g.stations(u)
	counts := tally(sts)
	d := pageData{
		Title:    "Apps",
		Who:      displayName(u),
		Admin:    u.Admin,
		Stations: sts,
		Counts:   counts,
		Search:   len(sts) >= searchFrom,
	}
	if u.Admin && g.updater != nil {
		g.checkIfStale()
		d.Waiting = g.waiting()
		d.UpdatesLink = true
	}
	d.AddLink = u.Admin && g.adder != nil
	d.SecretsLink = u.Admin && g.secrets != nil
	d.PublicLink = u.Admin && g.exposure != nil
	d.AILink = u.Admin && g.ai != nil
	if u.Admin && g.control != nil {
		d.Poll = g.manage(d.Stations)
	}
	// A crashed app comes back on its own, with nobody's action to refresh the
	// row, and whoever is looking wants to see it happen.
	d.Poll = d.Poll || slices.ContainsFunc(d.Stations, func(s station) bool { return s.State == stateStarting })
	d.Icons = slices.ContainsFunc(d.Stations, func(s station) bool {
		return s.Usage != nil || s.Quick != nil || len(s.Acts) > 0
	})
	g.render(w, r, http.StatusOK, "portal", d)
}

func displayName(u auth.User) string {
	if u.DisplayName != "" {
		return u.DisplayName
	}
	return u.Username
}

// loginPage shows the form, or carries on at once for someone already
// logged in at the portal: that's the second app costing no typing.
func (g *Gateway) loginPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	app := g.knownApp(q.Get("app"))
	next := safeNext(q.Get("next"))
	if tok, s, u, ok := g.session(r); ok && s.App == "" {
		g.touch(tok, s)
		g.continueTo(w, r, tok, u, app, next)
		return
	}
	d := pageData{Title: "Sign in", App: app, AppTitle: g.titleOf(app), Next: next}
	if q.Get("out") == "1" {
		d.Notice = "You're signed out of every app."
	}
	g.render(w, r, http.StatusOK, "login", d)
}

// titleOf is what to call a routed app in words.
func (g *Gateway) titleOf(name string) string {
	if rt, ok := g.routes.get(name); ok && rt.Title != "" {
		return rt.Title
	}
	return name
}

func (g *Gateway) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	username := strings.ToLower(strings.TrimSpace(r.PostForm.Get("username")))
	password := r.PostForm.Get("password")
	app := g.knownApp(r.PostForm.Get("app"))
	next := safeNext(r.PostForm.Get("next"))
	form := pageData{Title: "Sign in", App: app, AppTitle: g.titleOf(app), Next: next, Username: username}

	ip := g.clientIP(r)
	keys := []string{"user:" + username, "ip:" + ip}
	attempt := fmt.Sprintf("for %s from %s", quoted(username), ip)
	// Taken before the check, not after: the check is slow by design, and
	// a burst sent together would otherwise all get in before the first
	// failure counts.
	if d := g.throttle.begin(g.now(), keys...); d > 0 {
		// Counted by address alone: a throttled client changing the name
		// it tries each time is still one line.
		g.activity.failed(EventSignInFail, "from "+ip, true)
		form.Error = "Too many attempts. Try again in " + waitWords(d) + "."
		g.render(w, r, http.StatusTooManyRequests, "login", form)
		return
	}

	g.checks <- struct{}{}
	u, err := g.auth.Authenticate(username, password)
	<-g.checks
	if err != nil {
		if !errors.Is(err, auth.ErrBadCredentials) {
			g.throttle.pass(keys...)
			g.logf("gateway: login: %v", err)
			form.Error = "Something went wrong. Try again."
			g.render(w, r, http.StatusInternalServerError, "login", form)
			return
		}
		g.throttle.fail(g.now(), keys...)
		g.activity.failed(EventSignInFail, attempt, false)
		form.Error = "Wrong username or password."
		g.render(w, r, http.StatusUnauthorized, "login", form)
		return
	}
	// The user's own backoff is over; the address keeps its failures,
	// since knowing one password clears nothing about the others tried
	// from there.
	g.throttle.reset("user:" + username)
	g.throttle.pass("ip:" + ip)

	// Always a new token, never the one the browser arrived with, so a
	// session fixed in advance by someone else is worthless.
	if old, err := r.Cookie(g.cookieName); err == nil {
		_ = g.auth.RevokeSession(old.Value) // replaced either way; a stale row is pruned later
	}
	tok, err := g.auth.CreateSessionFrom(u.Username, "", "", r.UserAgent(), ip)
	if err != nil {
		g.logf("gateway: login: %v", err)
		form.Error = "Something went wrong. Try again."
		g.render(w, r, http.StatusInternalServerError, "login", form)
		return
	}
	g.activity.settle(EventSignInFail, attempt)
	g.activity.record(EventSignIn, "%s signed in from %s", u.Username, ip)
	g.setCookie(w, tok)
	g.continueTo(w, r, tok, u, app, next)
}

// continueApp is the middle of an app handshake. The app's host has stamped
// the browser and sent it here with a fresh code, and the session that
// minted the code has to be the one this browser holds at the portal: the
// link the browser arrived on would otherwise work for any browser at all.
func (g *Gateway) continueApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	next := safeNext(q.Get("next"))
	stamp := q.Get("state")
	p, ok := g.codes.take(q.Get("code"), g.now())
	if !ok || p.state == "" || !sameToken(stamp, p.state) {
		g.message(w, r, http.StatusBadRequest, "Sign-in link expired",
			"This sign-in link has expired or was already used.", g.portalLink(), "Back to apps")
		return
	}
	again := g.loginURL(p.app, next)
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" || !sameToken(tok, p.parent) {
		// Not the sign-in this code came from: someone else's link, or a
		// session that has since ended.
		g.message(w, r, http.StatusBadRequest, "Sign in again",
			"This sign-in link was for another browser, or your sign-in has ended.", again, "Sign in")
		return
	}
	g.touch(tok, s)
	rt, ok := g.routes.get(p.app)
	if !ok {
		g.message(w, r, http.StatusNotFound, "Nothing here",
			"That app is no longer here.", g.portalLink(), "Back to apps")
		return
	}
	if !allowed(u, rt) {
		g.denied(w, r, rt, u)
		return
	}
	code, err := g.codes.mint(pendingCode{user: u.Username, app: p.app, parent: tok, state: stamp}, g.now())
	if err != nil {
		g.logf("gateway: %v", err)
		g.message(w, r, http.StatusServiceUnavailable, "Try again", err.Error(), again, "Try again")
		return
	}
	v := url.Values{"code": {code}, "state": {stamp}}
	if next != "/" {
		v.Set("next", next)
	}
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, g.appOrigin(p.app)+"/.outpost/callback?"+v.Encode(), http.StatusSeeOther)
}

// logout ends the portal session and every app session it handed out.
func (g *Gateway) logout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		g.message(w, r, http.StatusMethodNotAllowed, "Sign out from the button",
			"Signing out takes the Sign out button, so a link elsewhere can't sign you out.", g.portalLink(), "Back to apps")
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
		if err := g.auth.RevokeSession(c.Value); err != nil {
			g.logf("gateway: logout: %v", err)
		} else if signedIn {
			g.activity.record(EventSignOut, "%s signed out from %s", u.Username, g.clientIP(r))
		}
	}
	g.clearCookie(w)
	http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
}

// crossOrigin answers a form posted from another site.
func (g *Gateway) crossOrigin(w http.ResponseWriter, r *http.Request) {
	g.message(w, r, http.StatusForbidden, "Refused",
		"That form came from another site, so Outpost didn't act on it.", g.portalLink(), "Back to apps")
}

// setupValid reports whether tok is the live first-run token. Once anyone
// exists, however they were added, the link is closed for good.
func (g *Gateway) setupValid(tok string) bool {
	want := g.setup.current()
	if want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
		return false
	}
	users, err := g.auth.ListUsers()
	if err != nil {
		g.logf("gateway: setup: %v", err)
		return false
	}
	if len(users) > 0 {
		g.closeSetup()
		return false
	}
	return true
}

func (g *Gateway) closeSetup() {
	g.setup.mu.Lock()
	g.setup.token = ""
	g.setup.mu.Unlock()
}

func (g *Gateway) setupPage(w http.ResponseWriter, r *http.Request) {
	tok := r.URL.Query().Get("token")
	if !g.setupValid(tok) {
		http.NotFound(w, r)
		return
	}
	g.render(w, r, http.StatusOK, "setup", pageData{Title: "Set up", Token: tok, MinPassword: auth.MinPasswordLen})
}

func (g *Gateway) setupSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	tok := r.PostForm.Get("token")
	username := strings.ToLower(strings.TrimSpace(r.PostForm.Get("username")))
	password := r.PostForm.Get("password")
	form := pageData{Title: "Set up", Token: tok, Username: username, MinPassword: auth.MinPasswordLen}

	// One at a time, so two submissions can't both create a first admin.
	g.setup.mu.Lock()
	defer g.setup.mu.Unlock()
	want := g.setup.token
	if want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
		http.NotFound(w, r)
		return
	}
	if users, err := g.auth.ListUsers(); err != nil || len(users) > 0 {
		if err != nil {
			g.logf("gateway: setup: %v", err)
		}
		g.setup.token = ""
		http.NotFound(w, r)
		return
	}
	if password != r.PostForm.Get("confirm") {
		form.Error = "The passwords don't match."
		g.render(w, r, http.StatusBadRequest, "setup", form)
		return
	}
	// AddUser accepts an empty password, for an account handed over later;
	// the first admin needs one now.
	if err := auth.ValidPassword(password); err != nil {
		form.Error = err.Error()
		g.render(w, r, http.StatusBadRequest, "setup", form)
		return
	}
	g.checks <- struct{}{}
	err := g.auth.AddUser(auth.User{Username: username, Admin: true}, password)
	<-g.checks
	if err != nil {
		// The validators' messages say what to fix and echo nothing secret.
		form.Error = err.Error()
		g.render(w, r, http.StatusBadRequest, "setup", form)
		return
	}
	g.setup.token = ""
	g.logf("gateway: first admin %q created through the setup link", username)
	g.activity.record(EventSetup, "first admin %s created through the setup link from %s", username, g.clientIP(r))

	sess, err := g.auth.CreateSessionFrom(username, "", "", r.UserAgent(), g.clientIP(r))
	if err != nil {
		g.logf("gateway: setup: %v", err)
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	g.setCookie(w, sess)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

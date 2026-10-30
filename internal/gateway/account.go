package gateway

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/sroberts/outpost/internal/auth"
)

// accountPage shows who is signed in, a password form, and a way to end
// every session at once.
func (g *Gateway) accountPage(w http.ResponseWriter, r *http.Request) {
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		http.Redirect(w, r, "/login?"+"next=%2Faccount", http.StatusFound)
		return
	}
	g.touch(tok, s)
	d := g.accountData(u)
	if r.URL.Query().Get("changed") == "1" {
		d.Notice = "Password changed. Every other device has been signed out."
	}
	g.render(w, r, http.StatusOK, "account", d)
}

func (g *Gateway) accountData(u auth.User) pageData {
	return pageData{
		Title:       "Account",
		Who:         displayName(u),
		Username:    u.Username,
		Admin:       u.Admin,
		MinPassword: auth.MinPasswordLen,
	}
}

func (g *Gateway) accountSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	_, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/account", "Back to account")
		return
	}
	switch r.PostForm.Get("action") {
	case "password":
		g.changePassword(w, r, u)
	case "everywhere":
		n, err := g.auth.RevokeUserSessions(u.Username)
		if err != nil {
			g.logf("gateway: sign out everywhere: %v", err)
			g.message(w, r, http.StatusInternalServerError, "Not signed out", "Something went wrong. Try again.", "/account", "Back to account")
			return
		}
		g.activity.record(EventSignOut, "%s signed out everywhere from %s, ending %d %s", u.Username, g.clientIP(r), n, plural(n, "session"))
		g.clearCookie(w)
		http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
	default:
		http.Redirect(w, r, "/account", http.StatusSeeOther)
	}
}

// changePassword checks the current password, as a login does and under the
// same throttle, so a session left open on someone else's screen can't be
// turned into a stolen account.
func (g *Gateway) changePassword(w http.ResponseWriter, r *http.Request, u auth.User) {
	form := g.accountData(u)
	fail := func(status int, msg string) {
		form.Error = msg
		g.render(w, r, status, "account", form)
	}
	current := r.PostForm.Get("current")
	next := r.PostForm.Get("password")
	if next != r.PostForm.Get("confirm") {
		fail(http.StatusBadRequest, "The new passwords don't match.")
		return
	}
	if err := auth.ValidPassword(next); err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}

	ip := g.clientIP(r)
	keys := []string{"user:" + u.Username, "ip:" + ip}
	attempt := fmt.Sprintf("for %s from %s", quoted(u.Username), ip)
	if d := g.throttle.wait(g.now(), keys...); d > 0 {
		g.activity.failed(EventSignInFail, attempt, true)
		fail(http.StatusTooManyRequests, fmt.Sprintf("Too many attempts. Try again in %d seconds.", int(d.Seconds()+0.999)))
		return
	}
	g.checks <- struct{}{}
	_, err := g.auth.Authenticate(u.Username, current)
	<-g.checks
	if err != nil {
		if !errors.Is(err, auth.ErrBadCredentials) {
			g.logf("gateway: change password: %v", err)
			fail(http.StatusInternalServerError, "Something went wrong. Try again.")
			return
		}
		g.throttle.fail(g.now(), keys...)
		g.activity.failed(EventSignInFail, attempt, false)
		fail(http.StatusUnauthorized, "That isn't your current password.")
		return
	}
	g.throttle.reset(keys...)
	g.activity.settle(EventSignInFail, attempt)

	g.checks <- struct{}{}
	err = g.auth.SetPassword(u.Username, next)
	<-g.checks
	if err != nil {
		g.logf("gateway: change password: %v", err)
		fail(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	g.activity.record(EventPassword, "%s changed their password from %s, ending their other sessions", u.Username, ip)
	// SetPassword ended every session, since whoever knew the old password
	// may still be signed in somewhere; this browser starts a fresh one.
	tok, err := g.auth.CreateSession(u.Username, "", "", r.UserAgent())
	if err != nil {
		g.logf("gateway: change password: %v", err)
		g.clearCookie(w)
		http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
		return
	}
	g.setCookie(w, tok)
	http.Redirect(w, r, "/account?changed=1", http.StatusSeeOther)
}

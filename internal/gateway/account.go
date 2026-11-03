package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

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
	d := g.accountData(r, u)
	if r.URL.Query().Get("changed") == "1" {
		d.Notice = "Password changed. Every other device has been signed out, and any access tokens revoked."
	}
	g.render(w, r, http.StatusOK, "account", d)
}

func (g *Gateway) accountData(r *http.Request, u auth.User) pageData {
	return pageData{
		UpdatesLink: u.Admin && g.updater != nil,
		AddLink:     u.Admin && g.adder != nil,
		Title:       "Account",
		Who:         displayName(u),
		Username:    u.Username,
		Admin:       u.Admin,
		MinPassword: auth.MinPasswordLen,
		SignIns:     g.signIns(r, u),
	}
}

// signInView is one sign-in as the account page shows it.
type signInView struct {
	ID      string
	Device  string
	This    bool   // the sign-in this page was opened from
	Apps    string // the apps opened in it, in words
	Since   string // when it began, to the minute
	Address string // where it began, when that was kept
	Active  string // roughly when it was last used
}

// Label tells this sign-in apart from others on the same kind of device,
// for the notice after ending it and for a screen reader's button name.
func (v signInView) Label() string {
	l := v.Device + ", signed in " + v.Since
	if v.Address != "" {
		l += " from " + v.Address
	}
	return l
}

// signIns lists u's sign-ins for the account page, this one first. A
// failure to list them leaves the section out rather than the page.
func (g *Gateway) signIns(r *http.Request, u auth.User) []signInView {
	list, err := g.auth.SignIns(u.Username)
	if err != nil {
		g.logf("gateway: list sign-ins: %v", err)
		return nil
	}
	this := ""
	if c, err := r.Cookie(g.cookieName); err == nil {
		this = auth.HashToken(c.Value)
	}
	now := g.now()
	out := make([]signInView, 0, len(list))
	for _, si := range list {
		v := signInView{
			ID:      si.ID,
			Device:  DescribeAgent(si.UserAgent),
			This:    si.ID == this,
			Since:   signedInAt(now, si.CreatedAt),
			Address: si.IP,
			Active:  activeAgo(now, si.LastSeen),
		}
		titles := make([]string, 0, len(si.Apps))
		for _, a := range si.Apps {
			titles = append(titles, g.titleOf(a))
		}
		v.Apps = strings.Join(titles, ", ")
		if v.This {
			out = append([]signInView{v}, out...)
		} else {
			out = append(out, v)
		}
	}
	return out
}

// signedInAt says when a sign-in began, to the minute, which is what tells
// two on the same kind of device apart: "today at 14:32", "12 Oct at 09:10".
func signedInAt(now, at time.Time) string {
	now, at = now.Local(), at.Local()
	y1, m1, d1 := now.Date()
	y2, m2, d2 := at.Date()
	clock := at.Format("15:04")
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return "today at " + clock
	case now.AddDate(0, 0, -1).Format("2006-01-02") == at.Format("2006-01-02"):
		return "yesterday at " + clock
	case y1 == y2:
		return at.Format("2 Jan") + " at " + clock
	}
	return at.Format("2 Jan 2006")
}

// activeAgo says roughly when a sign-in was last used. A session records its
// use only when it slides, at most once a day, so the last use is somewhere
// in the day after seen, and anything finer would be made up.
func activeAgo(now, seen time.Time) string {
	days := int(now.Sub(seen) / (24 * time.Hour))
	switch {
	case days < 1:
		return "Active in the last day"
	case days == 1:
		return "Active in the last two days"
	}
	return fmt.Sprintf("Active about %d days ago", days)
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
	case "end":
		g.endSignIn(w, r, u)
	case "others":
		g.endOtherSignIns(w, r, u)
	case "everywhere":
		n, tokens, err := g.auth.RevokeUserAccess(u.Username)
		if err != nil {
			g.logf("gateway: sign out everywhere: %v", err)
			g.message(w, r, http.StatusInternalServerError, "Not signed out", "Something went wrong. Try again.", "/account", "Back to account")
			return
		}
		g.activity.record(EventSignOut, "%s signed out everywhere from %s, ending %d %s", u.Username, g.clientIP(r), n, plural(n, "session"))
		if tokens > 0 {
			g.activity.record(EventTokenRevoke, "%d access %s for %s revoked: signed out everywhere", tokens, plural(tokens, "token"), u.Username)
		}
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
	// The page, and the sign-ins query behind it, is only built to show a
	// failure; a successful change redirects.
	fail := func(status int, msg string) {
		form := g.accountData(r, u)
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
	// Taken before the check, as at sign-in, so a burst can't all get in
	// before the first failure counts.
	if d := g.throttle.begin(g.now(), keys...); d > 0 {
		g.activity.failed(EventPasswordFail, "from "+ip, true)
		fail(http.StatusTooManyRequests, "Too many attempts. Try again in "+waitWords(d)+".")
		return
	}
	g.checks <- struct{}{}
	_, err := g.auth.Authenticate(u.Username, current)
	<-g.checks
	if err != nil {
		if !errors.Is(err, auth.ErrBadCredentials) {
			g.throttle.pass(keys...)
			g.logf("gateway: change password: %v", err)
			fail(http.StatusInternalServerError, "Something went wrong. Try again.")
			return
		}
		g.throttle.fail(g.now(), keys...)
		g.activity.failed(EventPasswordFail, attempt, false)
		fail(http.StatusUnauthorized, "That isn't your current password.")
		return
	}
	g.throttle.reset("user:" + u.Username)
	g.throttle.pass("ip:" + ip)
	g.activity.settle(EventPasswordFail, attempt)

	// Counted first, since SetPassword leaves nothing to count. Only the feed
	// line depends on it, so a failure here doesn't stop the change.
	tokens, err := g.auth.CountAccessTokens(u.Username)
	if err != nil {
		g.logf("gateway: change password: %v", err)
	}
	g.checks <- struct{}{}
	err = g.auth.SetPassword(u.Username, next)
	<-g.checks
	if err != nil {
		g.logf("gateway: change password: %v", err)
		fail(http.StatusInternalServerError, "Something went wrong. Try again.")
		return
	}
	g.activity.record(EventPassword, "%s changed their password from %s, ending their other sessions", u.Username, ip)
	if tokens > 0 {
		g.activity.record(EventTokenRevoke, "%d access %s for %s revoked: password changed", tokens, plural(tokens, "token"), u.Username)
	}
	// SetPassword ended every session, since whoever knew the old password
	// may still be signed in somewhere; this browser starts a fresh one.
	tok, err := g.auth.CreateSessionFrom(u.Username, "", "", r.UserAgent(), ip)
	if err != nil {
		g.logf("gateway: change password: %v", err)
		g.clearCookie(w)
		http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
		return
	}
	g.setCookie(w, tok)
	http.Redirect(w, r, "/account?changed=1", http.StatusSeeOther)
}

// endSignIn signs out one device. Ending the one this page came from is
// signing out, and lands where signing out does. Otherwise the account page
// comes straight back, with the form's #where taking it to the list and a
// notice there naming the device, so it's plain which one went.
func (g *Gateway) endSignIn(w http.ResponseWriter, r *http.Request, u auth.User) {
	id := r.PostForm.Get("id")
	this := false
	if c, err := r.Cookie(g.cookieName); err == nil {
		this = auth.HashToken(c.Value) == id
	}
	// Named before it ends, while it can still be looked up.
	label := ""
	for _, v := range g.signIns(r, u) {
		if v.ID == id {
			label = v.Label()
		}
	}
	err := g.auth.EndSignIn(u.Username, id)
	if err != nil && !errors.Is(err, auth.ErrNoSuchSession) {
		g.logf("gateway: end sign-in: %v", err)
		g.message(w, r, http.StatusInternalServerError, "Not signed out", "Something went wrong. Try again.", "/account", "Back to account")
		return
	}
	if this {
		g.activity.record(EventSignOut, "%s signed out from %s", u.Username, g.clientIP(r))
		g.clearCookie(w)
		http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
		return
	}
	d := g.accountData(r, u)
	switch {
	case err != nil || label == "":
		// Already over, from another tab or by expiring, which is what was
		// asked for. An ID that isn't theirs reads the same, and changes
		// nothing.
		d.SignInNotice = "That device was already signed out."
	default:
		g.activity.record(EventSignOut, "%s signed out another device from %s", u.Username, g.clientIP(r))
		d.SignInNotice = label + ", is signed out, along with any app it opened."
	}
	g.render(w, r, http.StatusOK, "account", d)
}

// endOtherSignIns signs out every device but this one: the usual way back
// after losing one, without signing out here too.
func (g *Gateway) endOtherSignIns(w http.ResponseWriter, r *http.Request, u auth.User) {
	c, err := r.Cookie(g.cookieName)
	if err != nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	n, err := g.auth.EndOtherSignIns(u.Username, auth.HashToken(c.Value))
	if err != nil {
		g.logf("gateway: sign out other devices: %v", err)
		g.message(w, r, http.StatusInternalServerError, "Not signed out", "Something went wrong. Try again.", "/account", "Back to account")
		return
	}
	g.activity.record(EventSignOut, "%s signed out every other device from %s, ending %d %s", u.Username, g.clientIP(r), n, plural(n, "sign-in"))
	d := g.accountData(r, u)
	switch n {
	case 0:
		d.SignInNotice = "No other device was signed in."
	case 1:
		d.SignInNotice = "1 other device is signed out. You're still signed in here."
	default:
		d.SignInNotice = fmt.Sprintf("%d other devices are signed out. You're still signed in here.", n)
	}
	g.render(w, r, http.StatusOK, "account", d)
}

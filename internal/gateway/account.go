package gateway

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
)

// accountPage shows who is signed in, where, a password form, and ways to end
// one session or every session at once.
func (g *Gateway) accountPage(w http.ResponseWriter, r *http.Request) {
	tok, s, u, ok := g.session(r)
	if !ok || s.App != "" {
		http.Redirect(w, r, "/login?"+"next=%2Faccount", http.StatusFound)
		return
	}
	g.touch(tok, s)
	d := g.accountData(tok, u)
	switch {
	case r.URL.Query().Get("changed") == "1":
		d.Notice = "Password changed. Every other device has been signed out, and any access tokens revoked."
	case r.URL.Query().Get("ended") == "1":
		d.Notice = "That session has ended, along with the apps it was signed in to."
	}
	g.render(w, r, http.StatusOK, "account", d)
}

func (g *Gateway) accountData(tok string, u auth.User) pageData {
	return pageData{
		Title:       "Account",
		Who:         displayName(u),
		Username:    u.Username,
		Admin:       u.Admin,
		MinPassword: auth.MinPasswordLen,
		SignIns:     g.signIns(tok, u),
	}
}

// signInRow is one login as the account page draws it.
type signInRow struct {
	ID     string
	Device string
	Used   string // when it was last used, in words
	Apps   string // the apps it's signed in to, in words
	Here   bool   // the login this page was asked for from
}

// signIns lists u's logins, this one first. Failing to list them only leaves
// the section out: the rest of the page, and the way to end every session,
// don't depend on it.
func (g *Gateway) signIns(tok string, u auth.User) []signInRow {
	list, err := g.auth.SignIns(u.Username)
	if err != nil {
		g.logf("gateway: account: %v", err)
		return nil
	}
	here := auth.HashToken(tok)
	rows := make([]signInRow, 0, len(list))
	for _, in := range list {
		titles := make([]string, 0, len(in.Apps))
		for _, app := range in.Apps {
			titles = append(titles, g.titleOf(app))
		}
		row := signInRow{
			ID:     in.ID,
			Device: deviceOf(in.UserAgent),
			Used:   lastUsed(in.LastSeenAt, g.now()),
			Apps:   strings.Join(titles, ", "),
			Here:   in.ID == here,
		}
		if row.Here {
			rows = append([]signInRow{row}, rows...)
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

// lastUsed says when a login was last used. The time recorded moves only when
// a session slides, so it can be a day behind the truth, and the words don't
// claim more than that.
func lastUsed(seen, now time.Time) string {
	if now.Sub(seen) < 2*24*time.Hour {
		return "Used in the last two days"
	}
	return "Last used around " + seen.Format("Jan 2, 2006")
}

// deviceOf names a device well enough to recognise it from a browser's
// User-Agent. It's a label for the person who owns the session and nothing
// decides anything on it, so a browser that lies only misleads itself.
func deviceOf(ua string) string {
	first := func(pairs ...string) string {
		for i := 0; i+1 < len(pairs); i += 2 {
			if strings.Contains(ua, pairs[i]) {
				return pairs[i+1]
			}
		}
		return ""
	}
	// Order matters: an iPhone's agent also says "like Mac OS X", Android's
	// also says Linux, and every Chromium browser also says Chrome and Safari.
	system := first("iPhone", "iPhone", "iPad", "iPad", "Android", "Android",
		"Macintosh", "Mac", "Windows", "Windows", "CrOS", "ChromeOS", "Linux", "Linux")
	browser := first("Edg", "Edge", "OPR/", "Opera", "Firefox", "Firefox", "FxiOS", "Firefox",
		"Chrome/", "Chrome", "CriOS", "Chrome", "Safari", "Safari")
	switch {
	case browser != "" && system != "":
		return browser + " on " + system
	case system != "":
		return system
	case browser != "":
		return browser
	}
	return "Unknown device"
}

func (g *Gateway) accountSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	tok, s, u, ok := g.session(r)
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
		g.changePassword(w, r, tok, u)
	case "end":
		// A login that already ended, on another tab say, is what was wanted.
		ended, err := g.auth.EndSignIn(u.Username, r.PostForm.Get("session"))
		if err != nil {
			g.logf("gateway: end session: %v", err)
			g.message(w, r, http.StatusInternalServerError, "Session not ended", "Something went wrong. Try again.", "/account", "Back to account")
			return
		}
		if ended {
			g.activity.record(EventSignOut, "%s ended another session from %s", u.Username, g.clientIP(r))
		}
		http.Redirect(w, r, "/account?ended=1", http.StatusSeeOther)
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
func (g *Gateway) changePassword(w http.ResponseWriter, r *http.Request, tok string, u auth.User) {
	form := g.accountData(tok, u)
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
		g.activity.failed(EventPasswordFail, "from "+ip, true)
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
		g.activity.failed(EventPasswordFail, attempt, false)
		fail(http.StatusUnauthorized, "That isn't your current password.")
		return
	}
	g.throttle.reset(keys...)
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
	fresh, err := g.auth.CreateSession(u.Username, "", "", r.UserAgent())
	if err != nil {
		g.logf("gateway: change password: %v", err)
		g.clearCookie(w)
		http.Redirect(w, r, g.signedOutURL(), http.StatusSeeOther)
		return
	}
	g.setCookie(w, fresh)
	http.Redirect(w, r, "/account?changed=1", http.StatusSeeOther)
}

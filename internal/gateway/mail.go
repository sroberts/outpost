package gateway

import (
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/mail"
)

// Mail is the mail service: the Gmail account, set on the Mail page, and
// each app's addresses and whether it may send, set under its Manage menu.
// *mail.Service has it.
type Mail interface {
	Status() mail.Status
	PasswordSecret() string
	SetAccount(address, password, by string) error
	CheckNow()
	Routes(service string) []mail.Route
	AddAlias(service, note, by string) (mail.Route, error)
	RemoveAlias(service, route, by string) error
	CanSend(service string) bool
	SetSend(service string, allowed bool, by string) error
	Held(service string) (int, time.Time)
	Regenerate(service, by string) error
}

// EventMail is the account set, or an app's addresses, sending or token
// changed.
const EventMail = "mail"

// panelMail is the Mail panel under an app's menu.
const panelMail = "mail"

// mailView is the Mail page's state, for the template. The password is never
// in it: the page can set one, not show one.
type mailView struct {
	Address     string
	FromConfig  bool // the address is config.toml's, not set here
	HasPassword bool
	Secret      string // where the password is kept
	Health      string // a station mark: unset, starting, up or down
	State       string // how reading the inbox is going, in words
	Error       string // why the last check failed
	Notice      string
	FormError   string
}

// mailHealth is the account's state as a station mark and words.
func (g *Gateway) mailHealth(st mail.Status) (health, words string) {
	switch {
	case !st.Ready():
		return "unset", "Not set up"
	case st.Error != "":
		return "down", "Can't read the inbox"
	case st.Checked.IsZero():
		return "starting", "Checking the inbox"
	default:
		return "up", "Checked " + timeAgo(g.now(), st.Checked)
	}
}

// ago is how long before now t was, in a few words.
func timeAgo(now, t time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		n := int(d / time.Minute)
		return fmt.Sprintf("%d %s ago", n, noun(n, "minute", "minutes"))
	case d < 48*time.Hour:
		n := int(d / time.Hour)
		return fmt.Sprintf("%d %s ago", n, noun(n, "hour", "hours"))
	default:
		n := int(d / (24 * time.Hour))
		return fmt.Sprintf("%d days ago", n)
	}
}

// noun is one or many, as n calls for.
func noun(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (g *Gateway) mailPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.adminUser(w, r, "/mail", "Only an admin can set up mail.")
	if !ok {
		return
	}
	v := mailView{}
	switch r.URL.Query().Get("done") {
	case "saved":
		v.Notice = "Saved. Outpost is checking the inbox now."
	case "checked":
		v.Notice = "Checking the inbox now. Reload in a moment to see how it went."
	}
	g.renderMail(w, r, u, http.StatusOK, v)
}

func (g *Gateway) renderMail(w http.ResponseWriter, r *http.Request, u auth.User, status int, v mailView) {
	st := g.mail.Status()
	if v.Address == "" {
		v.Address = st.Address
	}
	v.FromConfig = st.Address != "" && !st.FromPortal
	v.HasPassword, v.Secret = st.HasPassword, g.mail.PasswordSecret()
	v.Health, v.State = g.mailHealth(st)
	v.Error = st.Error
	if st.Ready() && st.Error == "" && !st.LastRouted.IsZero() {
		v.State += fmt.Sprintf("; %d %s routed since Outpost started, the last %s", st.Routed,
			noun(st.Routed, "message", "messages"), timeAgo(g.now(), st.LastRouted))
	}
	g.render(w, r, status, "mail", pageData{
		Title: "Mail",
		Who:   displayName(u),
		Admin: u.Admin,
		Mail:  &v,
	})
}

func (g *Gateway) mailSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.adminUser(w, r, "/mail", "Only an admin can set up mail.")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/mail", "Back to Mail")
		return
	}
	switch r.PostForm.Get("action") {
	case "account":
		addr := r.PostForm.Get("address")
		if err := g.mail.SetAccount(addr, r.PostForm.Get("password"), u.Username); err != nil {
			g.renderMail(w, r, u, http.StatusBadRequest, mailView{Address: addr, FormError: sentence(err.Error())})
			return
		}
		g.activity.record(EventMail, "%s set the mail account from %s", u.Username, g.clientIP(r))
		http.Redirect(w, r, "/mail?done=saved", http.StatusSeeOther)
	case "check":
		g.mail.CheckNow()
		http.Redirect(w, r, "/mail?done=checked", http.StatusSeeOther)
	default:
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/mail", "Back to Mail")
	}
}

// appMail is an app's mail, for the panel under its menu.
type appMail struct {
	Ready bool // there's an account to get mail through
	// Routes are its addresses: its name, then the aliases made for it.
	Routes []mail.Route
	Send   bool
	Held   string // what's waiting for it, in words
	Words  string // the summary, in a few words
	Health string // the account's station mark
}

func (g *Gateway) appMail(app string, st mail.Status) *appMail {
	out := &appMail{Ready: st.Ready(), Routes: g.mail.Routes(app), Send: g.mail.CanSend(app)}
	out.Health, _ = g.mailHealth(st)
	if n, last := g.mail.Held(app); n == 0 {
		out.Held = "No mail yet."
	} else {
		out.Held = fmt.Sprintf("%d %s held, the newest %s.", n, noun(n, "message", "messages"), timeAgo(g.now(), last))
	}
	switch {
	case !out.Ready:
		out.Words = "not set up"
	case len(out.Routes) == 0:
		out.Words = "no addresses"
	default:
		out.Words = fmt.Sprintf("%d %s", len(out.Routes), noun(len(out.Routes), "address", "addresses"))
		if out.Send {
			out.Words += ", sends"
		}
	}
	return out
}

// mailSettingsNotice is the words for a mail outcome on an app's panel.
func mailSettingsNotice(done, route, title string) (string, bool) {
	switch done {
	case "mail-alias-added":
		return "Made the address +" + route + ". Mail to it reaches " + title + " from the next check.", true
	case "mail-alias-removed":
		return "Removed +" + route + ". Mail to it now stays in the inbox.", true
	case "mail-send-on":
		return title + " may send mail now, up to 100 messages a day.", true
	case "mail-send-off":
		return title + " can't send mail any more.", true
	case "mail-token":
		return "New token made. " + title + " restarts with it, and the old one no longer works.", true
	}
	return "", false
}

// mailSettingsSubmit takes the mail forms under an app's menu.
func (g *Gateway) mailSettingsSubmit(action, app string, f url.Values, by string) (done string, extra url.Values, err error) {
	switch action {
	case "mail-alias-add":
		rt, err := g.mail.AddAlias(app, f.Get("note"), by)
		if err != nil {
			return "", nil, err
		}
		return "mail-alias-added", url.Values{"route": {rt.Route}}, nil
	case "mail-alias-remove":
		route := f.Get("route")
		if err := g.mail.RemoveAlias(app, route, by); err != nil {
			return "", nil, err
		}
		return "mail-alias-removed", url.Values{"route": {route}}, nil
	case "mail-send":
		on := f.Get("send") != ""
		if err := g.mail.SetSend(app, on, by); err != nil {
			return "", nil, err
		}
		if on {
			return "mail-send-on", nil, nil
		}
		return "mail-send-off", nil, nil
	case "mail-token":
		if err := g.mail.Regenerate(app, by); err != nil {
			return "", nil, err
		}
		return "mail-token", nil, nil
	}
	return "", nil, fmt.Errorf("unknown mail action %q", action)
}

// validMailRoute is what a route in a redirect may be, so the notice can't be
// made to say anything else.
func validMailRoute(r string) bool {
	if r == "" || len(r) > 64 {
		return false
	}
	for _, c := range r {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}

// mailActivity is a mail change, for the activity feed.
func mailActivity(done, app, route string) string {
	switch done {
	case "mail-alias-added":
		return "gave " + app + " the mail address +" + route
	case "mail-alias-removed":
		return "took the mail address +" + route + " away from " + app
	case "mail-send-on":
		return "let " + app + " send mail"
	case "mail-send-off":
		return "stopped " + app + " sending mail"
	default:
		return "regenerated " + app + "'s mail token"
	}
}

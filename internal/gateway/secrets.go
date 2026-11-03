package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/secrets"
)

// maxSecretForm bounds the secrets page's form. A value may be MaxValue
// bytes, which form encoding can triple.
const maxSecretForm = 3*secrets.MaxValue + 16<<10

// Secrets is what the gateway needs to manage secrets. The daemon hands it
// the same store the supervisor reads, which restarts every service a change
// reaches on its next tick.
type Secrets interface {
	List() []secrets.Secret
	Get(name string) (secrets.Secret, bool)
	Create(name, env, value string) (secrets.Secret, error)
	Rotate(name, value string) (secrets.Secret, error)
	Delete(name string) (secrets.Secret, error)
	Bind(name, service string) (secrets.Secret, error)
	Unbind(name, service string) (secrets.Secret, error)
	Reaches(name string, services []string) []string
}

// secretsView is the secrets page's state, for the template. No field ever
// holds a value: the page can set one, never show one.
type secretsView struct {
	Rows   []secretRow
	Apps   []secretApp // every declared app, for the add form
	Notice string
	Error  string
	// ErrorFor is the secret a failed form was about, so its row shows the
	// error; "" is the add form.
	ErrorFor string
	// What the add form had, less the value, when it's shown again.
	Name, Env string
	Global    bool
}

type secretRow struct {
	Name, Env string
	Global    bool
	Bound     string // the apps it's bound to by name, in words
	Reaches   string // the apps that get it now, in words
	ReachN    int    // how many, for the verb's agreement
	Changed   string
	Apps      []secretApp // checked where bound by name
	Error     string
}

type secretApp struct {
	Name, Title string
	Checked     bool
}

// secretsUser is the admin behind a request to the secrets page.
func (g *Gateway) secretsUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	return g.adminUser(w, r, "/secrets", "Only an admin can manage secrets.")
}

// secretApps lists the declared services, by title. A failure costs the
// checkboxes, not the page.
func (g *Gateway) secretApps(ctx context.Context) []secretApp {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	sts, err := g.services.List(ctx)
	if err != nil {
		g.logf("gateway: list services for secrets: %v", err)
		return nil
	}
	apps := make([]secretApp, 0, len(sts))
	for _, st := range sts {
		title := st.Title
		if title == "" {
			title = titleCase(st.Name)
		}
		apps = append(apps, secretApp{Name: st.Name, Title: title})
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Title < apps[j].Title })
	return apps
}

func appNames(apps []secretApp) []string {
	out := make([]string, len(apps))
	for i, a := range apps {
		out[i] = a.Name
	}
	return out
}

func (g *Gateway) secretsPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.secretsUser(w, r)
	if !ok {
		return
	}
	v := secretsView{Notice: g.secretNotice(r)}
	g.renderSecrets(w, r, u, http.StatusOK, v)
}

// renderSecrets fills in the rows and draws the page.
func (g *Gateway) renderSecrets(w http.ResponseWriter, r *http.Request, u auth.User, status int, v secretsView) {
	apps := g.secretApps(r.Context())
	names := appNames(apps)
	titles := map[string]string{}
	for _, a := range apps {
		titles[a.Name] = a.Title
	}
	words := func(svcs []string) string {
		out := make([]string, len(svcs))
		for i, s := range svcs {
			if t, ok := titles[s]; ok {
				out[i] = t
			} else {
				out[i] = s + " (not declared)"
			}
		}
		return listWords(out)
	}
	now := g.now()
	for _, sec := range g.secrets.List() {
		reach := g.secrets.Reaches(sec.Name, names)
		row := secretRow{
			Name: sec.Name, Env: sec.Env, Global: sec.Global,
			Bound:   words(sec.Services),
			Reaches: words(reach),
			ReachN:  len(reach),
			Changed: signedInAt(now, sec.UpdatedAt),
		}
		bound := map[string]bool{}
		for _, s := range sec.Services {
			bound[s] = true
		}
		for _, a := range apps {
			row.Apps = append(row.Apps, secretApp{Name: a.Name, Title: a.Title, Checked: bound[a.Name]})
		}
		if v.ErrorFor == sec.Name {
			row.Error = v.Error
		}
		v.Rows = append(v.Rows, row)
	}
	if v.ErrorFor != "" {
		v.Error = "" // shown on its row instead
	}
	v.Apps = apps
	g.render(w, r, status, "secrets", pageData{
		Title:   "Secrets",
		Who:     displayName(u),
		Admin:   u.Admin,
		Secrets: &v,
	})
}

// secretNotice reads the confirmation a change redirects with. Only the
// secret's name and a fixed verb come from the URL; who gets it is worked out
// now, from the store.
func (g *Gateway) secretNotice(r *http.Request) string {
	q := r.URL.Query()
	name := q.Get("secret")
	if secrets.ValidName(name) != nil {
		return ""
	}
	apps := g.secretApps(r.Context())
	var titles []string
	reach := g.secrets.Reaches(name, appNames(apps))
	for _, a := range apps {
		for _, n := range reach {
			if a.Name == n {
				titles = append(titles, a.Title)
			}
		}
	}
	restarts := ""
	switch len(titles) {
	case 0:
	case 1:
		restarts = " " + titles[0] + " restarts with it in a few seconds."
	default:
		restarts = " " + listWords(titles) + " restart with it in a few seconds."
	}
	switch q.Get("done") {
	case "created":
		if restarts == "" {
			return "Added " + name + ". No app gets it yet."
		}
		return "Added " + name + "." + restarts
	case "rotated":
		return "Rotated " + name + "." + restarts
	case "apps":
		return "Saved which apps get " + name + "." + strings.Replace(restarts, "with it", "to pick that up", 1)
	case "removed":
		return "Removed " + name + ". Any app that had it restarts without it in a few seconds."
	}
	return ""
}

// listWords joins names as a sentence does: "a", "a and b", "a, b and c".
func listWords(s []string) string {
	switch len(s) {
	case 0:
		return ""
	case 1:
		return s[0]
	}
	return strings.Join(s[:len(s)-1], ", ") + " and " + s[len(s)-1]
}

func (g *Gateway) secretsSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.secretsUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSecretForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/secrets", "Back to secrets")
		return
	}
	f := r.PostForm
	name := strings.TrimSpace(f.Get("name"))
	fail := func(about string, err error) {
		v := secretsView{Error: secretError(err), ErrorFor: about}
		if about == "" {
			v.Name, v.Env, v.Global = name, strings.TrimSpace(f.Get("env")), f.Get("global") != ""
		}
		g.renderSecrets(w, r, u, http.StatusBadRequest, v)
	}
	done := func(verb, what string) {
		g.activity.record(EventSecret, "%s %s secret %s from %s", u.Username, what, name, g.clientIP(r))
		g.logf("gateway: %s %s secret %s", u.Username, what, name)
		http.Redirect(w, r, "/secrets?done="+verb+"&secret="+url.QueryEscape(name)+"#s-"+url.QueryEscape(name), http.StatusSeeOther)
	}

	switch f.Get("action") {
	case "create":
		env := strings.TrimSpace(f.Get("env"))
		if env == "" {
			env = secrets.DefaultEnv(name)
		}
		if _, err := g.secrets.Create(name, env, f.Get("value")); err != nil {
			fail("", err)
			return
		}
		// Bindings after the create; one refused leaves the secret made, and
		// says which, rather than undoing the part that worked.
		if err := g.bindAll(name, f.Get("global") != "", f["app"]); err != nil {
			fail(name, err)
			return
		}
		done("created", "added")
	case "rotate":
		if _, ok := g.secrets.Get(name); !ok {
			fail("", secrets.ErrNotFound)
			return
		}
		if _, err := g.secrets.Rotate(name, f.Get("value")); err != nil {
			fail(name, err)
			return
		}
		done("rotated", "rotated")
	case "apps":
		sec, ok := g.secrets.Get(name)
		if !ok {
			fail("", secrets.ErrNotFound)
			return
		}
		if err := g.setApps(sec, f.Get("global") != "", f["app"]); err != nil {
			fail(name, err)
			return
		}
		done("apps", "changed the apps for")
	case "remove":
		if _, err := g.secrets.Delete(name); err != nil {
			fail("", err)
			return
		}
		done("removed", "removed")
	default:
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/secrets", "Back to secrets")
	}
}

// bindAll binds a new secret as the add form asked.
func (g *Gateway) bindAll(name string, global bool, apps []string) error {
	if global {
		if _, err := g.secrets.Bind(name, ""); err != nil {
			return err
		}
	}
	for _, app := range apps {
		if _, err := g.secrets.Bind(name, app); err != nil {
			return err
		}
	}
	return nil
}

// setApps makes a secret's bindings match the form: binding first, so a
// refused binding leaves the old ones in place rather than none.
func (g *Gateway) setApps(sec secrets.Secret, global bool, apps []string) error {
	want := map[string]bool{}
	for _, a := range apps {
		want[a] = true
	}
	if global && !sec.Global {
		if _, err := g.secrets.Bind(sec.Name, ""); err != nil {
			return err
		}
	}
	for a := range want {
		if _, err := g.secrets.Bind(sec.Name, a); err != nil {
			return err
		}
	}
	if !global && sec.Global {
		if _, err := g.secrets.Unbind(sec.Name, ""); err != nil {
			return err
		}
	}
	for _, a := range sec.Services {
		if !want[a] {
			if _, err := g.secrets.Unbind(sec.Name, a); err != nil {
				return err
			}
		}
	}
	return nil
}

// secretError words a refusal for the page. The store's own messages are
// already about what was typed, never about a value.
func secretError(err error) string {
	switch {
	case errors.Is(err, secrets.ErrNotFound):
		return "That secret isn't there any more. Someone may have removed it."
	case errors.Is(err, secrets.ErrExists):
		return "There's already a secret with that name. Rotate it below instead."
	}
	msg := err.Error()
	return strings.ToUpper(msg[:1]) + msg[1:] + "."
}

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/secrets"
)

// What belongs to one app is set under that app's Manage menu, beside its
// start and stop: what the internet reaches of it, the secrets it gets, and
// the AI it may use. The Secrets and AI pages keep what isn't any one app's:
// secrets given to every app, and the models and their providers.

// maxSettingsForm bounds a settings form. A secret's value may be MaxValue
// bytes, which form encoding can triple.
const maxSettingsForm = 3*secrets.MaxValue + 16<<10

// The panels under an app's menu, for the template and the redirect back.
const (
	panelPublic  = "public"
	panelSecrets = "secrets"
	panelAI      = "ai"
)

// AIApps is the cheap read an app's menu needs from the AI service: what's
// chosen and what each app has asked for, without asking any provider.
// *ai.Service has it; without it an app's menu has no AI section.
type AIApps interface {
	Choice(kind string) ai.Choice
	Usage() []ai.Usage
}

// appSettings is what an admin can set for one app.
type appSettings struct {
	Name, Title string
	Public      *publicRow  // nil without a way to change it
	Secrets     *appSecrets // nil without a secrets store
	AI          *appAI      // nil without AI
	// Open is the panel a change just came back to, with Notice or Error,
	// so the menu opens on it.
	Open          string
	Notice, Error string
}

// appSecrets are the variables an app gets from secrets.
type appSecrets struct {
	// Own are given to it by name, which the app's menu can replace and
	// take away; Shared are given to every app, set on the Secrets page.
	Own    []appSecret
	Shared []appSecret
	Count  int // variables it gets
	// What the add form had, less the value, when it's shown again.
	Name, Env string
}

type appSecret struct {
	Name, Env string
	// Others are the other apps given it by name, in words: replacing it
	// reaches them too.
	Others string
	// Gets is whether the app gets this one now. A shared one doesn't when
	// the app has its own for the same variable.
	Gets bool
}

// appAI is what an app may use of the AI, and what it has.
type appAI struct {
	Kinds []appAIKind
	// Hosted says which models an app may use can be chosen here.
	Hosted    bool
	Words     string   // what it may use, in a few words
	Used      []string // what it has asked for, per kind, in words
	LastError string
}

type appAIKind struct {
	Kind, Name, Model string
	Allowed           bool
}

// settingsFlash is a settings form's outcome, shown on the app it was for.
type settingsFlash struct {
	App, Panel    string
	Notice, Error string
	// What a refused form had, shown again.
	Paths      string
	Funnel     bool
	Name, Env  string
	typedPaths bool
}

// settle gives each station an admin's settings, reading the secrets and AI
// once for them all.
func (g *Gateway) settle(ctx context.Context, sts []station, fl *settingsFlash) {
	_, canAI := g.ai.(AIApps)
	if g.exposure == nil && g.secrets == nil && !canAI {
		return
	}
	var apps []secretApp
	if g.secrets != nil {
		apps = g.secretApps(ctx)
	}
	var usage map[string][]ai.Usage
	var choices []ai.Choice
	if a, ok := g.ai.(AIApps); ok {
		usage = map[string][]ai.Usage{}
		for _, u := range a.Usage() {
			usage[u.Service] = append(usage[u.Service], u)
		}
		for _, k := range ai.Kinds {
			choices = append(choices, a.Choice(k))
		}
	}
	for i := range sts {
		st := &sts[i]
		rt, ok := g.routes.get(st.Name)
		if !ok {
			continue
		}
		set := &appSettings{Name: st.Name, Title: st.Title}
		if g.exposure != nil {
			row := publicRowFor(rt)
			set.Public = &row
		}
		if g.secrets != nil {
			set.Secrets = g.appSecrets(rt.Name, apps)
		}
		if canAI {
			set.AI = g.appAI(rt.Name, choices, usage[rt.Name])
		}
		if fl != nil && fl.App == st.Name {
			set.Open, set.Notice, set.Error = fl.Panel, fl.Notice, fl.Error
			if set.Public != nil && fl.typedPaths {
				set.Public.Paths, set.Public.Funnel = fl.Paths, fl.Funnel
			}
			if set.Secrets != nil {
				set.Secrets.Name, set.Secrets.Env = fl.Name, fl.Env
			}
		}
		st.Settings = set
	}
}

// appSecrets lists what app gets from secrets: its own, then those given
// to every app.
func (g *Gateway) appSecrets(app string, apps []secretApp) *appSecrets {
	titles := map[string]string{}
	for _, a := range apps {
		titles[a.Name] = a.Title
	}
	names := appNames(apps)
	if !slices.Contains(names, app) {
		names = append(names, app)
	}
	out := &appSecrets{}
	for _, sec := range g.secrets.List() {
		gets := slices.Contains(g.secrets.Reaches(sec.Name, names), app)
		if slices.Contains(sec.Services, app) {
			var others []string
			for _, s := range sec.Services {
				if s != app {
					others = append(others, firstNonEmpty(titles[s], s))
				}
			}
			out.Own = append(out.Own, appSecret{Name: sec.Name, Env: sec.Env, Others: listWords(others), Gets: gets})
		} else if sec.Global {
			out.Shared = append(out.Shared, appSecret{Name: sec.Name, Env: sec.Env, Gets: gets})
		}
		if gets {
			out.Count++
		}
	}
	return out
}

// appAI is what app may use of each model, and has.
func (g *Gateway) appAI(app string, choices []ai.Choice, used []ai.Usage) *appAI {
	_, hosted := g.ai.(AIControls)
	out := &appAI{Hosted: hosted}
	var allowed []string
	for _, c := range choices {
		k := appAIKind{Kind: c.Kind, Name: ai.KindName(c.Kind), Model: c.Model,
			Allowed: !hosted || c.Every || slices.Contains(c.Apps, app)}
		if k.Allowed && k.Model != "" {
			allowed = append(allowed, k.Name)
		}
		out.Kinds = append(out.Kinds, k)
	}
	switch {
	case len(allowed) == 0:
		out.Words = "no models"
	default:
		out.Words = andList(allowed)
	}
	var lastAt time.Time
	for _, u := range used {
		out.Used = append(out.Used, usageWords(u))
		if u.LastError != "" && (out.LastError == "" || u.LastErrorAt.After(lastAt)) {
			out.LastError, lastAt = u.LastError, u.LastErrorAt
		}
	}
	return out
}

// settingsNotice reads the confirmation a settings change redirects with.
// Only the app, the panel and a fixed outcome come from the URL; the words
// are made here, about what's in force now.
func (g *Gateway) settingsNotice(r *http.Request) *settingsFlash {
	q := r.URL.Query()
	rt, ok := g.routes.get(q.Get("app"))
	if !ok {
		return nil
	}
	title := firstNonEmpty(rt.Title, titleCase(rt.Name))
	fl := &settingsFlash{App: rt.Name}
	switch q.Get("done") {
	case "public-saved":
		fl.Panel, fl.Notice = panelPublic, "Saved. It takes effect in a few seconds; the app itself doesn't restart."
	case "public-reset":
		fl.Panel, fl.Notice = panelPublic, "Back to what the manifest says."
	case "secret-added", "secret-replaced", "secret-removed":
		name := q.Get("secret")
		if secrets.ValidName(name) != nil {
			return nil
		}
		fl.Panel = panelSecrets
		switch q.Get("done") {
		case "secret-added":
			fl.Notice = "Added " + name + ". " + title + " restarts with it in a few seconds."
		case "secret-replaced":
			fl.Notice = "Replaced " + name + ". Every app that gets it restarts with the new value in a few seconds."
		default:
			fl.Notice = "Took " + name + " away from " + title + ". It restarts without it in a few seconds."
		}
	case "ai-saved":
		fl.Panel, fl.Notice = panelAI, "Saved. It takes effect from "+title+"'s next request."
	case "ai-token":
		fl.Panel, fl.Notice = panelAI, "New token made. "+title+" restarts with it, and the old one no longer works."
	default:
		return nil
	}
	return fl
}

// settingsSubmit takes a form from an app's settings, then goes back to the
// catalog, open on that app's panel; a refusal is shown there in place.
func (g *Gateway) settingsSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.adminUser(w, r, "/", "Only an admin can change an app's settings.")
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxSettingsForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", g.portalLink(), "Back to apps")
		return
	}
	f := r.PostForm
	rt, ok := g.routes.get(f.Get("app"))
	if !ok {
		g.message(w, r, http.StatusBadRequest, "That app isn't there", "It may have been removed.", g.portalLink(), "Back to apps")
		return
	}
	app := rt.Name
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	refuse := func(panel string, err error, fl settingsFlash) {
		fl.App, fl.Panel, fl.Error = app, panel, sentence(err.Error())
		g.renderHome(w, r, u, http.StatusBadRequest, &fl)
	}
	back := func(done string, extra url.Values) {
		q := url.Values{"app": {app}, "done": {done}}
		for k, v := range extra {
			q[k] = v
		}
		// So the page it lands on shows what was just set.
		g.refreshRoutes(r.Context())
		http.Redirect(w, r, "/?"+q.Encode()+"#app-"+url.QueryEscape(app), http.StatusSeeOther)
	}
	record := func(kind, format string, v ...any) {
		what := fmt.Sprintf(format, v...)
		g.activity.record(kind, "%s %s from %s", u.Username, what, g.clientIP(r))
		g.logf("gateway: %s %s", u.Username, what)
	}
	unknown := func() {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", g.portalLink(), "Back to apps")
	}

	switch action := f.Get("action"); action {
	case "public-save", "public-reset":
		if g.exposure == nil {
			unknown()
			return
		}
		if action == "public-reset" {
			if err := g.exposure.Reset(ctx, app, u.Username); err != nil {
				refuse(panelPublic, err, settingsFlash{})
				return
			}
			record(EventExposure, "went back to the manifest's public paths and Funnel for %s", app)
			back("public-reset", nil)
			return
		}
		funnel, paths := f.Get("funnel") != "", splitPaths(f.Get("paths"))
		if err := g.exposure.Set(ctx, app, funnel, paths, u.Username); err != nil {
			refuse(panelPublic, err, settingsFlash{Paths: f.Get("paths"), Funnel: funnel, typedPaths: true})
			return
		}
		record(EventExposure, "%s for %s", describeExposure(funnel, paths), app)
		back("public-saved", nil)

	case "secret-add", "secret-replace", "secret-remove":
		if g.secrets == nil {
			unknown()
			return
		}
		name := strings.TrimSpace(f.Get("name"))
		fail := func(err error, fl settingsFlash) {
			fl.App, fl.Panel, fl.Error = app, panelSecrets, secretError(err)
			g.renderHome(w, r, u, http.StatusBadRequest, &fl)
		}
		switch action {
		case "secret-add":
			env := strings.TrimSpace(f.Get("env"))
			typed := settingsFlash{Name: name, Env: env}
			if env == "" {
				env = secrets.DefaultEnv(name)
			}
			if _, err := g.secrets.Create(name, env, f.Get("value")); err != nil {
				fail(err, typed)
				return
			}
			if _, err := g.secrets.Bind(name, app); err != nil {
				fail(err, settingsFlash{})
				return
			}
			record(EventSecret, "added secret %s for %s", name, app)
			back("secret-added", url.Values{"secret": {name}})
		case "secret-replace":
			if _, err := g.secrets.Rotate(name, f.Get("value")); err != nil {
				fail(err, settingsFlash{})
				return
			}
			record(EventSecret, "replaced secret %s from %s's settings", name, app)
			back("secret-replaced", url.Values{"secret": {name}})
		default:
			// Taken from this app only; a secret no app is left with goes, so
			// nothing is kept that no page shows.
			sec, ok := g.secrets.Get(name)
			if !ok {
				fail(secrets.ErrNotFound, settingsFlash{})
				return
			}
			var err error
			if len(sec.Services) == 1 && sec.Services[0] == app && !sec.Global {
				_, err = g.secrets.Delete(name)
			} else {
				_, err = g.secrets.Unbind(name, app)
			}
			if err != nil {
				fail(err, settingsFlash{})
				return
			}
			record(EventSecret, "took secret %s away from %s", name, app)
			back("secret-removed", url.Values{"secret": {name}})
		}

	case "ai-access":
		controls, hosted := g.ai.(AIControls)
		reader, ok := g.ai.(AIApps)
		if !hosted || !ok {
			unknown()
			return
		}
		want := f["kind"]
		for _, k := range ai.Kinds {
			c := reader.Choice(k)
			has := c.Every || slices.Contains(c.Apps, app)
			if has == slices.Contains(want, k) {
				continue
			}
			every, apps := false, slices.Clone(c.Apps)
			switch {
			case has && c.Every:
				// Every app but this one: the rest keep what they had.
				apps = nil
				for _, other := range g.routes.all() {
					if other.Name != app {
						apps = append(apps, other.Name)
					}
				}
			case has:
				apps = slices.DeleteFunc(apps, func(a string) bool { return a == app })
			default:
				apps = append(apps, app)
			}
			if err := controls.SetApps(k, every, apps, u.Username); err != nil {
				refuse(panelAI, err, settingsFlash{})
				return
			}
			if has {
				record(EventAI, "took the %s away from %s", ai.KindName(k), app)
			} else {
				record(EventAI, "gave the %s to %s", ai.KindName(k), app)
			}
		}
		back("ai-saved", nil)

	case "ai-token":
		if g.ai == nil {
			unknown()
			return
		}
		if err := g.ai.Regenerate(app, u.Username); err != nil {
			refuse(panelAI, err, settingsFlash{})
			return
		}
		record(EventAI, "regenerated %s's AI token", app)
		back("ai-token", nil)

	default:
		unknown()
	}
}

// sentence is an error as the page shows it: starting with a capital.
func sentence(msg string) string {
	if msg == "" {
		return msg
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

// renderHome is the catalog for u, with fl's outcome shown on its app.
func (g *Gateway) renderHome(w http.ResponseWriter, r *http.Request, u auth.User, status int, fl *settingsFlash) {
	sts := g.stations(u)
	d := pageData{
		Title:    "Apps",
		Who:      displayName(u),
		Admin:    u.Admin,
		Stations: sts,
		Counts:   tally(sts),
		Search:   len(sts) >= searchFrom,
	}
	if u.Admin && g.updater != nil {
		g.checkIfStale()
		d.Waiting = g.waiting()
		d.UpdatesLink = true
	}
	d.AddLink = u.Admin && g.adder != nil
	d.SecretsLink = u.Admin && g.secrets != nil
	d.AILink = u.Admin && g.ai != nil
	if u.Admin && g.control != nil {
		d.Poll = g.manage(d.Stations)
	}
	if u.Admin {
		g.settle(r.Context(), d.Stations, fl)
	}
	// A crashed app comes back on its own, with nobody's action to refresh the
	// row, and whoever is looking wants to see it happen.
	d.Poll = d.Poll || slices.ContainsFunc(d.Stations, func(s station) bool { return s.State == stateStarting })
	d.Icons = slices.ContainsFunc(d.Stations, func(s station) bool {
		return s.Usage != nil || s.Quick != nil || len(s.Acts) > 0
	})
	g.render(w, r, status, "portal", d)
}

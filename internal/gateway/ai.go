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
)

// maxAIForm bounds the AI models page's forms: a kind and a model name, the
// apps, or an API key.
const maxAIForm = 64 << 10

// aiTestTimeout bounds the page's Test button. The first request to a model
// Ollama hasn't loaded waits for it to load.
const aiTestTimeout = 2 * time.Minute

// AIModels is what the gateway needs to show and choose the models apps are
// given. *ai.Service is one.
type AIModels interface {
	Overview(ctx context.Context) ai.Overview
	Set(ctx context.Context, kind, model, by string) (string, error)
	Reset(kind, by string) error
	Test(ctx context.Context, kind string) (ai.TestResult, error)
	Regenerate(service, by string) error
}

// AIControls is what the page needs beyond AIModels to use hosted providers
// and choose which apps may use each model. *ai.Service has it; without it
// the page offers Ollama's models alone, to every app.
type AIControls interface {
	SetModel(ctx context.Context, kind, provider, model, by string) (string, error)
	SetApps(kind string, every bool, apps []string, by string) error
	ResetApps(kind, by string) error
	SetKey(provider, value, by string) error
}

// aiView is the AI models page's state, for the template.
type aiView struct {
	URL, OllamaURL string
	OllamaError    string
	Kinds          []aiKindRow
	Apps           []aiAppRow
	// Hosted says the hosted providers and app access are offered.
	Hosted    bool
	Providers []aiProviderRow
	// KeysSet counts the providers with a key. The Providers and Apps
	// sections are folded unless something in them wants attention: a
	// chosen model with no key, or a notice or error from a form there.
	KeysSet                 int
	ProvidersOpen, AppsOpen bool
}

// aiProviderRow is a hosted provider and its key, for the Providers section.
type aiProviderRow struct {
	Name, Title string
	Serves      string // the kinds, in words
	KeySecret   string
	HasKey      bool
	BaseURL     string
	Notice      string
	Error       string
	// Waiting names the chosen models this provider's missing key stops.
	Waiting string
}

// aiHosted is one hosted provider's models for one kind, offered as a list to
// type or pick from: OpenRouter alone has hundreds.
type aiHosted struct {
	Provider, Title string
	NeedsKey        bool
	Options         []aiOption
	// Complete says a model not in Options is refused.
	Complete bool
	Error    string
	Current  string // the model, when it's this provider's
}

// aiAppChoice is one app in a kind's Apps form.
type aiAppChoice struct {
	Name, Title string
	Checked     bool
}

type aiKindRow struct {
	Kind, Name string
	Model      string
	// Source says where Model came from, in words.
	Source  string
	Missing bool
	// FromPortal offers going back to config.toml's.
	FromPortal bool
	Options    []aiOption
	Notice     string
	Error      string
	// Test is a Test button's result, shown once.
	Test string
	// Health is its station's state: up, down (apps asking get an error,
	// a missing model or key) or unset; HealthWords says it for a reader.
	Health, HealthWords string

	// With hosted providers: who serves it, what reaches it, and who may.
	Provider, ProviderTitle string
	APIsNote                string
	NoKey                   bool
	Hosted                  []aiHosted
	Every                   bool
	AppsWords               string
	AppsFromPortal          bool
	AppChoices              []aiAppChoice
}

type aiOption struct {
	Name, Detail string
	Checked      bool
}

type aiAppRow struct {
	Name, Title string
	Used        []string // per kind, in words
	LastError   string
	Notice      string
}

func (g *Gateway) aiUser(w http.ResponseWriter, r *http.Request) (auth.User, bool) {
	return g.adminUser(w, r, "/ai", "Only an admin can choose the models apps are given.")
}

func (g *Gateway) aiPage(w http.ResponseWriter, r *http.Request) {
	u, ok := g.aiUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	notice := map[string]string{}
	switch q.Get("done") {
	case "saved":
		if k := q.Get("kind"); ai.ValidKind(k) {
			notice[k] = "Saved. Apps get it from their next request; none of them restarts."
		}
	case "reset":
		if k := q.Get("kind"); ai.ValidKind(k) {
			notice[k] = "Back to what config.toml says."
		}
	case "regenerated":
		if app := q.Get("app"); app != "" {
			notice["app:"+app] = "New token made. The app restarts with it, and the old one no longer works."
		}
	case "apps":
		if k := q.Get("kind"); ai.ValidKind(k) {
			notice[k] = "Saved who may use it. It takes effect from the next request."
		}
	case "appsreset":
		if k := q.Get("kind"); ai.ValidKind(k) {
			notice[k] = "Back to the apps config.toml gives it."
		}
	case "key":
		if p := q.Get("provider"); ai.ValidProvider(p) {
			notice["provider:"+p] = "Key saved. Its models are listed now, and apps' requests use it from the next one."
		}
	}
	g.renderAI(w, r, u, http.StatusOK, notice, nil, nil)
}

// renderAI draws the page. errs and tests are by kind; notice is by kind, or
// "app:<name>" for an app.
func (g *Gateway) renderAI(w http.ResponseWriter, r *http.Request, u auth.User, status int, notice, errs, tests map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	o := g.ai.Overview(ctx)
	_, hosted := g.ai.(AIControls)
	v := aiView{URL: o.URL, OllamaURL: o.OllamaURL, OllamaError: o.ModelsError, Hosted: hosted}
	routes := g.routes.all()
	for _, c := range o.Choices {
		provider := c.Provider
		if provider == "" {
			provider = ai.ProviderOllama
		}
		row := aiKindRow{
			Kind: c.Kind, Name: ai.KindName(c.Kind), Model: c.Model, Missing: c.Missing,
			FromPortal: c.FromPortal, Notice: notice[c.Kind], Error: errs[c.Kind], Test: tests[c.Kind],
			Provider: provider, ProviderTitle: ai.ProviderTitle(provider), NoKey: c.NoKey,
		}
		row.Source = choiceWords(c)
		switch {
		case row.Model == "":
			row.Health, row.HealthWords = "unset", "none chosen"
		case row.Missing || row.NoKey:
			row.Health, row.HealthWords = "down", "not working"
		default:
			row.Health, row.HealthWords = "up", "chosen"
		}
		for _, m := range o.Models {
			if m.Serves(c.Kind) {
				row.Options = append(row.Options, aiOption{Name: m.Name, Detail: modelDetail(m), Checked: provider == ai.ProviderOllama && m.Name == c.Model})
			}
		}
		if hosted {
			fillHosted(&row, c, o, routes)
		}
		v.Kinds = append(v.Kinds, row)
	}
	if hosted {
		// The kinds a provider's missing key is holding up, so its row can
		// say so: the link from a kind's error lands there.
		waiting := map[string][]string{}
		for _, k := range v.Kinds {
			if k.NoKey {
				waiting[k.Provider] = append(waiting[k.Provider], k.Name)
				v.ProvidersOpen = true
			}
		}
		for _, p := range o.Providers {
			if !p.Keyed {
				continue
			}
			var kinds []string
			for _, k := range p.Kinds {
				kinds = append(kinds, ai.KindName(k))
			}
			row := aiProviderRow{
				Name: p.Name, Title: p.Title, Serves: strings.Join(kinds, " and "), KeySecret: p.KeySecret,
				HasKey: p.HasKey, BaseURL: p.BaseURL, Notice: notice["provider:"+p.Name], Error: errs["provider:"+p.Name],
			}
			if row.HasKey {
				v.KeysSet++
			}
			if row.Notice != "" || row.Error != "" {
				v.ProvidersOpen = true
			}
			if names := waiting[p.Name]; len(names) > 0 {
				verb := "is"
				if len(names) > 1 {
					verb = "are"
				}
				row.Waiting = fmt.Sprintf("The %s %s waiting for this key.", andList(names), verb)
			}
			v.Providers = append(v.Providers, row)
		}
	}
	used := map[string][]ai.Usage{}
	for _, us := range o.Usage {
		used[us.Service] = append(used[us.Service], us)
	}
	for _, rt := range routes {
		row := aiAppRow{Name: rt.Name, Title: rt.Title, Notice: notice["app:"+rt.Name]}
		if row.Notice != "" {
			v.AppsOpen = true
		}
		if row.Title == "" {
			row.Title = titleCase(rt.Name)
		}
		var lastErrAt time.Time
		for _, us := range used[rt.Name] {
			row.Used = append(row.Used, usageWords(us))
			if us.LastError != "" && us.LastErrorAt.After(lastErrAt) {
				row.LastError, lastErrAt = us.LastError, us.LastErrorAt
			}
		}
		v.Apps = append(v.Apps, row)
	}
	g.render(w, r, status, "ai", pageData{
		Title: "AI models",
		Who:   displayName(u),
		Admin: u.Admin,
		AI:    &v,
	})
}

// fillHosted adds what the hosted providers offer for a kind, and who may
// use it, to its row.
func fillHosted(row *aiKindRow, c ai.Choice, o ai.Overview, routes []route) {
	if row.Kind == ai.LLM && row.Model != "" && len(c.APIs) == 1 {
		other := "/anthropic/v1/messages"
		if c.APIs[0] == other {
			other = "/openai/v1/chat/completions"
		}
		row.APIsNote = fmt.Sprintf("%s doesn't take %s, so apps calling it get an error saying to use %s.", row.ProviderTitle, other, c.APIs[0])
	}
	keyed := map[string]bool{}
	for _, p := range o.Providers {
		keyed[p.Name] = p.Keyed && !p.HasKey
	}
	for _, cat := range o.Catalogs {
		if cat.Kind != row.Kind || cat.Provider == ai.ProviderOllama {
			continue
		}
		h := aiHosted{Provider: cat.Provider, Title: ai.ProviderTitle(cat.Provider), NeedsKey: keyed[cat.Provider],
			Complete: cat.Complete, Error: cat.Error}
		if h.NeedsKey {
			h.Error = ""
		}
		if row.Provider == cat.Provider {
			h.Current = row.Model
		}
		for _, m := range cat.Models {
			h.Options = append(h.Options, aiOption{Name: m.Name, Detail: m.Note})
		}
		row.Hosted = append(row.Hosted, h)
	}
	row.Every, row.AppsFromPortal = c.Every, c.AppsFromPortal
	titles := map[string]string{}
	for _, rt := range routes {
		t := rt.Title
		if t == "" {
			t = titleCase(rt.Name)
		}
		titles[rt.Name] = t
		row.AppChoices = append(row.AppChoices, aiAppChoice{Name: rt.Name, Title: t, Checked: slices.Contains(c.Apps, rt.Name)})
	}
	switch {
	case c.Every:
		row.AppsWords = "Every app may use it"
	case len(c.Apps) == 0:
		row.AppsWords = "No app may use it"
	default:
		names := make([]string, len(c.Apps))
		for i, a := range c.Apps {
			names[i] = firstNonEmpty(titles[a], a)
		}
		row.AppsWords = "Only " + andList(names) + " may use it"
	}
	if c.AppsFromPortal {
		row.AppsWords += ", as chosen here."
	} else {
		row.AppsWords += "."
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// andList joins names as a sentence does: "a", "a and b", "a, b and c".
func andList(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func choiceWords(c ai.Choice) string {
	if c.Provider != "" && c.Provider != ai.ProviderOllama {
		from := "from " + ai.ProviderTitle(c.Provider)
		if c.FromPortal {
			return fmt.Sprintf("Chosen here by %s %s, %s.", c.UpdatedBy, changedAgo(c.UpdatedAt), from)
		}
		return "From config.toml, " + from + "."
	}
	switch {
	case c.FromPortal && c.File != "":
		return fmt.Sprintf("Chosen here by %s %s, over config.toml's %s.", c.UpdatedBy, changedAgo(c.UpdatedAt), c.File)
	case c.FromPortal:
		return fmt.Sprintf("Chosen here by %s %s.", c.UpdatedBy, changedAgo(c.UpdatedAt))
	case c.Model != "":
		return "From config.toml."
	}
	return ""
}

// changedAgo is a rough "when" for a change.
func changedAgo(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return count(int(d/time.Minute), "minute") + " ago"
	case d < 48*time.Hour:
		return count(int(d/time.Hour), "hour") + " ago"
	}
	return "on " + t.Local().Format("2 Jan 2006")
}

// count is n and unit, made plural when it needs to be.
func count(n int, unit string) string {
	return fmt.Sprintf("%d %s", n, plural(n, unit))
}

// modelDetail is what sets one model apart from another in the list.
func modelDetail(m ai.Model) string {
	var parts []string
	if m.ParameterSize != "" {
		parts = append(parts, m.ParameterSize)
	}
	if m.Quantization != "" {
		parts = append(parts, m.Quantization)
	}
	if m.Size > 0 {
		parts = append(parts, humanBytes(uint64(m.Size)))
	}
	if extra := extraCaps(m.Capabilities); extra != "" {
		parts = append(parts, extra)
	}
	return strings.Join(parts, " · ")
}

// extraCaps are the capabilities beyond the one that decides the kind.
func extraCaps(caps []string) string {
	var out []string
	for _, c := range caps {
		if c != "completion" && c != "decision" {
			out = append(out, c)
		}
	}
	return strings.Join(out, ", ")
}

func usageWords(u ai.Usage) string {
	s := fmt.Sprintf("%s: %s", ai.KindName(u.Kind), count(int(u.Requests), "request"))
	if u.Errors > 0 {
		s += fmt.Sprintf(", %d failed", u.Errors)
	}
	if u.InputTokens+u.OutputTokens > 0 {
		s += fmt.Sprintf(", %d tokens in, %d out", u.InputTokens, u.OutputTokens)
	}
	return s + ", last " + changedAgo(u.LastAt)
}

func (g *Gateway) aiSubmit(w http.ResponseWriter, r *http.Request) {
	if !g.sameOrigin(r) {
		g.crossOrigin(w, r)
		return
	}
	u, ok := g.aiUser(w, r)
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxAIForm)
	if err := r.ParseForm(); err != nil {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
		return
	}
	f := r.PostForm
	action, kind := f.Get("action"), f.Get("kind")
	if action == "regenerate" {
		g.aiRegenerate(w, r, u, f.Get("app"))
		return
	}
	controls, hosted := g.ai.(AIControls)
	if action == "key" && hosted {
		g.aiKey(w, r, u, controls, f.Get("provider"), f.Get("key"))
		return
	}
	if !ai.ValidKind(kind) {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), aiTestTimeout)
	defer cancel()
	name := ai.KindName(kind)
	var err error
	var done, what string
	switch action {
	case "save":
		var model string
		provider := f.Get("provider")
		if hosted && provider != "" {
			model, err = controls.SetModel(ctx, kind, provider, f.Get("model"), u.Username)
			done, what = "saved", fmt.Sprintf("set the %s to %s's %s", name, ai.ProviderTitle(provider), model)
		} else {
			model, err = g.ai.Set(ctx, kind, f.Get("model"), u.Username)
			done, what = "saved", fmt.Sprintf("set the %s to %s", name, model)
		}
	case "apps":
		if !hosted {
			g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
			return
		}
		every := f.Get("every") != ""
		apps := f["app"]
		err = controls.SetApps(kind, every, apps, u.Username)
		who := "every app"
		if !every {
			who = "only " + andList(apps)
			if len(apps) == 0 {
				who = "no app"
			}
		}
		done, what = "apps", fmt.Sprintf("gave the %s to %s", name, who)
	case "appsreset":
		if !hosted {
			g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
			return
		}
		err = controls.ResetApps(kind, u.Username)
		done, what = "appsreset", fmt.Sprintf("put the %s's apps back to config.toml's", name)
	case "reset":
		err = g.ai.Reset(kind, u.Username)
		done, what = "reset", fmt.Sprintf("put the %s back to config.toml's", name)
	case "test":
		res, err := g.ai.Test(ctx, kind)
		var msg string
		if err != nil {
			g.renderAI(w, r, u, http.StatusOK, nil, map[string]string{kind: "The test failed: " + err.Error()}, nil)
			return
		}
		msg = fmt.Sprintf("%s answered in %s: %s", res.Model, res.Took.Round(100*time.Millisecond), orQuote(res.Reply))
		g.renderAI(w, r, u, http.StatusOK, nil, nil, map[string]string{kind: msg})
		return
	default:
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
		return
	}
	if err != nil {
		msg := err.Error()
		g.renderAI(w, r, u, http.StatusBadRequest, nil, map[string]string{kind: strings.ToUpper(msg[:1]) + msg[1:]}, nil)
		return
	}
	g.activity.record(EventAI, "%s %s from %s", u.Username, what, g.clientIP(r))
	g.logf("gateway: %s %s", u.Username, what)
	http.Redirect(w, r, "/ai?done="+done+"&kind="+url.QueryEscape(kind)+"#k-"+url.QueryEscape(kind), http.StatusSeeOther)
}

// aiKey saves a hosted provider's API key. The key goes into secrets.db and
// is never shown again, here or anywhere; the activity feed says only that
// it changed.
func (g *Gateway) aiKey(w http.ResponseWriter, r *http.Request, u auth.User, controls AIControls, provider, key string) {
	if !ai.ValidProvider(provider) {
		g.message(w, r, http.StatusBadRequest, "That didn't go through", "The form couldn't be read. Try again.", "/ai", "Back to AI models")
		return
	}
	if err := controls.SetKey(provider, key, u.Username); err != nil {
		msg := err.Error()
		g.renderAI(w, r, u, http.StatusBadRequest, nil, map[string]string{"provider:" + provider: strings.ToUpper(msg[:1]) + msg[1:]}, nil)
		return
	}
	title := ai.ProviderTitle(provider)
	g.activity.record(EventAI, "%s set %s's API key from %s", u.Username, title, g.clientIP(r))
	g.logf("gateway: %s set %s's API key", u.Username, title)
	http.Redirect(w, r, "/ai?done=key&provider="+url.QueryEscape(provider)+"#p-"+url.QueryEscape(provider), http.StatusSeeOther)
}

func orQuote(s string) string {
	if s == "" {
		return "an empty reply"
	}
	return "“" + s + "”"
}

func (g *Gateway) aiRegenerate(w http.ResponseWriter, r *http.Request, u auth.User, app string) {
	rt, ok := g.routes.get(app)
	if !ok {
		g.message(w, r, http.StatusBadRequest, "That app isn't there", "It may have been removed.", "/ai", "Back to AI models")
		return
	}
	if err := g.ai.Regenerate(rt.Name, u.Username); err != nil {
		g.message(w, r, http.StatusInternalServerError, "That didn't go through", err.Error(), "/ai", "Back to AI models")
		return
	}
	g.activity.record(EventAI, "%s regenerated %s's AI token from %s", u.Username, rt.Name, g.clientIP(r))
	g.logf("gateway: %s regenerated %s's AI token", u.Username, rt.Name)
	http.Redirect(w, r, "/ai?done=regenerated&app="+url.QueryEscape(rt.Name)+"#a-"+url.QueryEscape(rt.Name), http.StatusSeeOther)
}

package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/auth"
)

// maxAIForm bounds the AI models page's forms: a kind and a model name.
const maxAIForm = 8 << 10

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

// aiView is the AI models page's state, for the template.
type aiView struct {
	URL, OllamaURL string
	OllamaError    string
	Kinds          []aiKindRow
	Apps           []aiAppRow
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
	}
	g.renderAI(w, r, u, http.StatusOK, notice, nil, nil)
}

// renderAI draws the page. errs and tests are by kind; notice is by kind, or
// "app:<name>" for an app.
func (g *Gateway) renderAI(w http.ResponseWriter, r *http.Request, u auth.User, status int, notice, errs, tests map[string]string) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	o := g.ai.Overview(ctx)
	v := aiView{URL: o.URL, OllamaURL: o.OllamaURL, OllamaError: o.ModelsError}
	for _, c := range o.Choices {
		row := aiKindRow{
			Kind: c.Kind, Name: ai.KindName(c.Kind), Model: c.Model, Missing: c.Missing,
			FromPortal: c.FromPortal, Notice: notice[c.Kind], Error: errs[c.Kind], Test: tests[c.Kind],
		}
		row.Source = choiceWords(c)
		for _, m := range o.Models {
			if m.Serves(c.Kind) {
				row.Options = append(row.Options, aiOption{Name: m.Name, Detail: modelDetail(m), Checked: m.Name == c.Model})
			}
		}
		v.Kinds = append(v.Kinds, row)
	}
	used := map[string][]ai.Usage{}
	for _, us := range o.Usage {
		used[us.Service] = append(used[us.Service], us)
	}
	for _, rt := range g.routes.all() {
		row := aiAppRow{Name: rt.Name, Title: rt.Title, Notice: notice["app:"+rt.Name]}
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

func choiceWords(c ai.Choice) string {
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
		model, err = g.ai.Set(ctx, kind, f.Get("model"), u.Username)
		done, what = "saved", fmt.Sprintf("set the %s to %s", name, model)
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

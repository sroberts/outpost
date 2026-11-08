// Package ai gives Outpost's apps the models the operator chose: one LLM and
// one System 1 model for the whole installation, each served by Ollama or a
// hosted provider (OpenAI, Anthropic, OpenRouter, TypeSafe).
//
// Apps don't reach Ollama themselves. Each is started with OUTPOST_AI_URL,
// a loopback endpoint this package serves, and OUTPOST_AI_TOKEN, its own. A
// request there goes to whichever model is chosen at that moment, so changing
// it takes effect on the next request with no app restarted. The choice comes
// from config.toml, unless the portal has made one, which wins; that is kept
// in ai.db. See docs/design/ai-models.md.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/sroberts/outpost/internal/apptoken"
)

// The two kinds of model an installation has.
const (
	// LLM generates text, through the OpenAI or Anthropic chat API.
	LLM = "llm"
	// System1 answers typed questions about an input with probabilities,
	// through POST /v1/systemone.
	System1 = "system1"
)

// Kinds are the kinds, in the order they're shown.
var Kinds = []string{LLM, System1}

// KindName is how a kind is written for people.
func KindName(kind string) string {
	if kind == System1 {
		return "System 1"
	}
	return "LLM"
}

// ValidKind reports whether kind is one of Kinds.
func ValidKind(kind string) bool { return slices.Contains(Kinds, kind) }

// Environment variables every app is started with.
const (
	EnvURL   = "OUTPOST_AI_URL"
	EnvToken = "OUTPOST_AI_TOKEN"
)

// pick is config.toml's choice for one kind.
type pick struct {
	provider, model string
	// apps may use it; nil is every app.
	apps []string
}

// Options configures a Service.
type Options struct {
	// Path is ai.db.
	Path string
	// OllamaURL is where Ollama's API is, such as http://127.0.0.1:11434.
	OllamaURL string
	// Listen is the loopback address the endpoint serves on, which apps are
	// told as OUTPOST_AI_URL.
	Listen string
	// LLM and System1 are config.toml's choice of model, used for a kind
	// the portal hasn't set; either may be empty. LLMProvider and
	// System1Provider serve them, "" meaning Ollama.
	LLM, System1                 string
	LLMProvider, System1Provider string
	// LLMApps and System1Apps are the apps config.toml lets use each; nil
	// is every app.
	LLMApps, System1Apps []string
	// Providers are config.toml's settings for the hosted providers, by name.
	Providers map[string]ProviderConfig
	// Keys is where the hosted providers' API keys are kept. Without it only
	// Ollama can be used.
	Keys Keys
	// Client reaches the providers; tests use fakes. It has no overall
	// timeout, since a streamed reply can run for minutes.
	Client *http.Client
	// Logf receives one line per request and per change.
	Logf func(format string, v ...any)
}

// Service is the AI endpoint and its settings. It is safe for concurrent use.
type Service struct {
	store    *store
	ollama   *ollama
	base     string
	listen   string
	file     map[string]pick
	provCfg  map[string]ProviderConfig
	keys     Keys
	catalogs catalogs
	client   *http.Client
	logf     func(format string, v ...any)
	now      func() time.Time

	mu sync.Mutex
	// issued is which app each handed-out token belongs to.
	issued *apptoken.Issued
	usage  map[usageKey]*Usage
}

// Open opens ai.db and readies the endpoint; Serve starts it.
func Open(opts Options) (*Service, error) {
	if opts.Listen == "" || opts.OllamaURL == "" {
		return nil, errors.New("ai: Listen and OllamaURL are required")
	}
	file := map[string]pick{
		LLM:     {opts.LLMProvider, opts.LLM, opts.LLMApps},
		System1: {opts.System1Provider, opts.System1, opts.System1Apps},
	}
	for k, p := range file {
		if p.provider == "" {
			p.provider = ProviderOllama
		}
		if !ProviderServes(p.provider, k) {
			return nil, fmt.Errorf("ai: %s can't serve the %s", ProviderTitle(p.provider), KindName(k))
		}
		file[k] = p
	}
	st, err := openStore(opts.Path)
	if err != nil {
		return nil, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Minute, // a large model can take minutes to load
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   8,
		}}
	}
	logf := opts.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	base := strings.TrimSuffix(opts.OllamaURL, "/")
	return &Service{
		store:   st,
		ollama:  newOllama(base, client),
		base:    base,
		listen:  opts.Listen,
		file:    file,
		provCfg: opts.Providers,
		keys:    opts.Keys,
		client:  client,
		logf:    logf,
		now:     time.Now,
		issued:  apptoken.NewIssued(tokenPrefix),
		usage:   map[usageKey]*Usage{},
	}, nil
}

// Close closes ai.db.
func (s *Service) Close() error { return s.store.close() }

// URL is what apps are told as OUTPOST_AI_URL.
func (s *Service) URL() string { return "http://" + s.listen }

// OllamaURL is where Ollama is.
func (s *Service) OllamaURL() string { return s.base }

// Choice is the model in force for one kind, where it came from, and which
// apps may use it.
type Choice struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider"`
	Model    string `json:"model,omitempty"`
	// FromPortal says the portal chose it, over FileProvider and File,
	// config.toml's.
	FromPortal   bool      `json:"from_portal,omitempty"`
	FileProvider string    `json:"file_provider,omitempty"`
	File         string    `json:"file,omitempty"`
	UpdatedBy    string    `json:"updated_by,omitempty"`
	UpdatedAt    time.Time `json:"updated_at,omitzero"`
	// Every app may use it, or only Apps.
	Every bool     `json:"every"`
	Apps  []string `json:"apps,omitempty"`
	// AppsFromPortal says the portal chose the apps, over config.toml's.
	AppsFromPortal bool `json:"apps_from_portal,omitempty"`
	// APIs are the paths apps can reach it through; for an OpenAI LLM, not
	// the Anthropic one.
	APIs []string `json:"apis,omitempty"`
	// Missing says the provider doesn't list Model, and NoKey that it needs a
	// key Outpost doesn't have. Only Overview fills them in.
	Missing bool `json:"missing,omitempty"`
	NoKey   bool `json:"no_key,omitempty"`
}

// Choice returns what is in force for kind.
func (s *Service) Choice(kind string) Choice {
	f := s.file[kind]
	c := Choice{Kind: kind, Provider: f.provider, Model: f.model, FileProvider: f.provider, File: f.model,
		Every: f.apps == nil, Apps: append([]string(nil), f.apps...)}
	if st, ok := s.store.get(kind); ok {
		c.Provider, c.Model, c.FromPortal, c.UpdatedBy, c.UpdatedAt = st.Provider, st.Model, true, st.UpdatedBy, st.UpdatedAt
	}
	if a, ok := s.store.getAccess(kind); ok {
		c.Every, c.Apps, c.AppsFromPortal = a.Every, a.Apps, true
	}
	if c.Every {
		c.Apps = nil
	}
	if c.Model != "" {
		c.APIs = providers[c.Provider].apis(kind)
	}
	return c
}

// allowed reports whether service may use kind.
func (c Choice) allowed(service string) bool {
	return c.Every || slices.Contains(c.Apps, service)
}

// Catalog is one provider's models for one kind, as the page offers them.
type Catalog struct {
	Provider string  `json:"provider"`
	Kind     string  `json:"kind"`
	Models   []Model `json:"models,omitempty"`
	// Complete says these are all its models, so one not here is refused;
	// otherwise they're suggestions, and any name can be typed in.
	Complete bool   `json:"complete,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Overview is everything the portal page and `outpost ai` show.
type Overview struct {
	URL       string           `json:"url"`
	OllamaURL string           `json:"ollama_url"`
	Choices   []Choice         `json:"choices"`
	Providers []ProviderStatus `json:"providers"`
	// Catalogs are what each provider has, for each kind it serves.
	Catalogs []Catalog `json:"catalogs"`
	// Models is everything Ollama has, or nil with ModelsError saying why
	// not.
	Models      []Model `json:"models,omitempty"`
	ModelsError string  `json:"models_error,omitempty"`
	Usage       []Usage `json:"usage,omitempty"`
}

// Overview asks each provider what it has and puts that beside the
// settings. The providers are asked at once, so a slow one costs only its
// own wait.
func (s *Service) Overview(ctx context.Context) Overview {
	o := Overview{URL: s.URL(), OllamaURL: s.base, Usage: s.Usage(), Providers: s.Providers()}
	type job struct{ provider, kind string }
	var jobs []job
	for _, k := range Kinds {
		for _, p := range ProviderNames {
			if ProviderServes(p, k) {
				jobs = append(jobs, job{p, k})
			}
		}
	}
	cats := make([]Catalog, len(jobs))
	// The hosted providers are asked at once; Ollama, meanwhile, once, and
	// its list shared by both kinds.
	var wg sync.WaitGroup
	for i, j := range jobs {
		if j.provider == ProviderOllama {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			cats[i] = catalogOf(j.provider, j.kind, s.models(ctx, j.provider, j.kind))
		}()
	}
	models, err := s.ollama.models(ctx)
	o.Models = models
	if err != nil {
		o.ModelsError = err.Error()
	}
	for i, j := range jobs {
		if j.provider == ProviderOllama {
			cats[i] = catalogOf(j.provider, j.kind, ollamaCatalog(models, err, j.kind))
		}
	}
	wg.Wait()
	o.Catalogs = cats
	for _, k := range Kinds {
		c := s.Choice(k)
		if c.Model != "" {
			if _, has := s.key(c.Provider); providers[c.Provider].keyed && !has {
				c.NoKey = true
			}
			for _, cat := range cats {
				if cat.Provider == c.Provider && cat.Kind == k && cat.Complete {
					_, ok := find(cat.Models, c.Model, c.Provider)
					c.Missing = !ok
				}
			}
		}
		o.Choices = append(o.Choices, c)
	}
	return o
}

func catalogOf(provider, kind string, cat catalog) Catalog {
	c := Catalog{Provider: provider, Kind: kind, Models: cat.models, Complete: cat.complete}
	if cat.err != nil {
		c.Error, c.Complete = cat.err.Error(), false
	}
	return c
}

// ollamaCatalog is the models of Ollama's list that serve kind.
func ollamaCatalog(models []Model, err error, kind string) catalog {
	var out []Model
	for _, m := range models {
		if m.Serves(kind) {
			out = append(out, m)
		}
	}
	return catalog{models: out, complete: err == nil, err: err}
}

// find looks a model up. For Ollama, as Ollama does: a name without a tag
// means :latest.
func find(models []Model, name, provider string) (Model, bool) {
	want := name
	if provider == ProviderOllama {
		want = fullName(name)
	}
	for _, m := range models {
		have := m.Name
		if provider == ProviderOllama {
			have = fullName(have)
		}
		if have == want {
			return m, true
		}
	}
	return Model{}, false
}

func fullName(name string) string {
	if !strings.Contains(name, ":") {
		return name + ":latest"
	}
	return name
}

func badKind(kind string) error {
	return fmt.Errorf("there's no kind of model called %q; it's %s or %s", kind, LLM, System1)
}

// Set is SetModel from the provider already in force for kind.
func (s *Service) Set(ctx context.Context, kind, model, by string) (string, error) {
	return s.SetModel(ctx, kind, "", model, by)
}

// SetModel makes provider's model the one for kind, over config.toml's, and
// returns the name it was saved as: for Ollama, Ollama's, so "laya" is saved
// as "laya:latest". A provider with a complete list must have the model in
// it, of that kind; one whose list can't be had saves nothing, since
// nothing could be checked. provider "" is the one already in force.
func (s *Service) SetModel(ctx context.Context, kind, provider, model, by string) (string, error) {
	if !ValidKind(kind) {
		return "", badKind(kind)
	}
	if provider == "" {
		provider = s.Choice(kind).Provider
	}
	p, ok := providers[provider]
	if !ok {
		return "", fmt.Errorf("there's no provider called %q; they're %s", provider, strings.Join(ProviderNames, ", "))
	}
	if !p.serves(kind) {
		return "", fmt.Errorf("%s doesn't serve a %s model", p.title, KindName(kind))
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("choose a model")
	}
	if !validModelName(model) {
		return "", fmt.Errorf("%q isn't a model name", model)
	}
	if _, has := s.key(provider); p.keyed && !has {
		return "", fmt.Errorf("add %s's API key first", p.title)
	}
	cat := s.models(ctx, provider, kind)
	saved := model
	switch {
	case cat.err != nil && (cat.complete || provider == ProviderOllama || p.models != ""):
		return "", fmt.Errorf("can't check the model, so it wasn't saved: %w", cat.err)
	case cat.complete:
		m, ok := find(cat.models, model, provider)
		if !ok {
			if provider == ProviderOllama {
				// Ollama lists every model it has, so check whether the model
				// exists at all, to say why it's refused.
				if all, err := s.ollama.models(ctx); err == nil {
					if m, ok := find(all, model, provider); ok {
						return "", fmt.Errorf("can't use %s as the %s: Ollama lists it as %s", m.Name, KindName(kind), capsWords(m.Capabilities))
					}
				}
				return "", fmt.Errorf("there's no %s in Ollama at %s; pull it with `ollama pull %s` first", model, s.base, model)
			}
			return "", fmt.Errorf("%s doesn't list a %s model called %s", p.title, KindName(kind), model)
		}
		saved = m.Name
	}
	if err := s.store.set(kind, provider, saved, by); err != nil {
		return "", err
	}
	s.logf("ai: %s set the %s to %s's %s", by, KindName(kind), p.title, saved)
	return saved, nil
}

// validModelName allows what the providers' names are made of, and nothing
// that could carry a second value.
func validModelName(name string) bool {
	if len(name) > 200 {
		return false
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-:/~@+", r) {
			return false
		}
	}
	return true
}

func capsWords(caps []string) string {
	if len(caps) == 0 {
		return "having no capabilities"
	}
	return strings.Join(caps, ", ")
}

// Reset goes back to config.toml's choice for kind.
func (s *Service) Reset(kind, by string) error {
	if !ValidKind(kind) {
		return badKind(kind)
	}
	if err := s.store.reset(kind); err != nil {
		return err
	}
	s.logf("ai: %s put the %s back to config.toml's", by, KindName(kind))
	return nil
}

// SetApps chooses which apps may use kind: every app, or those named.
func (s *Service) SetApps(kind string, every bool, apps []string, by string) error {
	if !ValidKind(kind) {
		return badKind(kind)
	}
	var clean []string
	if !every {
		seen := map[string]bool{}
		for _, a := range apps {
			a = strings.TrimSpace(a)
			if a != "" && !seen[a] {
				seen[a] = true
				clean = append(clean, a)
			}
		}
		sort.Strings(clean)
	}
	if err := s.store.setAccess(kind, every, clean, by); err != nil {
		return err
	}
	who := "every app"
	if !every {
		who = orNoApps(clean)
	}
	s.logf("ai: %s gave the %s to %s", by, KindName(kind), who)
	return nil
}

func orNoApps(apps []string) string {
	if len(apps) == 0 {
		return "no app"
	}
	return strings.Join(apps, ", ")
}

// ResetApps goes back to config.toml's apps for kind.
func (s *Service) ResetApps(kind, by string) error {
	if !ValidKind(kind) {
		return badKind(kind)
	}
	if err := s.store.resetAccess(kind); err != nil {
		return err
	}
	s.logf("ai: %s put the %s's apps back to config.toml's", by, KindName(kind))
	return nil
}

// Resolve is the variables service is started with, and a revision that
// changes when they do: only when its token is regenerated. It's called on
// every reconcile tick for each running app, so it answers from memory.
func (s *Service) Resolve(service string) (map[string]string, string) {
	tok, gen := s.store.token(service)
	s.issued.Add(tok, service)
	return map[string]string{EnvURL: s.URL(), EnvToken: tok}, "ai:" + strconv.FormatInt(gen, 10)
}

// Regenerate gives service a new token. The old one stops working at once;
// the supervisor sees the revision change and restarts the app with the new
// one.
func (s *Service) Regenerate(service, by string) error {
	if service == "" {
		return errors.New("an app is required")
	}
	old, _ := s.store.token(service)
	if err := s.store.regenerate(service); err != nil {
		return err
	}
	s.issued.Remove(old)
	s.logf("ai: %s regenerated %s's token", by, service)
	return nil
}

// serviceFor is the app a token was handed to.
func (s *Service) serviceFor(tok string) (string, bool) {
	return s.issued.Service(tok)
}

// TestResult is what a test request got back.
type TestResult struct {
	Kind     string        `json:"kind"`
	Provider string        `json:"provider"`
	Model    string        `json:"model"`
	Reply    string        `json:"reply"`
	Took     time.Duration `json:"took"`
}

// Test sends the model for kind one small request, the way an app's goes,
// and reports what came back.
func (s *Service) Test(ctx context.Context, kind string) (TestResult, error) {
	if !ValidKind(kind) {
		return TestResult{}, badKind(kind)
	}
	c := s.Choice(kind)
	if c.Model == "" {
		return TestResult{}, fmt.Errorf("no %s is chosen", KindName(kind))
	}
	r := TestResult{Kind: kind, Provider: c.Provider, Model: c.Model}
	start := s.now()
	var err error
	switch {
	case kind == System1:
		r.Reply, err = s.testSystem1(ctx, c)
	case c.Provider == ProviderOllama:
		r.Reply, err = s.testOllamaLLM(ctx, c.Model)
	default:
		r.Reply, err = s.testLLM(ctx, c)
	}
	r.Took = s.now().Sub(start)
	return r, err
}

// testOllamaLLM uses Ollama's own API, which can turn thinking off.
func (s *Service) testOllamaLLM(ctx context.Context, model string) (string, error) {
	req := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "Reply with one word: ready"}},
		"options":  map[string]any{"num_predict": 32},
	}
	// A thinking model would otherwise spend the whole budget thinking. Only
	// asked of one that thinks: Ollama refuses think for any other.
	if models, err := s.ollama.models(ctx); err == nil {
		if m, ok := find(models, model, ProviderOllama); ok && slices.Contains(m.Capabilities, "thinking") {
			req["think"] = false
		}
	}
	var out struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := s.ollama.call(ctx, http.MethodPost, "/api/chat", req, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.Message.Content), nil
}

// testLLM asks a hosted LLM through the API it takes natively.
func (s *Service) testLLM(ctx context.Context, c Choice) (string, error) {
	msgs := []map[string]string{{"role": "user", "content": "Reply with one word: ready"}}
	if c.Provider == ProviderAnthropic {
		b, err := s.call(ctx, c, protoAnthropic, map[string]any{"messages": msgs, "max_tokens": 32})
		if err != nil {
			return "", err
		}
		var out struct {
			Content []struct {
				Type, Text string
			} `json:"content"`
		}
		if err := json.Unmarshal(b, &out); err != nil {
			return "", fmt.Errorf("the reply isn't what Outpost expects: %w", err)
		}
		var text []string
		for _, part := range out.Content {
			if part.Type == "text" {
				text = append(text, part.Text)
			}
		}
		return strings.TrimSpace(strings.Join(text, "")), nil
	}
	// No token limit: a reasoning model refuses max_tokens, and spends a
	// small max_completion_tokens before it says anything.
	b, err := s.call(ctx, c, protoOpenAI, map[string]any{"messages": msgs})
	if err != nil {
		return "", err
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("the reply isn't what Outpost expects: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", errors.New("the reply had no choices in it")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

func (s *Service) testSystem1(ctx context.Context, c Choice) (string, error) {
	b, err := s.call(ctx, c, protoSystemOne, map[string]any{
		"state": "The sky over the harbour is clear and blue this morning.",
		"questions": map[string]any{
			"weather": map[string]string{"type": "noul", "instructions": "Is this about the weather?"},
		},
	})
	if err != nil {
		return "", err
	}
	var out struct {
		Answers map[string]struct {
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return "", fmt.Errorf("the reply isn't what Outpost expects: %w", err)
	}
	a, ok := out.Answers["weather"]
	if !ok || a.Noul == nil {
		return "", errors.New("the reply had no answer to the test question")
	}
	return fmt.Sprintf("“Is this about the weather?” %.0f%% yes", *a.Noul*100), nil
}

// usageKey is one app's use of one kind.
type usageKey struct{ service, kind string }

// Usage counts one app's requests for one kind since the daemon started.
type Usage struct {
	Service      string    `json:"service"`
	Kind         string    `json:"kind"`
	Requests     int64     `json:"requests"`
	Errors       int64     `json:"errors"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	LastAt       time.Time `json:"last_at,omitzero"`
	LastError    string    `json:"last_error,omitempty"`
	LastErrorAt  time.Time `json:"last_error_at,omitzero"`
}

func (s *Service) count(service, kind string, in, out int64, failure string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := usageKey{service, kind}
	u := s.usage[k]
	if u == nil {
		u = &Usage{Service: service, Kind: kind}
		s.usage[k] = u
	}
	now := s.now()
	u.Requests++
	u.LastAt = now
	u.InputTokens += in
	u.OutputTokens += out
	if failure != "" {
		u.Errors++
		u.LastError, u.LastErrorAt = failure, now
	}
}

// Usage is every app's counts, by app and kind.
func (s *Service) Usage() []Usage {
	s.mu.Lock()
	out := make([]Usage, 0, len(s.usage))
	for _, u := range s.usage {
		out = append(out, *u)
	}
	s.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Service != out[j].Service {
			return out[i].Service < out[j].Service
		}
		return slices.Index(Kinds, out[i].Kind) < slices.Index(Kinds, out[j].Kind)
	})
	return out
}

// Serve answers apps on ln until ctx ends.
func (s *Service) Serve(ctx context.Context, ln net.Listener) error {
	srv := &http.Server{
		Handler:           s,
		ReadHeaderTimeout: 10 * time.Second,
		// No write timeout: a streamed reply runs as long as the model does.
	}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut) // a stream still open at exit is cut, which is all that's left to do
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// jsonBody marshals v, for a small reply built here.
func jsonBody(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}

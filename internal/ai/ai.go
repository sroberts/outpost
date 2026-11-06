// Package ai gives Outpost's apps the models the operator chose: one LLM and
// one System 1 model for the whole installation, both served by Ollama.
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
	"crypto/sha256"
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

// Options configures a Service.
type Options struct {
	// Path is ai.db.
	Path string
	// OllamaURL is where Ollama's API is, such as http://127.0.0.1:11434.
	OllamaURL string
	// Listen is the loopback address the endpoint serves on, which apps are
	// told as OUTPOST_AI_URL.
	Listen string
	// LLM and System1 are config.toml's choices, used for a kind the portal
	// hasn't set. Either may be empty.
	LLM, System1 string
	// Client reaches Ollama; tests use a fake. It has no overall timeout,
	// since a streamed reply can run for minutes.
	Client *http.Client
	// Logf receives one line per request and per change.
	Logf func(format string, v ...any)
}

// Service is the AI endpoint and its settings. It is safe for concurrent use.
type Service struct {
	store  *store
	ollama *ollama
	base   string
	listen string
	file   map[string]string
	client *http.Client
	logf   func(format string, v ...any)
	now    func() time.Time

	mu sync.Mutex
	// issued maps a token's hash to the app it was handed to.
	issued map[[32]byte]string
	usage  map[usageKey]*Usage
}

// Open opens ai.db and readies the endpoint; Serve starts it.
func Open(opts Options) (*Service, error) {
	if opts.Listen == "" || opts.OllamaURL == "" {
		return nil, errors.New("ai: Listen and OllamaURL are required")
	}
	st, err := openStore(opts.Path)
	if err != nil {
		return nil, err
	}
	client := opts.Client
	if client == nil {
		client = &http.Client{Transport: &http.Transport{
			Proxy:                 nil, // Ollama is local, or named by the operator; never via a proxy
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
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
		store:  st,
		ollama: newOllama(base, client),
		base:   base,
		listen: opts.Listen,
		file:   map[string]string{LLM: opts.LLM, System1: opts.System1},
		client: client,
		logf:   logf,
		now:    time.Now,
		issued: map[[32]byte]string{},
		usage:  map[usageKey]*Usage{},
	}, nil
}

// Close closes ai.db.
func (s *Service) Close() error { return s.store.close() }

// URL is what apps are told as OUTPOST_AI_URL.
func (s *Service) URL() string { return "http://" + s.listen }

// OllamaURL is where models are served from.
func (s *Service) OllamaURL() string { return s.base }

// Choice is the model in force for one kind, and where it came from.
type Choice struct {
	Kind  string `json:"kind"`
	Model string `json:"model,omitempty"`
	// FromPortal says the portal chose it, over File, config.toml's.
	FromPortal bool      `json:"from_portal,omitempty"`
	File       string    `json:"file,omitempty"`
	UpdatedBy  string    `json:"updated_by,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
	// Missing says Ollama doesn't have Model, as of the last listing. Only
	// Overview fills it in.
	Missing bool `json:"missing,omitempty"`
}

// Choice returns what is in force for kind.
func (s *Service) Choice(kind string) Choice {
	c := Choice{Kind: kind, Model: s.file[kind], File: s.file[kind]}
	if st, ok := s.store.get(kind); ok {
		c.Model, c.FromPortal, c.UpdatedBy, c.UpdatedAt = st.Model, true, st.UpdatedBy, st.UpdatedAt
	}
	return c
}

// Overview is everything the portal page and `outpost ai` show.
type Overview struct {
	URL       string   `json:"url"`
	OllamaURL string   `json:"ollama_url"`
	Choices   []Choice `json:"choices"`
	// Models is what Ollama has, or nil with ModelsError saying why not.
	Models      []Model `json:"models,omitempty"`
	ModelsError string  `json:"models_error,omitempty"`
	Usage       []Usage `json:"usage,omitempty"`
}

// Overview asks Ollama what it has and puts that beside the settings.
func (s *Service) Overview(ctx context.Context) Overview {
	o := Overview{URL: s.URL(), OllamaURL: s.base, Usage: s.Usage()}
	models, err := s.ollama.models(ctx)
	if err != nil {
		o.ModelsError = err.Error()
	}
	o.Models = models
	for _, k := range Kinds {
		c := s.Choice(k)
		if err == nil && c.Model != "" {
			_, ok := find(models, c.Model)
			c.Missing = !ok
		}
		o.Choices = append(o.Choices, c)
	}
	return o
}

// find looks a model up the way Ollama does: a name without a tag means
// :latest.
func find(models []Model, name string) (Model, bool) {
	want := fullName(name)
	for _, m := range models {
		if fullName(m.Name) == want {
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

// Set makes model the one for kind, over config.toml's, and returns the name
// it was saved as: Ollama's, so "laya" is saved as "laya:latest". It has to
// be one Ollama has now, and of that kind; with Ollama unreachable nothing
// can be checked, so nothing is saved.
func (s *Service) Set(ctx context.Context, kind, model, by string) (string, error) {
	if !ValidKind(kind) {
		return "", fmt.Errorf("there's no kind of model called %q; it's %s or %s", kind, LLM, System1)
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return "", errors.New("choose a model")
	}
	models, err := s.ollama.models(ctx)
	if err != nil {
		return "", fmt.Errorf("can't check the model, so it wasn't saved: %w", err)
	}
	m, ok := find(models, model)
	if !ok {
		return "", fmt.Errorf("there's no %s in Ollama at %s; pull it with `ollama pull %s` first", model, s.base, model)
	}
	if !m.Serves(kind) {
		return "", fmt.Errorf("can't use %s as the %s: Ollama lists it as %s", m.Name, KindName(kind), capsWords(m.Capabilities))
	}
	if err := s.store.set(kind, m.Name, by); err != nil {
		return "", err
	}
	s.logf("ai: %s set the %s to %s", by, KindName(kind), m.Name)
	return m.Name, nil
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
		return fmt.Errorf("there's no kind of model called %q; it's %s or %s", kind, LLM, System1)
	}
	if err := s.store.reset(kind); err != nil {
		return err
	}
	s.logf("ai: %s put the %s back to config.toml's", by, KindName(kind))
	return nil
}

// Resolve is the variables service is started with, and a revision that
// changes when they do: only when its token is regenerated. It's called on
// every reconcile tick for each running app, so it answers from memory.
func (s *Service) Resolve(service string) (map[string]string, string) {
	tok, gen := s.store.token(service)
	s.mu.Lock()
	s.issued[sha256.Sum256([]byte(tok))] = service
	s.mu.Unlock()
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
	s.mu.Lock()
	delete(s.issued, sha256.Sum256([]byte(old)))
	s.mu.Unlock()
	s.logf("ai: %s regenerated %s's token", by, service)
	return nil
}

// serviceFor is the app a token was handed to.
func (s *Service) serviceFor(tok string) (string, bool) {
	if !strings.HasPrefix(tok, tokenPrefix) {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	svc, ok := s.issued[sha256.Sum256([]byte(tok))]
	return svc, ok
}

// TestResult is what a test request got back.
type TestResult struct {
	Kind  string        `json:"kind"`
	Model string        `json:"model"`
	Reply string        `json:"reply"`
	Took  time.Duration `json:"took"`
}

// Test sends the model for kind one small request, as an app would, and
// reports what came back.
func (s *Service) Test(ctx context.Context, kind string) (TestResult, error) {
	if !ValidKind(kind) {
		return TestResult{}, fmt.Errorf("there's no kind of model called %q; it's %s or %s", kind, LLM, System1)
	}
	c := s.Choice(kind)
	if c.Model == "" {
		return TestResult{}, fmt.Errorf("no %s is chosen", KindName(kind))
	}
	r := TestResult{Kind: kind, Model: c.Model}
	start := s.now()
	var err error
	if kind == LLM {
		r.Reply, err = s.testLLM(ctx, c.Model)
	} else {
		r.Reply, err = s.testSystem1(ctx, c.Model)
	}
	r.Took = s.now().Sub(start)
	return r, err
}

func (s *Service) testLLM(ctx context.Context, model string) (string, error) {
	req := map[string]any{
		"model":    model,
		"stream":   false,
		"messages": []map[string]string{{"role": "user", "content": "Reply with one word: ready"}},
		"options":  map[string]any{"num_predict": 32},
	}
	// A thinking model would otherwise spend the whole budget thinking. Only
	// asked of one that thinks: Ollama refuses think for any other.
	if models, err := s.ollama.models(ctx); err == nil {
		if m, ok := find(models, model); ok && slices.Contains(m.Capabilities, "thinking") {
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

func (s *Service) testSystem1(ctx context.Context, model string) (string, error) {
	req := map[string]any{
		"model": model,
		"state": "The sky over the harbour is clear and blue this morning.",
		"questions": map[string]any{
			"weather": map[string]string{"type": "noul", "instructions": "Is this about the weather?"},
		},
	}
	var out struct {
		Answers map[string]struct {
			Noul *float64 `json:"noul"`
		} `json:"answers"`
	}
	if err := s.ollama.call(ctx, http.MethodPost, "/v1/systemone", req, &out); err != nil {
		return "", err
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

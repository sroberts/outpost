package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Providers serve the models. Ollama runs on the operator's machine and
// needs no key; the rest are hosted, and each needs an API key, kept as a
// secret in secrets.db and given to no app.
const (
	ProviderOllama     = "ollama"
	ProviderOpenAI     = "openai"
	ProviderAnthropic  = "anthropic"
	ProviderOpenRouter = "openrouter"
	ProviderTypeSafe   = "typesafe"
)

// ProviderNames are the providers, in the order they're shown.
var ProviderNames = []string{ProviderOllama, ProviderOpenAI, ProviderAnthropic, ProviderOpenRouter, ProviderTypeSafe}

// The APIs apps call, each under its own prefix on the endpoint.
const (
	protoOpenAI    = "openai"    // OpenAI Chat Completions
	protoAnthropic = "anthropic" // Anthropic Messages
	protoSystemOne = "systemone" // System 1
)

// provider is what Outpost knows about one: which kinds it serves, where,
// and how it takes its key.
type provider struct {
	name, title string
	base        string // the default; config.toml can move it
	keyed       bool
	// paths are the upstream path for each API an app can call it with. An
	// API that isn't here is one the provider doesn't take.
	paths map[string]string
	// decisions says System 1 requests go to OpenAI's Decisions API, which
	// takes a different shape and has to be translated both ways.
	decisions bool
	// models is where its model list is, or "" for none.
	models string
}

var providers = map[string]provider{
	ProviderOllama: {
		name: ProviderOllama, title: "Ollama",
		paths: map[string]string{protoOpenAI: "/v1/chat/completions", protoAnthropic: "/v1/messages", protoSystemOne: "/v1/systemone"},
	},
	ProviderOpenAI: {
		name: ProviderOpenAI, title: "OpenAI", base: "https://api.openai.com", keyed: true,
		// OpenAI doesn't take the Anthropic Messages API, so an app calling
		// /anthropic is refused while the LLM is OpenAI's.
		paths:     map[string]string{protoOpenAI: "/v1/chat/completions", protoSystemOne: "/v1/decisions"},
		decisions: true,
		models:    "/v1/models",
	},
	ProviderAnthropic: {
		name: ProviderAnthropic, title: "Anthropic", base: "https://api.anthropic.com", keyed: true,
		// Anthropic also answers the OpenAI Chat Completions API.
		paths:  map[string]string{protoAnthropic: "/v1/messages", protoOpenAI: "/v1/chat/completions"},
		models: "/v1/models",
	},
	ProviderOpenRouter: {
		name: ProviderOpenRouter, title: "OpenRouter", base: "https://openrouter.ai/api", keyed: true,
		paths:  map[string]string{protoOpenAI: "/v1/chat/completions", protoAnthropic: "/v1/messages", protoSystemOne: "/v1/systemone"},
		models: "/v1/models",
	},
	ProviderTypeSafe: {
		name: ProviderTypeSafe, title: "TypeSafe", base: "https://api.typesafe.ai", keyed: true,
		paths: map[string]string{protoSystemOne: "/v1/systemone"},
	},
}

// ProviderTitle is how a provider is written for people.
func ProviderTitle(name string) string {
	if p, ok := providers[name]; ok {
		return p.title
	}
	return name
}

// ValidProvider reports whether name is one of ProviderNames.
func ValidProvider(name string) bool { _, ok := providers[name]; return ok }

// ProviderServes reports whether a provider can be chosen for kind.
func ProviderServes(name, kind string) bool {
	p, ok := providers[name]
	if !ok {
		return false
	}
	return p.serves(kind)
}

func (p provider) serves(kind string) bool {
	if kind == System1 {
		_, ok := p.paths[protoSystemOne]
		return ok
	}
	_, a := p.paths[protoOpenAI]
	_, b := p.paths[protoAnthropic]
	return a || b
}

// apis are the APIs an app can reach the provider's kind through, as the
// paths apps call.
func (p provider) apis(kind string) []string {
	if kind == System1 {
		return []string{"/systemone/v1/systemone"}
	}
	var out []string
	if _, ok := p.paths[protoOpenAI]; ok {
		out = append(out, "/openai/v1/chat/completions")
	}
	if _, ok := p.paths[protoAnthropic]; ok {
		out = append(out, "/anthropic/v1/messages")
	}
	return out
}

// DefaultKeySecret is the secret a provider's key is kept in, unless
// config.toml names another.
func DefaultKeySecret(provider string) string { return provider + "-api-key" }

// ProviderConfig is config.toml's settings for one hosted provider.
type ProviderConfig struct {
	// BaseURL moves the provider, for a proxy or a compatible server.
	BaseURL string
	// KeySecret is the secret its key is kept in.
	KeySecret string
}

// Keys is where provider keys are kept: secrets.db, through the daemon.
type Keys interface {
	// Value is the secret's value, and false if there's no such secret.
	Value(name string) (string, bool)
	// Put creates the secret, given to no app, or replaces its value.
	Put(name, value string) error
}

// ProviderStatus is one provider as the page and `outpost ai` show it.
type ProviderStatus struct {
	Name      string   `json:"name"`
	Title     string   `json:"title"`
	Kinds     []string `json:"kinds"`
	BaseURL   string   `json:"base_url"`
	Keyed     bool     `json:"keyed,omitempty"`
	KeySecret string   `json:"key_secret,omitempty"`
	HasKey    bool     `json:"has_key,omitempty"`
}

// catalog is one provider's model list, as far as it can be had: complete,
// so a model not in it can be refused, or only suggestions.
type catalog struct {
	models   []Model
	complete bool
	err      error
	at       time.Time
}

// catalogTTL is how long a hosted provider's list is reused. They change
// rarely, and listing OpenRouter's is a large download.
const catalogTTL = 10 * time.Minute

// catalogs caches the hosted providers' lists, by provider and kind.
type catalogs struct {
	mu    sync.Mutex
	cache map[string]catalog
}

func (c *catalogs) get(key string, now time.Time) (catalog, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cat, ok := c.cache[key]
	if !ok || now.Sub(cat.at) > catalogTTL {
		return catalog{}, false
	}
	return cat, true
}

func (c *catalogs) put(key string, cat catalog) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache == nil {
		c.cache = map[string]catalog{}
	}
	c.cache[key] = cat
}

func (c *catalogs) forget(provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.cache {
		if strings.HasPrefix(k, provider+"/") {
			delete(c.cache, k)
		}
	}
}

// staticSystem1 are the System 1 models a provider without a list of them
// serves. Any other name can still be typed in.
var staticSystem1 = map[string][]Model{
	ProviderOpenAI:   {{Name: "gpt-6-luna", Note: "OpenAI's decision model, through the Decisions API", Capabilities: []string{capDecision}}},
	ProviderTypeSafe: {{Name: "jev-latest", Note: "the newest Jev", Capabilities: []string{capDecision}}},
}

// models is what provider has for kind. Ollama's is listed afresh each
// time; a hosted provider's is cached.
func (s *Service) models(ctx context.Context, name, kind string) catalog {
	if name == ProviderOllama {
		ms, err := s.ollama.models(ctx)
		return ollamaCatalog(ms, err, kind)
	}
	p := providers[name]
	if !p.serves(kind) {
		return catalog{}
	}
	if kind == System1 && name != ProviderOpenRouter {
		return catalog{models: staticSystem1[name]}
	}
	key := name + "/" + kind
	if cat, ok := s.catalogs.get(key, s.now()); ok {
		return cat
	}
	cat := s.fetchCatalog(ctx, p, kind)
	cat.at = s.now()
	// A failure is kept for a minute, not ten: it's likely a key being fixed.
	if cat.err != nil {
		cat.at = cat.at.Add(-catalogTTL + time.Minute)
	}
	s.catalogs.put(key, cat)
	return cat
}

func (s *Service) fetchCatalog(ctx context.Context, p provider, kind string) catalog {
	key, hasKey := s.key(p.name)
	// OpenRouter lists its models to anyone; the others want the key.
	if !hasKey && p.name != ProviderOpenRouter {
		return catalog{err: fmt.Errorf("add %s's API key to list its models", p.title)}
	}
	path := p.models
	if p.name == ProviderOpenRouter && kind == System1 {
		path += "?output_modalities=decisions"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.providerBase(p.name)+path, nil)
	if err != nil {
		return catalog{err: err}
	}
	if hasKey {
		authorize(req.Header, p.name, key)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return catalog{err: fmt.Errorf("can't reach %s: %w", p.title, unwrapURL(err))}
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return catalog{err: fmt.Errorf("read %s's model list: %w", p.title, err)}
	}
	if resp.StatusCode != http.StatusOK {
		return catalog{err: fmt.Errorf("%s answered %s: %s", p.title, resp.Status, ollamaError(b))}
	}
	var list struct {
		Data []struct {
			ID            string   `json:"id"`
			DisplayName   string   `json:"display_name"`
			Name          string   `json:"name"`
			ContextLength int64    `json:"context_length"`
			Endpoints     []string `json:"endpoints"`
			Pricing       struct {
				Prompt string `json:"prompt"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return catalog{err: fmt.Errorf("%s's model list isn't what Outpost expects: %w", p.title, err)}
	}
	capability := capCompletion
	if kind == System1 {
		capability = capDecision
	}
	var out []Model
	for _, d := range list.Data {
		if d.ID == "" {
			continue
		}
		switch p.name {
		case ProviderOpenAI:
			if !openAIChatModel(d.ID) {
				continue
			}
		case ProviderOpenRouter:
			// A model OpenRouter serves only some other way, such as images.
			if kind == LLM && len(d.Endpoints) > 0 && !slices.Contains(d.Endpoints, "/api/v1/chat/completions") {
				continue
			}
			// Its decisions list has extraction models in it too.
			if kind == System1 && len(d.Endpoints) > 0 && !slices.Contains(d.Endpoints, "/api/alpha/decisions") {
				continue
			}
		}
		m := Model{Name: d.ID, Capabilities: []string{capability}}
		var note []string
		if t := firstOf(d.DisplayName, d.Name); t != "" && t != d.ID {
			note = append(note, t)
		}
		if d.ContextLength > 0 {
			note = append(note, fmt.Sprintf("%dK context", d.ContextLength/1000))
		}
		if price := perMillion(d.Pricing.Prompt); price != "" {
			note = append(note, price+" per million input tokens")
		}
		m.Note = strings.Join(note, " · ")
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return catalog{models: out, complete: true}
}

// openAIChatModel tells OpenAI's chat models from the rest of its list,
// which has embeddings, speech, images and moderation in it too.
func openAIChatModel(id string) bool {
	for _, not := range []string{"embedding", "tts", "whisper", "transcribe", "dall-e", "image", "moderation",
		"realtime", "audio", "search", "davinci", "babbage", "sora", "computer-use", "decisions"} {
		if strings.Contains(id, not) {
			return false
		}
	}
	return strings.HasPrefix(id, "gpt-") || strings.HasPrefix(id, "chatgpt-") ||
		(len(id) > 1 && id[0] == 'o' && id[1] >= '0' && id[1] <= '9')
}

func firstOf(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// perMillion turns OpenRouter's price per token into one per million.
func perMillion(perToken string) string {
	var f float64
	if _, err := fmt.Sscanf(perToken, "%g", &f); err != nil || f < 0 {
		return ""
	}
	if f == 0 {
		return "free"
	}
	return fmt.Sprintf("$%.2f", f*1e6)
}

// providerBase is where a provider is: config.toml's base_url, or its own.
func (s *Service) providerBase(name string) string {
	if name == ProviderOllama {
		return s.base
	}
	if c, ok := s.provCfg[name]; ok && c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return providers[name].base
}

// keySecret is the secret a provider's key is kept in.
func (s *Service) keySecret(name string) string {
	if c, ok := s.provCfg[name]; ok && c.KeySecret != "" {
		return c.KeySecret
	}
	return DefaultKeySecret(name)
}

// key is a hosted provider's key, if there is one.
func (s *Service) key(name string) (string, bool) {
	if s.keys == nil || !providers[name].keyed {
		return "", false
	}
	v, ok := s.keys.Value(s.keySecret(name))
	return v, ok && v != ""
}

// authorize sets a provider's key on a request, the way it takes it.
func authorize(h http.Header, provider, key string) {
	if provider == ProviderAnthropic {
		h.Set("X-Api-Key", key)
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", "2023-06-01")
		}
		return
	}
	h.Set("Authorization", "Bearer "+key)
	if provider == ProviderOpenRouter {
		// OpenRouter's attribution, so its dashboard says what made a request.
		h.Set("X-Title", "Outpost")
	}
}

// Providers is every provider, with whether its key is set.
func (s *Service) Providers() []ProviderStatus {
	out := make([]ProviderStatus, 0, len(ProviderNames))
	for _, name := range ProviderNames {
		p := providers[name]
		st := ProviderStatus{Name: name, Title: p.title, BaseURL: s.providerBase(name), Keyed: p.keyed}
		for _, k := range Kinds {
			if p.serves(k) {
				st.Kinds = append(st.Kinds, k)
			}
		}
		if p.keyed {
			st.KeySecret = s.keySecret(name)
			_, st.HasKey = s.key(name)
		}
		out = append(out, st)
	}
	return out
}

// SetKey saves a hosted provider's API key, as a secret given to no app.
func (s *Service) SetKey(provider, value, by string) error {
	p, ok := providers[provider]
	if !ok || !p.keyed {
		return fmt.Errorf("%s doesn't take an API key; the providers that do are openai, anthropic, openrouter and typesafe", provider)
	}
	if s.keys == nil {
		return fmt.Errorf("this daemon has no secrets store to keep %s's key in", p.title)
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("paste %s's API key", p.title)
	}
	if err := s.keys.Put(s.keySecret(provider), value); err != nil {
		return err
	}
	s.catalogs.forget(provider)
	s.logf("ai: %s set %s's API key (secret %s)", by, p.title, s.keySecret(provider))
	return nil
}

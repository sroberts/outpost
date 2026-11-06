package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Defaults for [ai].
const (
	DefaultAIListen  = "127.0.0.1:47901"
	DefaultOllamaURL = "http://127.0.0.1:11434"
)

// AI is the models Outpost's apps are given: one LLM and one System 1 model,
// each served by Ollama or a hosted provider. The portal's AI models page can
// choose them too, and its choice wins over these. See
// docs/design/ai-models.md.
type AI struct {
	// Listen is the loopback address of the endpoint apps call, which they
	// are told as OUTPOST_AI_URL.
	Listen string `toml:"listen"`
	// OllamaURL is where Ollama's API is.
	OllamaURL string `toml:"ollama_url"`
	// LLM and System1 are model names as their provider lists them, such as
	// "gemma4:e4b" and "laya:latest" from Ollama. Unset means none until the
	// portal chooses one.
	LLM     string `toml:"llm"`
	System1 string `toml:"system1"`
	// LLMProvider and System1Provider serve them; unset is Ollama.
	LLMProvider     string `toml:"llm_provider"`
	System1Provider string `toml:"system1_provider"`
	// LLMApps and System1Apps are the apps that may use each; unset is every
	// app, and [] is none. Pointers, so unset and empty differ.
	LLMApps     *[]string `toml:"llm_apps"`
	System1Apps *[]string `toml:"system1_apps"`
	// The hosted providers' settings. Their keys aren't here: each is a
	// secret in secrets.db, set on the AI models page or with `outpost ai
	// key`.
	OpenAI     AIProvider `toml:"openai"`
	Anthropic  AIProvider `toml:"anthropic"`
	OpenRouter AIProvider `toml:"openrouter"`
	TypeSafe   AIProvider `toml:"typesafe"`
}

// AIProvider is one hosted provider's settings.
type AIProvider struct {
	// BaseURL moves it, for a proxy or a compatible server; unset is the
	// provider's own.
	BaseURL string `toml:"base_url"`
	// APIKeySecret is the secret its key is kept in; unset is
	// "<provider>-api-key".
	APIKeySecret string `toml:"api_key_secret"`
}

// Which providers serve which kind. internal/ai has the full table; this is
// what config.toml is checked against, kept in step with it.
var (
	llmProviders     = []string{"ollama", "openai", "anthropic", "openrouter"}
	system1Providers = []string{"ollama", "openai", "openrouter", "typesafe"}
)

// Providers is the hosted providers' settings, by name.
func (a AI) Providers() map[string]AIProvider {
	return map[string]AIProvider{"openai": a.OpenAI, "anthropic": a.Anthropic, "openrouter": a.OpenRouter, "typesafe": a.TypeSafe}
}

// ListenAddr is Listen, or the default.
func (a AI) ListenAddr() string {
	if a.Listen == "" {
		return DefaultAIListen
	}
	return a.Listen
}

// OllamaBase is OllamaURL, or the default, without a trailing slash.
func (a AI) OllamaBase() string {
	if a.OllamaURL == "" {
		return DefaultOllamaURL
	}
	return strings.TrimSuffix(a.OllamaURL, "/")
}

func (a AI) validate() error {
	host, port, err := net.SplitHostPort(a.ListenAddr())
	if err != nil {
		return fmt.Errorf("listen %q: want host:port, like %s", a.Listen, DefaultAIListen)
	}
	// Apps are told this address, and it answers anything holding a token:
	// it stays on this machine.
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("listen %q: the AI endpoint is for apps on this machine, so it has to be a loopback address, like %s", a.Listen, DefaultAIListen)
	}
	if port == "" || port == "0" {
		return fmt.Errorf("listen %q: give a fixed port; apps keep the address across daemon restarts", a.Listen)
	}
	u, err := url.Parse(a.OllamaBase())
	if err != nil {
		return fmt.Errorf("ollama_url: %w", err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("ollama_url %q: want a scheme and host, like %s", a.OllamaURL, DefaultOllamaURL)
	}
	for key, name := range map[string]string{"llm": a.LLM, "system1": a.System1} {
		if err := validModelName(name); err != nil {
			return fmt.Errorf("%s %q: %w", key, name, err)
		}
	}
	if a.LLMProvider != "" && !slices.Contains(llmProviders, a.LLMProvider) {
		return fmt.Errorf("llm_provider %q: the LLM can come from %s", a.LLMProvider, strings.Join(llmProviders, ", "))
	}
	if a.System1Provider != "" && !slices.Contains(system1Providers, a.System1Provider) {
		return fmt.Errorf("system1_provider %q: the System 1 model can come from %s", a.System1Provider, strings.Join(system1Providers, ", "))
	}
	for key, apps := range map[string]*[]string{"llm_apps": a.LLMApps, "system1_apps": a.System1Apps} {
		if apps == nil {
			continue
		}
		for _, app := range *apps {
			if strings.TrimSpace(app) == "" || strings.ContainsFunc(app, unicode.IsControl) {
				return fmt.Errorf("%s: %q isn't an app's name", key, app)
			}
		}
	}
	for name, p := range a.Providers() {
		if p.BaseURL != "" {
			u, err := url.Parse(p.BaseURL)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("%s.base_url %q: want a scheme, host and perhaps a path, like https://api.example.com", name, p.BaseURL)
			}
		}
		if p.APIKeySecret != "" && !secretName.MatchString(p.APIKeySecret) {
			return fmt.Errorf("%s.api_key_secret %q: a secret's name is 1-64 characters of a-z, 0-9, '.', '_' and '-'", name, p.APIKeySecret)
		}
	}
	return nil
}

// secretName is secrets.ValidName's rule, which config can't import.
var secretName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// validModelName allows what Ollama's names are made of: a name, a tag and
// perhaps a registry path, and nothing that could carry a second value.
func validModelName(name string) error {
	if len(name) > 200 {
		return errors.New("too long for a model name")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-:/~@+", r) {
			return errors.New("a model name is letters, digits and . _ - : / ~ @ +, as its provider lists it")
		}
	}
	return nil
}

func (a AI) render(b *strings.Builder) {
	b.WriteString("[ai]\n")
	b.WriteString("# The models apps are given. Every app gets OUTPOST_AI_URL and its own\n")
	b.WriteString("# OUTPOST_AI_TOKEN, and asks for \"llm\" or \"system1\". The portal's AI\n")
	b.WriteString("# models page can choose them too, and wins.\n")
	optional := func(key, val, example string) {
		if val != "" {
			fmt.Fprintf(b, "%s = %q\n", key, val)
		} else {
			fmt.Fprintf(b, "# %s = %q\n", key, example)
		}
	}
	optional("llm", a.LLM, "gemma4:e4b")
	b.WriteString("# ollama (the default), openai, anthropic or openrouter.\n")
	optional("llm_provider", a.LLMProvider, "ollama")
	optional("system1", a.System1, "laya:latest")
	b.WriteString("# ollama (the default), openai, openrouter or typesafe.\n")
	optional("system1_provider", a.System1Provider, "ollama")
	b.WriteString("# The apps that may use each; unset is every app.\n")
	list := func(key string, apps *[]string) {
		if apps == nil {
			fmt.Fprintf(b, "# %s = [\"sous\", \"tasking\"]\n", key)
			return
		}
		quoted := make([]string, len(*apps))
		for i, app := range *apps {
			quoted[i] = fmt.Sprintf("%q", app)
		}
		fmt.Fprintf(b, "%s = [%s]\n", key, strings.Join(quoted, ", "))
	}
	list("llm_apps", a.LLMApps)
	list("system1_apps", a.System1Apps)
	optional("ollama_url", a.OllamaURL, DefaultOllamaURL)
	b.WriteString("# The endpoint apps call; loopback only.\n")
	optional("listen", a.Listen, DefaultAIListen)
	// A hosted provider's table only when it has a setting; the keys are
	// secrets, not here.
	for _, name := range []string{"openai", "anthropic", "openrouter", "typesafe"} {
		p := a.Providers()[name]
		if p == (AIProvider{}) {
			continue
		}
		fmt.Fprintf(b, "\n[ai.%s]\n", name)
		if p.BaseURL != "" {
			fmt.Fprintf(b, "base_url = %q\n", p.BaseURL)
		}
		if p.APIKeySecret != "" {
			fmt.Fprintf(b, "api_key_secret = %q\n", p.APIKeySecret)
		}
	}
}

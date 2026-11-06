package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode"
)

// Defaults for [ai].
const (
	DefaultAIListen  = "127.0.0.1:47901"
	DefaultOllamaURL = "http://127.0.0.1:11434"
)

// AI is the models Outpost's apps are given: one LLM and one System 1 model,
// both served by Ollama. The portal's AI models page can choose them too, and
// its choice wins over these. See docs/design/ai-models.md.
type AI struct {
	// Listen is the loopback address of the endpoint apps call, which they
	// are told as OUTPOST_AI_URL.
	Listen string `toml:"listen"`
	// OllamaURL is where Ollama's API is.
	OllamaURL string `toml:"ollama_url"`
	// LLM and System1 are model names as Ollama lists them, such as
	// "gemma4:e4b" and "laya:latest". Unset means none until the portal
	// chooses one.
	LLM     string `toml:"llm"`
	System1 string `toml:"system1"`
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
	return nil
}

// validModelName allows what Ollama's names are made of: a name, a tag and
// perhaps a registry path, and nothing that could carry a second value.
func validModelName(name string) error {
	if len(name) > 200 {
		return errors.New("too long for a model name")
	}
	for _, r := range name {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("._-:/", r) {
			return errors.New("a model name is letters, digits and . _ - : /, as `ollama list` shows it")
		}
	}
	return nil
}

func (a AI) render(b *strings.Builder) {
	b.WriteString("[ai]\n")
	b.WriteString("# The models apps are given, both served by Ollama. Every app gets\n")
	b.WriteString("# OUTPOST_AI_URL and its own OUTPOST_AI_TOKEN, and asks for \"llm\" or\n")
	b.WriteString("# \"system1\". The portal's AI models page can choose them too, and wins.\n")
	optional := func(key, val, example string) {
		if val != "" {
			fmt.Fprintf(b, "%s = %q\n", key, val)
		} else {
			fmt.Fprintf(b, "# %s = %q\n", key, example)
		}
	}
	optional("llm", a.LLM, "gemma4:e4b")
	optional("system1", a.System1, "laya:latest")
	optional("ollama_url", a.OllamaURL, DefaultOllamaURL)
	b.WriteString("# The endpoint apps call; loopback only.\n")
	optional("listen", a.Listen, DefaultAIListen)
}

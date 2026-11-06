package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/ai"
)

// aiClient is a daemon with AI models as well as users and a gateway.
type aiClient struct {
	*extrasClient
	overview ai.Overview
	log      []string
}

func (c *aiClient) AIStatus() (ai.Overview, error) { return c.overview, nil }

func (c *aiClient) AISetFrom(kind, provider, model string) (string, error) {
	c.log = append(c.log, "set "+kind+" "+provider+" "+model)
	if provider == ai.ProviderAnthropic {
		return "", errors.New("add Anthropic's API key first")
	}
	return "The " + ai.KindName(kind) + " is now " + model + ".", nil
}

func (c *aiClient) AIReset(kind string) (string, error) {
	c.log = append(c.log, "reset "+kind)
	return "back to config.toml's", nil
}

func (c *aiClient) AIApps(kind string, every bool, apps []string) (string, error) {
	w := strings.Join(apps, ",")
	if every {
		w = "every"
	}
	c.log = append(c.log, "apps "+kind+" "+w)
	return "apps saved", nil
}

func (c *aiClient) AIAppsReset(kind string) (string, error) {
	c.log = append(c.log, "appsreset "+kind)
	return "apps back", nil
}

func (c *aiClient) AIKey(provider, value string) (string, error) {
	c.log = append(c.log, "key "+provider+" "+value)
	return "Saved " + ai.ProviderTitle(provider) + "'s API key.", nil
}

func (c *aiClient) AITest(kind string) (ai.TestResult, error) {
	return ai.TestResult{Kind: kind, Model: "gemma4:e4b", Reply: "ready", Took: 300 * time.Millisecond}, nil
}

func newAIClient() *aiClient {
	return &aiClient{extrasClient: newExtras(), overview: ai.Overview{
		Choices: []ai.Choice{
			{Kind: ai.LLM, Provider: ai.ProviderOllama, Model: "gemma4:e4b", FromPortal: true, UpdatedBy: "the CLI", Every: true},
			{Kind: ai.System1, Provider: ai.ProviderOpenAI, Model: "gpt-6-luna", Apps: []string{"notes"}, AppsFromPortal: true, NoKey: true},
		},
		Providers: []ai.ProviderStatus{
			{Name: "ollama", Title: "Ollama", Kinds: []string{"llm", "system1"}},
			{Name: "openai", Title: "OpenAI", Kinds: []string{"llm", "system1"}, Keyed: true, KeySecret: "openai-api-key"},
			{Name: "anthropic", Title: "Anthropic", Kinds: []string{"llm"}, Keyed: true, KeySecret: "anthropic-api-key"},
			{Name: "openrouter", Title: "OpenRouter", Kinds: []string{"llm", "system1"}, Keyed: true, HasKey: true, KeySecret: "openrouter-api-key"},
		},
		Catalogs: []ai.Catalog{
			{Provider: "ollama", Kind: "llm", Complete: true, Models: []ai.Model{{Name: "gemma4:e4b"}, {Name: "qwen3:4b"}}},
			{Provider: "ollama", Kind: "system1", Complete: true, Models: []ai.Model{{Name: "laya:latest"}}},
			{Provider: "openai", Kind: "llm", Error: "add OpenAI's API key to list its models"},
			{Provider: "anthropic", Kind: "llm", Error: "add Anthropic's API key to list its models"},
			{Provider: "openrouter", Kind: "llm", Complete: true, Models: []ai.Model{
				{Name: "anthropic/claude-haiku-5-5", Note: "$1.00 per million input tokens"}, {Name: "meta/llama-free:free"}}},
			{Provider: "openrouter", Kind: "system1", Complete: true, Models: []ai.Model{{Name: "typesafe/jev-1.14"}}},
			{Provider: "typesafe", Kind: "system1", Models: []ai.Model{{Name: "jev-latest"}}},
		},
		Usage: []ai.Usage{{Service: "notes", Kind: "llm", Requests: 3, InputTokens: 40, OutputTokens: 9}},
	}}
}

// aiView is a dashboard on the AI view, loaded.
func aiView(t *testing.T, c *aiClient) Model {
	t.Helper()
	m := New(c, nil)
	m.width, m.height = 100, 40
	m = drain(t, m, tea.Batch(m.fetchServices(), m.fetchGateway()))
	for range 3 {
		var cmd tea.Cmd
		m, cmd = run(t, m, tea.KeyMsg{Type: tea.KeyTab})
		m = drain(t, m, cmd)
	}
	if m.view != viewAI {
		t.Fatalf("three tabs reached view %d, not AI", m.view)
	}
	return m
}

func press(t *testing.T, m Model, keys ...string) Model {
	t.Helper()
	for _, k := range keys {
		var msg tea.KeyMsg
		switch k {
		case "enter":
			msg = tea.KeyMsg{Type: tea.KeyEnter}
		case "esc":
			msg = tea.KeyMsg{Type: tea.KeyEsc}
		case "down":
			msg = tea.KeyMsg{Type: tea.KeyDown}
		case "up":
			msg = tea.KeyMsg{Type: tea.KeyUp}
		case "space":
			msg = tea.KeyMsg{Type: tea.KeySpace}
		case "backspace":
			msg = tea.KeyMsg{Type: tea.KeyBackspace}
		default:
			msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
		}
		var cmd tea.Cmd
		m, cmd = run(t, m, msg)
		m = drain(t, m, cmd)
	}
	return m
}

func TestAI_ViewShowsModelsProvidersAndUsage(t *testing.T) {
	m := aiView(t, newAIClient())
	v := m.View()
	for _, want := range []string{
		"▸ AI", "▸ LLM", "gemma4:e4b", "Ollama · chosen by the CLI · every app",
		"gpt-6-luna", "OpenAI · config.toml · only notes", "no API key for OpenAI",
		"providers", "OpenAI", "no key", "OpenRouter", "key set", "secret openrouter-api-key",
		"notes", "3 requests",
		"enter choose", "a apps", "t test",
	} {
		if !strings.Contains(v, want) {
			t.Errorf("AI view lacks %q:\n%s", want, v)
		}
	}
	if strings.Contains(v, "secret ollama") {
		t.Error("Ollama is offered a key")
	}
}

func TestAI_ViewOnlyWithAIExtras(t *testing.T) {
	m := loadedExtras(t, newExtras())
	if strings.Contains(m.View(), "AI") {
		t.Error("a client without AIExtras is shown an AI tab")
	}
	for range 3 {
		var cmd tea.Cmd
		m, cmd = run(t, m, tea.KeyMsg{Type: tea.KeyTab})
		m = drain(t, m, cmd)
	}
	if m.view != viewServices {
		t.Errorf("three tabs without AIExtras landed on view %d, not back on services", m.view)
	}
}

func TestAI_PickAModelByTyping(t *testing.T) {
	c := newAIClient()
	m := aiView(t, c)
	m = press(t, m, "enter")
	if !strings.Contains(m.View(), "choose the LLM") || !strings.Contains(m.View(), "not listed: OpenAI (no key), Anthropic (no key)") {
		t.Fatalf("picker:\n%s", m.View())
	}
	// q and esc are letters and cancel here, not quit.
	m = press(t, m, "q", "w", "e", "n")
	if m.quitting {
		t.Fatal("typing q in the filter quit the dashboard")
	}
	if v := m.View(); !strings.Contains(v, "> qwen") || !strings.Contains(v, "qwen3:4b") || strings.Contains(v, "gemma4") {
		t.Errorf("filtered:\n%s", v)
	}
	m = press(t, m, "enter")
	if got := strings.Join(c.log, "|"); got != "set llm ollama qwen3:4b" {
		t.Errorf("calls = %s", got)
	}
	if !strings.Contains(m.View(), "The LLM is now qwen3:4b.") || m.ai.mode != aiBrowse {
		t.Errorf("after choosing:\n%s", m.View())
	}

	// OpenRouter's, found by provider name.
	m = press(t, m, "enter", "h", "a", "i", "k", "u", "enter")
	if got := c.log[len(c.log)-1]; got != "set llm openrouter anthropic/claude-haiku-5-5" {
		t.Errorf("last call = %s", got)
	}

	m = press(t, m, "enter", "esc")
	if m.ai.mode != aiBrowse || m.quitting {
		t.Error("esc didn't close the picker, or quit")
	}
}

func TestAI_PickAnUnlistedModelFromAProviderWithoutAList(t *testing.T) {
	c := newAIClient()
	m := aiView(t, c)
	m = press(t, m, "down", "enter")
	for _, r := range "jev-1.14.0" {
		m = press(t, m, string(r))
	}
	if !strings.Contains(m.View(), "use jev-1.14.0 as typed") {
		t.Fatalf("no custom entry:\n%s", m.View())
	}
	press(t, m, "enter")
	if got := c.log[len(c.log)-1]; got != "set system1 typesafe jev-1.14.0" {
		t.Errorf("last call = %s", got)
	}
}

func TestAI_ChooseApps(t *testing.T) {
	c := newAIClient()
	m := aiView(t, c)
	// The LLM is every app's: turn that off and give it to wiki alone.
	m = press(t, m, "a")
	if v := m.View(); !strings.Contains(v, "who may use the LLM") || !strings.Contains(v, "[x] every app") {
		t.Fatalf("apps:\n%s", v)
	}
	m = press(t, m, "space", "down", "down", "space", "enter")
	if got := strings.Join(c.log, "|"); got != "apps llm wiki" {
		t.Errorf("calls = %s", got)
	}
	// System 1 has apps set here, so u puts them back.
	m = press(t, m, "down", "u")
	if got := c.log[len(c.log)-1]; got != "appsreset system1" {
		t.Errorf("last call = %s", got)
	}
	// The LLM's model is set here, so u puts that back first.
	press(t, m, "up", "u")
	if got := c.log[len(c.log)-1]; got != "reset llm" {
		t.Errorf("last call = %s", got)
	}
}

func TestAI_SetAKeyWithoutShowingIt(t *testing.T) {
	c := newAIClient()
	m := aiView(t, c)
	m = press(t, m, "down", "down", "enter")
	if !strings.Contains(m.View(), "OpenAI's API key") {
		t.Fatalf("key entry:\n%s", m.View())
	}
	m = press(t, m, "sk-proj-secret", "q")
	v := m.View()
	if strings.Contains(v, "sk-proj") || !strings.Contains(v, "•••••") || m.quitting {
		t.Errorf("the key is on screen, or q quit:\n%s", v)
	}
	m = press(t, m, "enter")
	if got := c.log[len(c.log)-1]; got != "key openai sk-proj-secretq" {
		t.Errorf("last call = %s", got)
	}
	if m.ai.input.Value() != "" {
		t.Error("the key was kept in the field after saving")
	}
	if !strings.Contains(m.View(), "Saved OpenAI's API key.") {
		t.Errorf("after saving:\n%s", m.View())
	}

	// Esc throws a half-typed key away.
	m = press(t, m, "enter", "sk-half", "esc")
	if m.ai.input.Value() != "" || strings.Contains(c.log[len(c.log)-1], "sk-half") {
		t.Error("a cancelled key was kept or sent")
	}
}

func TestAI_TestAndErrors(t *testing.T) {
	c := newAIClient()
	m := aiView(t, c)
	m = press(t, m, "t")
	if !strings.Contains(m.View(), "gemma4:e4b answered in 300ms: ready") {
		t.Errorf("after t:\n%s", m.View())
	}
	// Anthropic's own Claude isn't listed without a key; OpenRouter's is.
	m = press(t, m, "enter", "c", "l", "a", "u", "d", "e")
	if v := m.View(); !strings.Contains(v, "anthropic/claude-haiku-5-5") || !strings.Contains(v, "Anthropic (no key)") {
		t.Errorf("picker:\n%s", v)
	}
	m = press(t, m, "z", "z", "z")
	if !strings.Contains(m.View(), "nothing matches") {
		t.Errorf("picker:\n%s", m.View())
	}
	m = press(t, m, "esc")
	// The services view's keys do nothing here.
	before := len(c.calls)
	m = press(t, m, "s", "x", "r")
	if len(c.calls) != before {
		t.Errorf("a services action ran from the AI view: %v", c.calls[before:])
	}
	_ = m
}

func TestAI_FitsEightyColumns(t *testing.T) {
	m := aiView(t, newAIClient())
	m.width, m.help.Width = 80, 80
	for _, keys := range [][]string{nil, {"enter"}, {"esc", "a"}, {"esc", "down", "down", "enter"}} {
		m = press(t, m, keys...)
		for _, line := range strings.Split(m.View(), "\n") {
			if w := lipgloss.Width(line); w > 80 {
				t.Errorf("after %v: %d cells: %q", keys, w, line)
			}
		}
	}
}

func TestField(t *testing.T) {
	f := newInput("hint", true)
	f = f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("ab\ncd")})
	if f.Value() != "abcd" {
		t.Errorf("value = %q; a pasted newline should be dropped", f.Value())
	}
	f = f.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if f.Value() != "abc" || strings.Contains(f.View(), "abc") {
		t.Errorf("value %q, view %q", f.Value(), f.View())
	}
	f = f.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	if f.Value() != "" {
		t.Error("ctrl+u didn't clear it")
	}
}

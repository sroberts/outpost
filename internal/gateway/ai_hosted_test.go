package gateway

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeHostedAI is fakeAI with the hosted providers and app access too.
type fakeHostedAI struct {
	*fakeAI
	hmu     sync.Mutex
	setLog  []string
	apps    []string
	keys    []string
	hasKey  map[string]bool
	choices map[string]ai.Choice
}

func newFakeHostedAI() *fakeHostedAI {
	return &fakeHostedAI{fakeAI: newFakeAI(), hasKey: map[string]bool{ai.ProviderOpenRouter: true, ai.ProviderTypeSafe: true}, choices: map[string]ai.Choice{
		ai.LLM: {Kind: ai.LLM, Provider: ai.ProviderOpenAI, Model: "gpt-6", FromPortal: true, UpdatedBy: "root",
			File: "gemma4:e4b", FileProvider: ai.ProviderOllama, APIs: []string{"/openai/v1/chat/completions"},
			Every: false, Apps: []string{"sous"}, AppsFromPortal: true, NoKey: true},
		ai.System1: {Kind: ai.System1, Provider: ai.ProviderOllama, Model: "laya:latest", Every: true,
			APIs: []string{"/systemone/v1/systemone"}},
	}}
}

func (f *fakeHostedAI) Overview(ctx context.Context) ai.Overview {
	o := f.fakeAI.Overview(ctx)
	f.hmu.Lock()
	defer f.hmu.Unlock()
	o.Choices = []ai.Choice{f.choices[ai.LLM], f.choices[ai.System1]}
	for _, name := range ai.ProviderNames {
		p := ai.ProviderStatus{Name: name, Title: ai.ProviderTitle(name), Keyed: name != ai.ProviderOllama,
			KeySecret: name + "-api-key", HasKey: f.hasKey[name], BaseURL: "https://" + name + ".example"}
		for _, k := range ai.Kinds {
			if ai.ProviderServes(name, k) {
				p.Kinds = append(p.Kinds, k)
			}
		}
		o.Providers = append(o.Providers, p)
	}
	o.Catalogs = []ai.Catalog{
		{Provider: ai.ProviderOpenAI, Kind: ai.LLM, Error: "add OpenAI's API key to list its models"},
		{Provider: ai.ProviderAnthropic, Kind: ai.LLM, Error: "add Anthropic's API key to list its models"},
		{Provider: ai.ProviderOpenRouter, Kind: ai.LLM, Complete: true, Models: []ai.Model{
			{Name: "anthropic/claude-haiku-5-5", Note: "Anthropic: Claude Haiku 5.5 · $1.00 per million input tokens"}}},
		{Provider: ai.ProviderOpenAI, Kind: ai.System1, Models: []ai.Model{{Name: "gpt-6-luna", Note: "OpenAI's decision model"}}},
		{Provider: ai.ProviderOpenRouter, Kind: ai.System1, Complete: true, Models: []ai.Model{{Name: "typesafe/jev-1.14"}}},
		{Provider: ai.ProviderTypeSafe, Kind: ai.System1, Models: []ai.Model{{Name: "jev-latest"}}},
	}
	return o
}

func (f *fakeHostedAI) SetModel(_ context.Context, kind, provider, model, by string) (string, error) {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	if provider == ai.ProviderAnthropic && !f.hasKey[provider] {
		return "", errors.New("add Anthropic's API key first")
	}
	f.setLog = append(f.setLog, kind+" "+provider+" "+model+" by "+by)
	c := f.choices[kind]
	c.Provider, c.Model = provider, model
	f.choices[kind] = c
	return model, nil
}

func (f *fakeHostedAI) SetApps(kind string, every bool, apps []string, by string) error {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	w := "every"
	if !every {
		w = strings.Join(apps, ",")
	}
	f.apps = append(f.apps, kind+" "+w+" by "+by)
	return nil
}

func (f *fakeHostedAI) ResetApps(kind, by string) error {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	f.apps = append(f.apps, kind+" reset by "+by)
	return nil
}

func (f *fakeHostedAI) SetKey(provider, value, by string) error {
	f.hmu.Lock()
	defer f.hmu.Unlock()
	if strings.TrimSpace(value) == "" {
		return errors.New("paste " + ai.ProviderTitle(provider) + "'s API key")
	}
	f.keys = append(f.keys, provider+" by "+by)
	f.hasKey[provider] = true
	return nil
}

func hostedHarness(t *testing.T) (*harness, *fakeHostedAI) {
	h := newHarness(t)
	f := newFakeHostedAI()
	h.g.ai = f
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Status: "running"},
		supervisor.ServiceStatus{Name: "tasking", Status: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	return h, f
}

func TestAIPage_Hosted_ShowsProvidersAccessAndWarnings(t *testing.T) {
	h, _ := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/ai"))
	for _, want := range []string{
		`<code>gpt-6</code><span class="tag">OpenAI</span>`,
		"Chosen here by root",
		"from OpenAI.",
		"Outpost has no API key for OpenAI",
		"OpenAI doesn&#39;t take /anthropic/v1/messages",
		"Only Sous may use it, as chosen here.",
		"Every app may use it.",
		"Add OpenAI's API key under Providers",
		`list="dl-llm-openrouter"`,
		`<option value="anthropic/claude-haiku-5-5">Anthropic: Claude Haiku 5.5`,
		"type to search its 1 models",
		`<option value="jev-latest">`,
		"one of these, or any model ID it serves",
		`id="p-openrouter"`, `<span class="tag">Key set</span>`,
		"secret <code>typesafe-api-key</code>",
		`name="app" value="sous" checked`,
		"Use config.toml's apps",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// TypeSafe serves no LLM, and Anthropic no System 1.
	llm := b[strings.Index(b, `id="k-llm"`):strings.Index(b, `id="k-system1"`)]
	if strings.Contains(llm, "dl-llm-typesafe") {
		t.Error("TypeSafe is offered for the LLM")
	}
	sys := b[strings.Index(b, `id="k-system1"`):strings.Index(b, `id="providers"`)]
	if strings.Contains(sys, "dl-system1-anthropic") || strings.Contains(sys, "<h3>Anthropic</h3>") {
		t.Error("Anthropic is offered for System 1")
	}
	if strings.Contains(b, `id="p-ollama"`) {
		t.Error("Ollama is offered a key")
	}
}

func TestAIPage_Hosted_SavesModelsAppsAndKeys(t *testing.T) {
	h, f := hostedHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	fd := h.recording()

	b := body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"save"}, "kind": {"system1"}, "provider": {"openrouter"}, "model": {"typesafe/jev-1.14"}}))
	if !strings.Contains(b, "Saved.") {
		t.Errorf("after saving:\n%s", b)
	}
	resp := h.post(c, portal+"/ai", portal, url.Values{"action": {"save"}, "kind": {"llm"}, "provider": {"anthropic"}, "model": {"claude-haiku-5-5"}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "Add Anthropic&#39;s API key first") {
		t.Errorf("a model from a provider with no key: %d", resp.StatusCode)
	}
	// Ollama's radios name their provider, so a hosted choice can go back to them.
	h.post(c, portal+"/ai", portal, url.Values{"action": {"save"}, "kind": {"llm"}, "provider": {"ollama"}, "model": {"qwen3:4b"}})
	if got := strings.Join(f.setLog, "|"); got != "system1 openrouter typesafe/jev-1.14 by root|llm ollama qwen3:4b by root" {
		t.Errorf("set = %s", got)
	}

	b = body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"apps"}, "kind": {"llm"}, "app": {"sous", "tasking"}}))
	if !strings.Contains(b, "Saved who may use it.") {
		t.Errorf("after apps:\n%s", b)
	}
	h.post(c, portal+"/ai", portal, url.Values{"action": {"apps"}, "kind": {"system1"}, "every": {"1"}})
	h.post(c, portal+"/ai", portal, url.Values{"action": {"appsreset"}, "kind": {"llm"}})
	if got := strings.Join(f.apps, "|"); got != "llm sous,tasking by root|system1 every by root|llm reset by root" {
		t.Errorf("apps = %s", got)
	}

	b = body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"key"}, "provider": {"anthropic"}, "key": {"sk-ant-secret"}}))
	if !strings.Contains(b, "Key saved.") || strings.Contains(b, "sk-ant-secret") {
		t.Errorf("after the key:\n%s", b)
	}
	resp = h.post(c, portal+"/ai", portal, url.Values{"action": {"key"}, "provider": {"openai"}, "key": {" "}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "Paste OpenAI&#39;s API key") {
		t.Errorf("an empty key: %d", resp.StatusCode)
	}
	if got := strings.Join(f.keys, "|"); got != "anthropic by root" {
		t.Errorf("keys = %s", got)
	}
	if resp := h.post(c, portal+"/ai", portal, url.Values{"action": {"key"}, "provider": {"nope"}, "key": {"x"}}); resp.StatusCode != 400 {
		t.Errorf("an unknown provider: %d", resp.StatusCode)
	}

	var lines []string
	for _, e := range fd.wait(t, 6) {
		lines = append(lines, e.kind+": "+e.message)
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"ai: root set the System 1 to OpenRouter's typesafe/jev-1.14",
		"ai: root gave the LLM to only sous and tasking",
		"ai: root gave the System 1 to every app",
		"ai: root set Anthropic's API key",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("activity lacks %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "sk-ant-secret") {
		t.Errorf("a key reached the activity feed:\n%s", all)
	}
}

func TestAIPage_Hosted_OnlyAnAdminSetsAKey(t *testing.T) {
	h, f := hostedHarness(t)
	h.addUser(auth.User{Username: "alice"})
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if resp := h.post(plain, portal+"/ai", portal, url.Values{"action": {"key"}, "provider": {"openai"}, "key": {"sk"}}); resp.StatusCode != 403 {
		t.Errorf("a plain user setting a key = %d", resp.StatusCode)
	}
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/ai", "http://evil.test", url.Values{"action": {"key"}, "provider": {"openai"}, "key": {"sk"}}); resp.StatusCode != 403 {
		t.Errorf("a key from another site = %d", resp.StatusCode)
	}
	if len(f.keys) != 0 {
		t.Errorf("a refused key was saved: %v", f.keys)
	}
}

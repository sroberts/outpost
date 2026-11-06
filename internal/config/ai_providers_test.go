package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAI_ProvidersAndAppsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `[ai]
llm = "claude-haiku-5-5"
llm_provider = "anthropic"
llm_apps = ["sous", "tasking"]
system1 = "~liquid/d1-latest"
system1_provider = "openrouter"
system1_apps = []

[ai.openrouter]
api_key_secret = "or-key"

[ai.openai]
base_url = "https://proxy.example/openai"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.AI
	if a.LLMProvider != "anthropic" || a.System1Provider != "openrouter" || a.System1 != "~liquid/d1-latest" {
		t.Errorf("AI = %+v", a)
	}
	if a.LLMApps == nil || strings.Join(*a.LLMApps, ",") != "sous,tasking" {
		t.Errorf("llm_apps = %v", a.LLMApps)
	}
	if a.System1Apps == nil || len(*a.System1Apps) != 0 {
		t.Errorf("system1_apps = %v; want set and empty, which is no app", a.System1Apps)
	}
	if a.OpenRouter.APIKeySecret != "or-key" || a.OpenAI.BaseURL != "https://proxy.example/openai" {
		t.Errorf("providers = %+v", a.Providers())
	}

	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	b := again.AI
	if b.LLMProvider != a.LLMProvider || b.System1 != a.System1 || b.LLMApps == nil || strings.Join(*b.LLMApps, ",") != "sous,tasking" ||
		b.System1Apps == nil || len(*b.System1Apps) != 0 || b.OpenRouter != a.OpenRouter || b.OpenAI != a.OpenAI {
		t.Errorf("after Save, AI = %+v", b)
	}
	if cfg.Fingerprint() == Defaults().Fingerprint() {
		t.Error("hosted providers in [ai] didn't change the fingerprint")
	}
}

func TestAI_ValidateProviders(t *testing.T) {
	none := []string{}
	ok := []AI{
		{LLMProvider: "openai", System1Provider: "openai"},
		{LLMProvider: "openrouter", System1Provider: "typesafe", System1Apps: &none},
		{Anthropic: AIProvider{BaseURL: "http://127.0.0.1:8080/anthropic", APIKeySecret: "claude.key"}},
	}
	for _, a := range ok {
		if err := a.validate(); err != nil {
			t.Errorf("%+v: %v", a, err)
		}
	}
	blank := []string{" "}
	bad := map[string]AI{
		"llm_provider":     {LLMProvider: "typesafe"},
		"system1_provider": {System1Provider: "anthropic"},
		"llm_apps":         {LLMApps: &blank},
		"openai.base_url":  {OpenAI: AIProvider{BaseURL: "api.openai.com"}},
		"api_key_secret":   {TypeSafe: AIProvider{APIKeySecret: "Has Spaces"}},
	}
	for want, a := range bad {
		err := a.validate()
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v; want an error with %q", a, err, want)
		}
	}
}

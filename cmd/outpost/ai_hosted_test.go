package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/secrets"
)

func TestSetArgs(t *testing.T) {
	for _, c := range []struct {
		args            []string
		model, provider string
	}{
		{[]string{"gemma4:e4b"}, "gemma4:e4b", ""},
		{[]string{"claude-haiku-5-5", "--provider", "anthropic"}, "claude-haiku-5-5", "anthropic"},
		{[]string{"--provider=openrouter", "anthropic/claude-haiku-5-5"}, "anthropic/claude-haiku-5-5", "openrouter"},
	} {
		model, provider, err := setArgs(ai.LLM, c.args)
		if err != nil || model != c.model || provider != c.provider {
			t.Errorf("setArgs(%v) = %q, %q, %v", c.args, model, provider, err)
		}
	}
	for _, args := range [][]string{{}, {"--provider"}, {"a", "b"}, {"--verbose", "a"}} {
		if _, _, err := setArgs(ai.LLM, args); err == nil {
			t.Errorf("setArgs(%v) took it", args)
		}
	}
	if _, _, err := setArgs(ai.LLM, []string{"jev-latest", "--provider", "typesafe"}); err == nil ||
		!strings.Contains(err.Error(), "the LLM can come from ollama, openai, anthropic, openrouter") {
		t.Errorf("TypeSafe for the LLM = %v", err)
	}
}

func TestAppsArgs(t *testing.T) {
	if every, reset, apps, err := appsArgs(ai.LLM, []string{"--all"}); err != nil || !every || reset || apps != nil {
		t.Errorf("--all = %v %v %v %v", every, reset, apps, err)
	}
	if every, reset, apps, err := appsArgs(ai.LLM, []string{"--none"}); err != nil || every || reset || len(apps) != 0 {
		t.Errorf("--none = %v %v %v %v", every, reset, apps, err)
	}
	if _, reset, _, err := appsArgs(ai.LLM, []string{"--reset"}); err != nil || !reset {
		t.Errorf("--reset = %v %v", reset, err)
	}
	if _, _, apps, err := appsArgs(ai.LLM, []string{"sous", "tasking"}); err != nil || strings.Join(apps, ",") != "sous,tasking" {
		t.Errorf("names = %v %v", apps, err)
	}
	for _, args := range [][]string{{}, {"--all", "sous"}, {"sous", "--all"}} {
		if _, _, _, err := appsArgs(ai.LLM, args); err == nil {
			t.Errorf("appsArgs(%v) took it", args)
		}
	}
}

func TestCmdAI_KeyAndAppsRefuseBadArguments(t *testing.T) {
	a := &app{home: t.TempDir(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	for _, args := range [][]string{
		{"key"}, {"key", "ollama"}, {"key", "nope"}, {"key", "openai", "sk-on-the-command-line"},
		{"apps"}, {"apps", "llm"}, {"set", "llm", "x", "--provider", "typesafe"},
	} {
		if err := a.cmdAI(args); err == nil || strings.Contains(err.Error(), "daemon") {
			t.Errorf("ai %v = %v; want a usage error", args, err)
		}
	}
}

func TestAIOptions_FromConfig(t *testing.T) {
	none := []string{}
	some := []string{"sous"}
	o := aiOptions(config.AI{
		LLM: "claude-haiku-5-5", LLMProvider: "anthropic", LLMApps: &some,
		System1: "laya", System1Apps: &none,
		OpenRouter: config.AIProvider{APIKeySecret: "or-key", BaseURL: "https://or.example/api"},
	}, "/x/ai.db", nil, nil)
	if o.Listen != config.DefaultAIListen || o.OllamaURL != config.DefaultOllamaURL || o.Path != "/x/ai.db" {
		t.Errorf("defaults = %+v", o)
	}
	if o.LLMProvider != "anthropic" || strings.Join(o.LLMApps, ",") != "sous" {
		t.Errorf("LLM = %q %v", o.LLMProvider, o.LLMApps)
	}
	if o.System1Apps == nil || len(o.System1Apps) != 0 {
		t.Errorf("system1_apps = [] should reach the service as no app, not every app: %#v", o.System1Apps)
	}
	if p := o.Providers["openrouter"]; p.KeySecret != "or-key" || p.BaseURL != "https://or.example/api" {
		t.Errorf("OpenRouter = %+v", p)
	}
	if o2 := aiOptions(config.AI{}, "p", nil, nil); o2.LLMApps != nil || o2.System1Apps != nil {
		t.Error("unset apps should be every app (nil)")
	}
}

func TestSecretKeys_KeepAProviderKeyUnbound(t *testing.T) {
	dir := t.TempDir()
	st, err := secrets.Open(filepath.Join(dir, "secrets.db"), filepath.Join(dir, "secrets.key"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	k := secretKeys{st}
	if err := k.Put("openai-api-key", "sk-1"); err != nil {
		t.Fatal(err)
	}
	if err := k.Put("openai-api-key", "sk-2"); err != nil {
		t.Fatal(err)
	}
	if v, ok := k.Value("openai-api-key"); !ok || v != "sk-2" {
		t.Errorf("Value = %q, %v", v, ok)
	}
	sec, _ := st.Get("openai-api-key")
	if sec.Env != "OPENAI_API_KEY" || sec.Global || len(sec.Services) != 0 || sec.Revision != 2 {
		t.Errorf("secret = %+v", sec)
	}
	if err := k.Put("openai-api-key", "bad\x00key"); err == nil {
		t.Error("a key with a NUL was taken")
	}
}

func TestWriteAIStatus_Hosted(t *testing.T) {
	var b bytes.Buffer
	writeAIStatus(&b, ai.Overview{
		URL: "http://127.0.0.1:47901", OllamaURL: "http://127.0.0.1:11434",
		Choices: []ai.Choice{
			{Kind: ai.LLM, Provider: ai.ProviderOpenAI, Model: "gpt-6", FromPortal: true, UpdatedBy: "root",
				NoKey: true, Apps: []string{"sous"}, APIs: []string{"/openai/v1/chat/completions"}},
			{Kind: ai.System1, Provider: ai.ProviderOpenRouter, Model: "liquid/d1", Every: true, Missing: true},
		},
		Providers: []ai.ProviderStatus{
			{Name: "ollama", Kinds: []string{"llm", "system1"}},
			{Name: "openai", Kinds: []string{"llm", "system1"}, Keyed: true, KeySecret: "openai-api-key"},
			{Name: "openrouter", Kinds: []string{"llm", "system1"}, Keyed: true, HasKey: true, KeySecret: "openrouter-api-key"},
		},
		Catalogs: []ai.Catalog{
			{Provider: "openai", Kind: "llm", Error: "add OpenAI's API key to list its models"},
			{Provider: "openrouter", Kind: "llm", Models: make([]ai.Model, 380)},
			{Provider: "openrouter", Kind: "system1", Models: make([]ai.Model, 29)},
		},
	})
	out := b.String()
	for _, want := range []string{
		"gpt-6", "(OpenAI; chosen by root; NO API KEY; only sous; apps must call /openai/v1/chat/completions)",
		"liquid/d1", "(OpenRouter; from config.toml; NOT LISTED BY OPENROUTER)",
		"none: `outpost ai key openai`", "set (secret openrouter-api-key)", "llm: 380; system1: 29",
		"llm: add OpenAI's API key to list its models",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

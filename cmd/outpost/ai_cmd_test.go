package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ai"
)

type fixedSecrets struct {
	vars map[string]string
	rev  string
}

func (f fixedSecrets) Resolve(string) (map[string]string, string) { return f.vars, f.rev }

func TestWithAI_AddsTheEndpointToEveryAppsSecrets(t *testing.T) {
	svc, err := ai.Open(ai.Options{
		Path: filepath.Join(t.TempDir(), "ai.db"), OllamaURL: "http://127.0.0.1:1", Listen: "127.0.0.1:47901",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = svc.Close() }()
	w := withAI{secrets: fixedSecrets{vars: map[string]string{"OPENAI_KEY": "k"}, rev: "s7"}, ai: svc}

	vars, rev := w.Resolve("sous")
	if vars["OPENAI_KEY"] != "k" || vars[ai.EnvURL] != "http://127.0.0.1:47901" || !strings.HasPrefix(vars[ai.EnvToken], "oai_") {
		t.Errorf("vars = %v", vars)
	}
	if !strings.HasPrefix(rev, "s7|") {
		t.Errorf("rev = %q; want the secrets' revision kept", rev)
	}

	// No secrets at all still gets the endpoint.
	bare := withAI{secrets: fixedSecrets{}, ai: svc}
	if vars, _ := bare.Resolve("yarr"); vars[ai.EnvToken] == "" {
		t.Error("an app with no secrets got no AI token")
	}

	// A new token is a new revision, which is what restarts the app.
	if err := svc.Regenerate("sous", "test"); err != nil {
		t.Fatal(err)
	}
	if _, rev2 := w.Resolve("sous"); rev2 == rev {
		t.Error("regenerating the token left the revision as it was")
	}
}

func TestWriteAIStatus(t *testing.T) {
	var b bytes.Buffer
	writeAIStatus(&b, ai.Overview{
		URL: "http://127.0.0.1:47901", OllamaURL: "http://127.0.0.1:11434",
		Choices: []ai.Choice{
			{Kind: ai.LLM, Model: "qwen3:4b", FromPortal: true, UpdatedBy: "root", File: "gemma4:e4b", Missing: true},
			{Kind: ai.System1},
		},
		Models: []ai.Model{{Name: "gemma4:e4b", ParameterSize: "8.0B", Capabilities: []string{"completion", "vision"}}},
		Usage:  []ai.Usage{{Service: "sous", Kind: ai.LLM, Requests: 4, Errors: 1, LastAt: time.Now(), LastError: "boom"}},
	})
	out := b.String()
	for _, want := range []string{
		"LLM:", "qwen3:4b", "chosen by root, over config.toml's gemma4:e4b; NOT IN OLLAMA",
		"System 1:", "none chosen",
		"gemma4:e4b  llm", "completion, vision",
		"sous", "boom",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	writeAIStatus(&b, ai.Overview{ModelsError: "can't reach Ollama", Choices: []ai.Choice{{Kind: ai.LLM}}})
	if !strings.Contains(b.String(), "Can't list Ollama's models: can't reach Ollama") {
		t.Errorf("unreachable:\n%s", b.String())
	}
}

func TestCmdAI_RefusesBadArgumentsBeforeAskingTheDaemon(t *testing.T) {
	a := &app{home: t.TempDir(), out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	for _, args := range [][]string{
		{"set"}, {"set", "embeddings", "x"}, {"set", "llm"}, {"reset", "llm2"}, {"test"},
		{"regenerate"}, {"status", "extra"}, {"frobnicate"},
	} {
		err := a.cmdAI(args)
		if err == nil || strings.Contains(err.Error(), "daemon") {
			t.Errorf("ai %v = %v; want a usage error", args, err)
		}
	}
}

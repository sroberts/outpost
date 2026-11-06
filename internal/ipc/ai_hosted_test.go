package ipc

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/gateway"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeHostedModels is fakeAIModels with hosted providers and app access.
type fakeHostedModels struct{ *fakeAIModels }

func (f fakeHostedModels) SetModel(_ context.Context, kind, provider, model, by string) (string, error) {
	f.note("setmodel " + kind + " " + provider + " " + model + " by " + by)
	return model, nil
}

func (f fakeHostedModels) SetApps(kind string, every bool, apps []string, by string) error {
	w := strings.Join(apps, ",")
	if every {
		w = "every"
	}
	f.note("apps " + kind + " " + w + " by " + by)
	return nil
}

func (f fakeHostedModels) ResetApps(kind, by string) error {
	f.note("appsreset " + kind + " by " + by)
	return nil
}

func (f fakeHostedModels) SetKey(provider, value, by string) error {
	f.note("key " + provider + " " + value + " by " + by)
	return nil
}

func daemonWithAI(t *testing.T, m gateway.AIModels) *Client {
	t.Helper()
	home := t.TempDir()
	svcDir := filepath.Join(home, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(filepath.Join(home, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	sup, err := supervisor.New(supervisor.Options{
		ServicesDir: svcDir, Store: st,
		ProcessRuntime: &runtime.ProcessRuntime{LogDir: filepath.Join(home, "logs")},
	})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sock, sup, (&logBuffer{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	srv.UseAI(m)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sock)
}

func TestAI_HostedCommands(t *testing.T) {
	m := fakeHostedModels{&fakeAIModels{}}
	c := daemonWithAI(t, m)

	if msg, err := c.AISetFrom("llm", "anthropic", "claude-haiku-5-5"); err != nil || !strings.Contains(msg, "The LLM is now Anthropic's claude-haiku-5-5") {
		t.Errorf("AISetFrom = %q, %v", msg, err)
	}
	if _, err := c.AISet("system1", "laya"); err != nil {
		t.Fatal(err)
	}
	if msg, err := c.AIApps("llm", false, []string{"sous", "tasking"}); err != nil || !strings.Contains(msg, "for only sous, tasking") {
		t.Errorf("AIApps = %q, %v", msg, err)
	}
	if msg, err := c.AIApps("system1", false, nil); err != nil || !strings.Contains(msg, "for no app") {
		t.Errorf("AIApps(none) = %q, %v", msg, err)
	}
	if _, err := c.AIApps("llm", true, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.AIAppsReset("llm"); err != nil {
		t.Fatal(err)
	}
	msg, err := c.AIKey("openrouter", "sk-or-1")
	if err != nil || strings.Contains(msg, "sk-or-1") || !strings.Contains(msg, "Saved OpenRouter's API key") {
		t.Errorf("AIKey = %q, %v", msg, err)
	}
	want := "setmodel llm anthropic claude-haiku-5-5 by the CLI|setmodel system1  laya by the CLI|" +
		"apps llm sous,tasking by the CLI|apps system1  by the CLI|apps llm every by the CLI|appsreset llm by the CLI|" +
		"key openrouter sk-or-1 by the CLI"
	if got := strings.Join(m.log, "|"); got != want {
		t.Errorf("calls =\n%s\nwant\n%s", got, want)
	}
}

func TestAI_HostedCommandsNeedTheControls(t *testing.T) {
	c := daemonWithAI(t, &fakeAIModels{})
	if _, err := c.AIKey("openai", "sk"); err == nil || !strings.Contains(err.Error(), "Ollama's alone") {
		t.Errorf("AIKey on an Ollama-only service = %v", err)
	}
	if _, err := c.AISetFrom("llm", "openai", "gpt-6"); err == nil {
		t.Error("a provider was accepted by an Ollama-only service")
	}
}

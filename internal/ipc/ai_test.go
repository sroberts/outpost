package ipc

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/state"
	"github.com/sroberts/outpost/internal/supervisor"
)

type fakeAIModels struct {
	mu   sync.Mutex
	log  []string
	fail error
}

func (f *fakeAIModels) note(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, s)
}

func (f *fakeAIModels) Overview(context.Context) ai.Overview {
	return ai.Overview{URL: "http://127.0.0.1:47901", Choices: []ai.Choice{{Kind: ai.LLM, Model: "gemma4:e4b"}}}
}

func (f *fakeAIModels) Set(_ context.Context, kind, model, by string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	f.note("set " + kind + " " + model + " by " + by)
	return model + ":latest", nil
}

func (f *fakeAIModels) Reset(kind, by string) error {
	f.note("reset " + kind + " by " + by)
	return nil
}

func (f *fakeAIModels) Test(_ context.Context, kind string) (ai.TestResult, error) {
	return ai.TestResult{Kind: kind, Model: "gemma4:e4b", Reply: "ready", Took: time.Second}, nil
}

func (f *fakeAIModels) Regenerate(service, by string) error {
	f.note("regenerate " + service + " by " + by)
	return nil
}

func aiDaemon(t *testing.T, m *fakeAIModels) *Client {
	t.Helper()
	home := t.TempDir()
	svcDir := filepath.Join(home, "services")
	if err := os.MkdirAll(svcDir, 0o700); err != nil {
		t.Fatal(err)
	}
	body := "name = \"sous\"\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nport = 19300\nstorage = " +
		`"` + filepath.Join(home, "data", "sous") + `"` + "\ntailscale_name = \"sous\"\n"
	if err := os.WriteFile(filepath.Join(svcDir, "sous.toml"), []byte(body), 0o600); err != nil {
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
	if err := sup.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sock, sup, (&logBuffer{}).logf)
	if err != nil {
		t.Fatal(err)
	}
	if m != nil {
		srv.UseAI(m)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sock)
}

func TestAI_Commands(t *testing.T) {
	m := &fakeAIModels{}
	c := aiDaemon(t, m)

	o, err := c.AIStatus()
	if err != nil || o.URL != "http://127.0.0.1:47901" || o.Choices[0].Model != "gemma4:e4b" {
		t.Fatalf("AIStatus = %+v, %v", o, err)
	}
	msg, err := c.AISet(ai.System1, "laya")
	if err != nil || !strings.Contains(msg, "The System 1 is now laya:latest") {
		t.Errorf("AISet = %q, %v", msg, err)
	}
	if msg, err := c.AIReset(ai.LLM); err != nil || !strings.Contains(msg, "back to config.toml's") {
		t.Errorf("AIReset = %q, %v", msg, err)
	}
	if res, err := c.AITest(ai.LLM); err != nil || res.Reply != "ready" {
		t.Errorf("AITest = %+v, %v", res, err)
	}
	if msg, err := c.AIRegenerate("sous"); err != nil || !strings.Contains(msg, "sous has a new AI token") {
		t.Errorf("AIRegenerate = %q, %v", msg, err)
	}
	if _, err := c.AIRegenerate("nope"); err == nil || !strings.Contains(err.Error(), "no service named nope") {
		t.Errorf("AIRegenerate(nope) = %v", err)
	}
	want := "set system1 laya by the CLI|reset llm by the CLI|regenerate sous by the CLI"
	if got := strings.Join(m.log, "|"); got != want {
		t.Errorf("calls = %s", got)
	}

	m.fail = errors.New("can't use laya:latest as the LLM")
	if _, err := c.AISet(ai.LLM, "laya:latest"); err == nil || !strings.Contains(err.Error(), "as the LLM") {
		t.Errorf("a refused AISet = %v", err)
	}
}

func TestAI_WithoutTheServiceSaysSo(t *testing.T) {
	c := aiDaemon(t, nil)
	if _, err := c.AIStatus(); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("AIStatus = %v", err)
	}
}

func TestAI_TestHasTimeForAModelToLoad(t *testing.T) {
	if Timeout(CmdAITest) < 2*time.Minute {
		t.Errorf("ai.test times out after %s", Timeout(CmdAITest))
	}
}

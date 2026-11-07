package gateway

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/ai"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeAI is an Ollama with three models, as the page sees it through
// ai.Service.
type fakeAI struct {
	mu          sync.Mutex
	choices     map[string]ai.Choice
	unreachable bool
	set, resets []string
	regenerated []string
	tests       []string
	usage       []ai.Usage
}

func newFakeAI() *fakeAI {
	return &fakeAI{choices: map[string]ai.Choice{
		ai.LLM:     {Kind: ai.LLM, Model: "gemma4:e4b", File: "gemma4:e4b"},
		ai.System1: {Kind: ai.System1},
	}}
}

var fakeModels = []ai.Model{
	{Name: "gemma4:e4b", Size: 9 << 30, ParameterSize: "8.0B", Capabilities: []string{"completion", "vision"}},
	{Name: "qwen3:4b", Size: 2 << 30, ParameterSize: "4.0B", Capabilities: []string{"completion"}},
	{Name: "laya:latest", Size: 800 << 20, Capabilities: []string{"decision"}},
}

func (f *fakeAI) Overview(context.Context) ai.Overview {
	f.mu.Lock()
	defer f.mu.Unlock()
	o := ai.Overview{URL: "http://127.0.0.1:47901", OllamaURL: "http://127.0.0.1:11434", Usage: f.usage}
	if f.unreachable {
		o.ModelsError = "can't reach Ollama at http://127.0.0.1:11434: connection refused"
	} else {
		o.Models = fakeModels
	}
	for _, k := range ai.Kinds {
		o.Choices = append(o.Choices, f.choices[k])
	}
	return o
}

func (f *fakeAI) Set(_ context.Context, kind, model, by string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unreachable {
		return "", errors.New("can't check the model, so it wasn't saved")
	}
	for _, m := range fakeModels {
		if m.Name == model && m.Serves(kind) {
			c := f.choices[kind]
			c.Model, c.FromPortal, c.UpdatedBy, c.UpdatedAt = model, true, by, time.Now()
			f.choices[kind] = c
			f.set = append(f.set, kind+" "+model+" by "+by)
			return model, nil
		}
	}
	return "", errors.New("can't use " + model + " as the " + ai.KindName(kind))
}

func (f *fakeAI) Reset(kind, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.choices[kind]
	c.Model, c.FromPortal = c.File, false
	f.choices[kind] = c
	f.resets = append(f.resets, kind+" by "+by)
	return nil
}

func (f *fakeAI) Test(_ context.Context, kind string) (ai.TestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tests = append(f.tests, kind)
	if kind == ai.System1 {
		return ai.TestResult{}, errors.New("no System 1 is chosen")
	}
	return ai.TestResult{Kind: kind, Model: "gemma4:e4b", Reply: "ready", Took: 1200 * time.Millisecond}, nil
}

func (f *fakeAI) Regenerate(service, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.regenerated = append(f.regenerated, service+" by "+by)
	return nil
}

func aiHarness(t *testing.T) (*harness, *fakeAI) {
	h := newHarness(t)
	f := newFakeAI()
	h.g.ai = f
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Status: "running"},
		supervisor.ServiceStatus{Name: "tasking", Status: "running"},
	)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})
	return h, f
}

func TestAIPage_ShowsTheModelsAndWhatOllamaHas(t *testing.T) {
	h, f := aiHarness(t)
	f.usage = []ai.Usage{{Service: "sous", Kind: ai.LLM, Requests: 3, Errors: 1, InputTokens: 40, OutputTokens: 9,
		LastAt: time.Now(), LastError: "Ollama can't serve qwen", LastErrorAt: time.Now()}}
	c := h.client(true)
	h.login(c, "root", "", "/")

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/ai"`) {
		t.Errorf("the catalog doesn't link an admin to AI models:\n%s", b)
	}
	b := body(t, h.get(c, portal+"/ai"))
	for _, want := range []string{
		"<code>gemma4:e4b</code>",
		"From config.toml.",
		"None chosen.",
		`value="qwen3:4b"`,
		`value="laya:latest"`,
		"8.0B",
		"http://127.0.0.1:47901",
		"/openai/v1",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	// Each model is offered only for the kind it serves.
	llm := b[strings.Index(b, `id="k-llm"`):strings.Index(b, `id="k-system1"`)]
	if strings.Contains(llm, "laya") {
		t.Error("Laya is offered as an LLM")
	}
	if sys := b[strings.Index(b, `id="k-system1"`):]; strings.Contains(sys, `value="gemma4:e4b"`) {
		t.Error("Gemma is offered as a System 1 model")
	}
	// What each app has asked for is under its own menu now.
	b = body(t, h.get(c, portal+"/"))
	for _, want := range []string{
		"LLM: 3 requests, 1 failed, 40 tokens in, 9 out",
		"Last error: Ollama can&#39;t serve qwen",
		"No requests yet.",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("the catalog lacks %q", want)
		}
	}
}

func TestAIPage_SavesTestsAndResets(t *testing.T) {
	h, f := aiHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	b := body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"save"}, "kind": {"system1"}, "model": {"laya:latest"}}))
	if !strings.Contains(b, "Saved. Apps get it from their next request") || !strings.Contains(b, "Chosen here by root") {
		t.Errorf("after saving:\n%s", b)
	}
	if strings.Join(f.set, "|") != "system1 laya:latest by root" {
		t.Errorf("set = %v", f.set)
	}

	resp := h.post(c, portal+"/ai", portal, url.Values{"action": {"save"}, "kind": {"llm"}, "model": {"laya:latest"}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "Can&#39;t use laya:latest as the LLM") {
		t.Errorf("a refused choice: %d\n%s", resp.StatusCode, b)
	}

	if b := body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"test"}, "kind": {"llm"}})); !strings.Contains(b, "gemma4:e4b answered in 1.2s: “ready”") {
		t.Errorf("after testing:\n%s", b)
	}
	f.choices[ai.System1] = ai.Choice{Kind: ai.System1}
	if b := body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"test"}, "kind": {"system1"}})); !strings.Contains(b, "The test failed: no System 1 is chosen") {
		t.Errorf("a failed test:\n%s", b)
	}

	f.choices[ai.LLM] = ai.Choice{Kind: ai.LLM, Model: "qwen3:4b", File: "gemma4:e4b", FromPortal: true, UpdatedBy: "root", UpdatedAt: time.Now()}
	b = body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, "over config.toml&#39;s gemma4:e4b") || !strings.Contains(b, "Use config.toml's") {
		t.Errorf("an overridden LLM:\n%s", b)
	}
	if b := body(t, h.post(c, portal+"/ai", portal, url.Values{"action": {"reset"}, "kind": {"llm"}})); !strings.Contains(b, "Back to what config.toml says.") {
		t.Errorf("after resetting:\n%s", b)
	}
	if strings.Join(f.resets, "|") != "llm by root" {
		t.Errorf("resets = %v", f.resets)
	}
}

func TestAIPage_RegeneratesAnAppsToken(t *testing.T) {
	h, f := aiHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-token"}, "app": {"sous"}}))
	if !strings.Contains(b, "New token made.") {
		t.Errorf("after regenerating:\n%s", b)
	}
	if strings.Join(f.regenerated, "|") != "sous by root" {
		t.Errorf("regenerated = %v", f.regenerated)
	}
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"ai-token"}, "app": {"nope"}}); resp.StatusCode != 400 {
		t.Errorf("an unknown app: %d", resp.StatusCode)
	}
}

func TestAIPage_SaysWhenOllamaCantBeReached(t *testing.T) {
	h, f := aiHarness(t)
	f.unreachable = true
	c := h.client(true)
	h.login(c, "root", "", "/")
	b := body(t, h.get(c, portal+"/ai"))
	if !strings.Contains(b, "Outpost can't list Ollama's models") || !strings.Contains(b, "<code>gemma4:e4b</code>") {
		t.Errorf("unreachable Ollama:\n%s", b)
	}
	if strings.Contains(b, "Choose<span") {
		t.Error("offers a choice with nothing to choose from")
	}
}

func TestAIPage_OnlyAnAdmin(t *testing.T) {
	h, f := aiHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, `href="/ai"`) {
		t.Error("a plain user is linked to AI models")
	}
	if resp := h.get(plain, portal+"/ai"); resp.StatusCode != 403 {
		t.Errorf("GET as a plain user = %d", resp.StatusCode)
	}
	save := url.Values{"action": {"save"}, "kind": {"llm"}, "model": {"qwen3:4b"}}
	if resp := h.post(plain, portal+"/ai", portal, save); resp.StatusCode != 403 {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/ai", "http://evil.test", save); resp.StatusCode != 403 {
		t.Errorf("POST from another site = %d", resp.StatusCode)
	}
	if len(f.set) != 0 {
		t.Errorf("a refused request was saved: %v", f.set)
	}
}

func TestNoAIPageWithoutTheService(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	c := h.client(true)
	h.login(c, "root", "", "/")
	if resp := h.get(c, portal+"/ai"); resp.StatusCode != 404 {
		t.Errorf("GET /ai with no AI service = %d", resp.StatusCode)
	}
}

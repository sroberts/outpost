package ai

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOllama is enough of Ollama's API for the endpoint: a model list, each
// model's capabilities, and the three APIs apps reach through Outpost.
type fakeOllama struct {
	t   *testing.T
	srv *httptest.Server

	mu     sync.Mutex
	models map[string][]string // name -> capabilities
	shows  int
	// last is the most recent request to one of the APIs.
	lastPath   string
	lastBody   map[string]any
	lastHeader http.Header
}

func newFakeOllama(t *testing.T) *fakeOllama {
	f := &fakeOllama{t: t, models: map[string][]string{
		"gemma4:e4b":   {"completion", "vision", "tools", "thinking"},
		"qwen3:4b":     {"completion", "tools"},
		"laya:latest":  {"decision"},
		"nomic:latest": {"embedding"},
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOllama) remove(name string) {
	f.mu.Lock()
	delete(f.models, name)
	f.mu.Unlock()
}

func (f *fakeOllama) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch r.URL.Path {
	case "/api/tags":
		type tag struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		}
		var out struct {
			Models []tag `json:"models"`
		}
		for name := range f.models {
			out.Models = append(out.Models, tag{Name: name, Digest: "sha-" + name})
		}
		_ = json.NewEncoder(w).Encode(out)
		return
	case "/api/show":
		f.shows++
		var req struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		caps, ok := f.models[req.Model]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":"model not found"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"capabilities": caps})
		return
	}

	body, _ := io.ReadAll(r.Body)
	var fields map[string]any
	_ = json.Unmarshal(body, &fields)
	f.lastPath, f.lastBody, f.lastHeader = r.URL.Path, fields, r.Header.Clone()
	model, _ := fields["model"].(string)
	if _, ok := f.models[model]; !ok {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"model '`+model+`' not found"}`)
		return
	}
	switch r.URL.Path {
	case "/v1/chat/completions":
		if fields["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, part := range []string{"Hel", "lo"} {
				_, _ = io.WriteString(w, `data: {"choices":[{"delta":{"content":"`+part+`"}}]}`+"\n\n")
				w.(http.Flusher).Flush()
			}
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+model+`","choices":[{"message":{"content":"Hello"}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`)
	case "/v1/messages":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"message","model":"`+model+`","content":[{"type":"text","text":"Hi"}],"usage":{"input_tokens":4,"cache_read_input_tokens":5,"output_tokens":2}}`)
	case "/v1/systemone":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+model+`","answers":{"weather":{"type":"noul","noul":0.92}},"usage":{"input_tokens":20,"output_tokens":0}}`)
	case "/api/chat":
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"model":"`+model+`","message":{"role":"assistant","content":" ready\n"}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"404 page not found"}`)
	}
}

func (f *fakeOllama) last() (string, map[string]any, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPath, f.lastBody, f.lastHeader
}

type harness struct {
	svc    *Service
	ollama *fakeOllama
	url    string
	dbPath string
	lines  *[]string
}

func newHarness(t *testing.T, llm, system1 string) *harness {
	t.Helper()
	f := newFakeOllama(t)
	dbPath := filepath.Join(t.TempDir(), "ai.db")
	h := &harness{ollama: f, dbPath: dbPath, lines: &[]string{}}
	h.svc = h.open(t, llm, system1)
	return h
}

func (h *harness) open(t *testing.T, llm, system1 string) *Service {
	t.Helper()
	var mu sync.Mutex
	svc, err := Open(Options{
		Path: h.dbPath, OllamaURL: h.ollama.srv.URL, Listen: "127.0.0.1:47901",
		LLM: llm, System1: system1,
		Logf: func(format string, v ...any) {
			mu.Lock()
			defer mu.Unlock()
			*h.lines = append(*h.lines, format)
		},
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return svc
}

func (h *harness) post(t *testing.T, path, token, body string, header ...string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.url+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestResolve_GivesEveryAppTheURLAndItsOwnToken(t *testing.T) {
	h := newHarness(t, "", "")
	env, rev := h.svc.Resolve("sous")
	if env[EnvURL] != "http://127.0.0.1:47901" {
		t.Errorf("%s = %q", EnvURL, env[EnvURL])
	}
	tok := env[EnvToken]
	if !strings.HasPrefix(tok, "oai_") || len(tok) < 40 {
		t.Errorf("%s = %q; want an oai_ token", EnvToken, tok)
	}
	other, _ := h.svc.Resolve("tasking")
	if other[EnvToken] == tok {
		t.Error("two apps were given the same token")
	}
	again, rev2 := h.svc.Resolve("sous")
	if again[EnvToken] != tok || rev2 != rev {
		t.Error("Resolve isn't stable for one app")
	}
}

func TestToken_SurvivesReopeningAndRegenerateReplacesIt(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, rev := h.svc.Resolve("sous")
	tok := env[EnvToken]
	_ = h.svc.Close()

	// A daemon restart: the app is still running with the old environment.
	svc := h.open(t, "gemma4:e4b", "")
	env2, rev2 := svc.Resolve("sous")
	if env2[EnvToken] != tok || rev2 != rev {
		t.Fatalf("token changed across a reopen: %q -> %q", tok, env2[EnvToken])
	}
	if resp, _ := h.post(t, "/openai/v1/chat/completions", tok, `{"messages":[]}`); resp.StatusCode != 200 {
		t.Fatalf("status = %d; want 200 with the token from before the restart", resp.StatusCode)
	}

	if err := svc.Regenerate("sous", "admin"); err != nil {
		t.Fatal(err)
	}
	env3, rev3 := svc.Resolve("sous")
	if env3[EnvToken] == tok || rev3 == rev {
		t.Error("Regenerate left the token or its revision as it was")
	}
	if resp, _ := h.post(t, "/openai/v1/chat/completions", tok, `{"messages":[]}`); resp.StatusCode != 401 {
		t.Errorf("old token: status = %d; want 401", resp.StatusCode)
	}
	if resp, _ := h.post(t, "/openai/v1/chat/completions", env3[EnvToken], `{"messages":[]}`); resp.StatusCode != 200 {
		t.Errorf("new token: status = %d; want 200", resp.StatusCode)
	}
}

func TestProxy_RefusesWithoutAKnownToken(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "laya:latest")
	for _, tok := range []string{"", "oai_nope", "opat_something"} {
		resp, body := h.post(t, "/openai/v1/chat/completions", tok, `{}`)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "OUTPOST_AI_TOKEN") {
			t.Errorf("token %q: %d %s", tok, resp.StatusCode, body)
		}
	}
	if path, _, _ := h.ollama.last(); path != "" {
		t.Errorf("a refused request reached Ollama at %s", path)
	}
}

func TestProxy_ReplacesTheModelAndNeverForwardsTheToken(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "laya:latest")
	env, _ := h.svc.Resolve("sous")
	tok := env[EnvToken]

	cases := []struct {
		path, upstream, model string
		header                []string
	}{
		{"/openai/v1/chat/completions", "/v1/chat/completions", "gemma4:e4b", nil},
		{"/anthropic/v1/messages", "/v1/messages", "gemma4:e4b", []string{"Anthropic-Version", "2023-06-01"}},
		{"/systemone/v1/systemone", "/v1/systemone", "laya:latest", nil},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, body := h.post(t, c.path, tok, `{"model":"llm","temperature":0.2,"messages":[{"role":"user","content":"hi"}]}`, c.header...)
			if resp.StatusCode != 200 {
				t.Fatalf("status = %d: %s", resp.StatusCode, body)
			}
			path, sent, hdr := h.ollama.last()
			if path != c.upstream {
				t.Errorf("upstream path = %q; want %q", path, c.upstream)
			}
			if sent["model"] != c.model {
				t.Errorf("model sent = %v; want %s", sent["model"], c.model)
			}
			if sent["temperature"] != 0.2 {
				t.Errorf("other fields weren't passed through: %v", sent)
			}
			for k, vs := range hdr {
				for _, v := range vs {
					if strings.Contains(v, tok) {
						t.Errorf("the app's token went to Ollama in %s", k)
					}
				}
			}
			if hdr.Get("Authorization") != "" || hdr.Get("X-Api-Key") != "" {
				t.Errorf("credential headers went to Ollama: %v", hdr)
			}
			if c.header != nil && hdr.Get(c.header[0]) != c.header[1] {
				t.Errorf("%s wasn't passed on", c.header[0])
			}
		})
	}
}

func TestProxy_TakesTheTokenAsXApiKey(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	resp, body := h.post(t, "/anthropic/v1/messages", "", `{"messages":[]}`, "X-Api-Key", env[EnvToken])
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d: %s", resp.StatusCode, body)
	}
}

func TestProxy_PortalChoiceWinsAndTakesEffectAtOnce(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	if _, err := h.svc.Set(context.Background(), LLM, "qwen3:4b", "admin"); err != nil {
		t.Fatal(err)
	}
	h.post(t, "/openai/v1/chat/completions", env[EnvToken], `{"messages":[]}`)
	if _, sent, _ := h.ollama.last(); sent["model"] != "qwen3:4b" {
		t.Errorf("model = %v; want the portal's qwen3:4b", sent["model"])
	}
	c := h.svc.Choice(LLM)
	if !c.FromPortal || c.File != "gemma4:e4b" || c.UpdatedBy != "admin" {
		t.Errorf("Choice = %+v", c)
	}

	if err := h.svc.Reset(LLM, "admin"); err != nil {
		t.Fatal(err)
	}
	h.post(t, "/openai/v1/chat/completions", env[EnvToken], `{"messages":[]}`)
	if _, sent, _ := h.ollama.last(); sent["model"] != "gemma4:e4b" {
		t.Errorf("after Reset model = %v; want config.toml's gemma4:e4b", sent["model"])
	}
}

func TestSet_OnlyAModelOllamaHasOfTheRightKind(t *testing.T) {
	h := newHarness(t, "", "")
	ctx := context.Background()
	cases := []struct {
		kind, model, want string
	}{
		{LLM, "llama9:70b", "ollama pull llama9:70b"},
		{LLM, "laya:latest", "can't use laya:latest as the LLM"},
		{System1, "gemma4:e4b", "as the System 1"},
		{LLM, "nomic", "as the LLM"},
		{"embeddings", "nomic", "no kind of model"},
		{LLM, " ", "choose a model"},
	}
	for _, c := range cases {
		_, err := h.svc.Set(ctx, c.kind, c.model, "admin")
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Set(%s, %q) = %v; want an error with %q", c.kind, c.model, err, c.want)
		}
	}
	// A bare name means :latest, as it does to Ollama, and is saved as listed.
	if saved, err := h.svc.Set(ctx, System1, "laya", "admin"); err != nil || saved != "laya:latest" {
		t.Fatalf("Set(system1, laya) = %q, %v; want it saved as laya:latest", saved, err)
	}
	if got := h.svc.Choice(System1).Model; got != "laya:latest" {
		t.Errorf("saved %q; want laya:latest", got)
	}
}

func TestSet_RefusesWhenOllamaIsUnreachable(t *testing.T) {
	h := newHarness(t, "", "")
	h.ollama.srv.Close()
	_, err := h.svc.Set(context.Background(), LLM, "gemma4:e4b", "admin")
	if err == nil || !strings.Contains(err.Error(), "wasn't saved") {
		t.Errorf("Set = %v; want it refused", err)
	}
	if h.svc.Choice(LLM).FromPortal {
		t.Error("a setting was saved without being checked")
	}
	o := h.svc.Overview(context.Background())
	if o.ModelsError == "" || !strings.Contains(o.ModelsError, "can't reach Ollama") {
		t.Errorf("ModelsError = %q", o.ModelsError)
	}
}

func TestProxy_NoModelChosenIs503(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	resp, body := h.post(t, "/systemone/v1/systemone", env[EnvToken], `{"state":"x","questions":{}}`)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "no System 1 is configured") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
}

func TestProxy_AModelGoneFromOllamaIs503NamingIt(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	h.ollama.remove("gemma4:e4b")
	resp, body := h.post(t, "/openai/v1/chat/completions", env[EnvToken], `{"messages":[]}`)
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(body, "gemma4:e4b") {
		t.Errorf("%d %s", resp.StatusCode, body)
	}
	o := h.svc.Overview(context.Background())
	if !o.Choices[0].Missing {
		t.Errorf("Overview doesn't say the LLM is missing: %+v", o.Choices[0])
	}
}

func TestProxy_RefusesOtherPathsMethodsAndBodies(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	tok := env[EnvToken]
	if resp, _ := h.post(t, "/api/chat", tok, `{}`); resp.StatusCode != 404 {
		t.Errorf("Ollama's own API: %d; want 404", resp.StatusCode)
	}
	if resp, _ := h.post(t, "/openai/v1/embeddings", tok, `{}`); resp.StatusCode != 404 {
		t.Errorf("embeddings: %d; want 404", resp.StatusCode)
	}
	if resp, _ := h.post(t, "/openai/v1/chat/completions", tok, `[1,2]`); resp.StatusCode != 400 {
		t.Errorf("a JSON array: %d; want 400", resp.StatusCode)
	}
	big := `{"x":"` + strings.Repeat("a", MaxBody) + `"}`
	if resp, _ := h.post(t, "/openai/v1/chat/completions", tok, big); resp.StatusCode != 413 {
		t.Errorf("over MaxBody: %d; want 413", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, h.url+"/openai/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("GET: %d; want 405", resp.StatusCode)
	}
}

func TestProxy_StreamsAsItArrives(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "")
	env, _ := h.svc.Resolve("sous")
	req, _ := http.NewRequest(http.MethodPost, h.url+"/openai/v1/chat/completions", strings.NewReader(`{"stream":true,"messages":[]}`))
	req.Header.Set("Authorization", "Bearer "+env[EnvToken])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q", ct)
	}
	var events []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if line := sc.Text(); strings.HasPrefix(line, "data: ") {
			events = append(events, line)
		}
	}
	if len(events) != 3 || events[2] != "data: [DONE]" {
		t.Errorf("events = %q", events)
	}
}

func TestProxy_ModelsListsTheNamesAppsUse(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "laya:latest")
	env, _ := h.svc.Resolve("sous")
	req, _ := http.NewRequest(http.MethodGet, h.url+"/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+env[EnvToken])
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Model string `json:"outpost_model"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatal(err)
	}
	if len(list.Data) != 2 || list.Data[0].ID != "llm" || list.Data[0].Model != "gemma4:e4b" ||
		list.Data[1].ID != "system1" || list.Data[1].Model != "laya:latest" {
		t.Errorf("models = %+v", list.Data)
	}
}

func TestUsage_CountsByAppAndKind(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "laya:latest")
	sous, _ := h.svc.Resolve("sous")
	tasking, _ := h.svc.Resolve("tasking")
	h.post(t, "/openai/v1/chat/completions", sous[EnvToken], `{"messages":[]}`)
	h.post(t, "/anthropic/v1/messages", sous[EnvToken], `{"messages":[]}`)
	h.post(t, "/systemone/v1/systemone", tasking[EnvToken], `{"state":"x","questions":{}}`)
	h.ollama.remove("laya:latest")
	h.post(t, "/systemone/v1/systemone", tasking[EnvToken], `{"state":"x","questions":{}}`)

	u := h.svc.Usage()
	if len(u) != 2 {
		t.Fatalf("usage = %+v", u)
	}
	if u[0].Service != "sous" || u[0].Kind != LLM || u[0].Requests != 2 || u[0].InputTokens != 21 || u[0].OutputTokens != 5 || u[0].Errors != 0 {
		t.Errorf("sous = %+v", u[0])
	}
	if u[1].Service != "tasking" || u[1].Kind != System1 || u[1].Requests != 2 || u[1].Errors != 1 || u[1].LastError == "" {
		t.Errorf("tasking = %+v", u[1])
	}
}

func TestTest_SendsOneSmallRequestOfEachKind(t *testing.T) {
	h := newHarness(t, "gemma4:e4b", "laya:latest")
	r, err := h.svc.Test(context.Background(), LLM)
	if err != nil || r.Reply != "ready" || r.Model != "gemma4:e4b" {
		t.Errorf("LLM test = %+v, %v", r, err)
	}
	if path, sent, _ := h.ollama.last(); path != "/api/chat" || sent["think"] != false {
		t.Errorf("LLM test went to %s with %v; want /api/chat with think off for a thinking model", path, sent)
	}
	r, err = h.svc.Test(context.Background(), System1)
	if err != nil || !strings.Contains(r.Reply, "92% yes") {
		t.Errorf("System 1 test = %+v, %v", r, err)
	}
	if _, err := (newHarness(t, "", "")).svc.Test(context.Background(), LLM); err == nil {
		t.Error("testing with no LLM chosen succeeded")
	}
}

func TestOllama_DescribesEachModelOnce(t *testing.T) {
	h := newHarness(t, "", "")
	for i := 0; i < 3; i++ {
		if o := h.svc.Overview(context.Background()); o.ModelsError != "" || len(o.Models) != 4 {
			t.Fatalf("Overview = %+v", o)
		}
	}
	h.ollama.mu.Lock()
	shows := h.ollama.shows
	h.ollama.mu.Unlock()
	if shows != 4 {
		t.Errorf("/api/show called %d times for 4 models over 3 listings; want 4", shows)
	}
}

func TestServe_StopsWithItsContext(t *testing.T) {
	h := newHarness(t, "", "")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.svc.Serve(ctx, ln) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve didn't return after its context ended")
	}
}

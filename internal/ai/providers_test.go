package ai

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// memKeys is secrets.db as the AI service sees it.
type memKeys struct {
	mu   sync.Mutex
	vals map[string]string
}

func (k *memKeys) Value(name string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	v, ok := k.vals[name]
	return v, ok
}

func (k *memKeys) Put(name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.vals == nil {
		k.vals = map[string]string{}
	}
	k.vals[name] = value
	return nil
}

// hosted is a fake hosted provider: it records what it was sent, lists
// models, and answers each API.
type hosted struct {
	srv *httptest.Server

	mu     sync.Mutex
	path   string
	query  string
	body   map[string]any
	header http.Header
	lists  int
}

func newHosted(t *testing.T, serve func(h *hosted, w http.ResponseWriter, r *http.Request, body map[string]any)) *hosted {
	h := &hosted{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		h.mu.Lock()
		h.path, h.query, h.body, h.header = r.URL.Path, r.URL.RawQuery, body, r.Header.Clone()
		if strings.HasSuffix(r.URL.Path, "/models") {
			h.lists++
		}
		h.mu.Unlock()
		serve(h, w, r, body)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hosted) last() (string, map[string]any, http.Header) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.path, h.body, h.header
}

func writeJSON(w http.ResponseWriter, s string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, s)
}

func fakeOpenAI(t *testing.T) *hosted {
	return newHosted(t, func(h *hosted, w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer sk-openai" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, `{"error":{"message":"Incorrect API key provided"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			writeJSON(w, `{"data":[{"id":"gpt-6"},{"id":"gpt-6-mini"},{"id":"text-embedding-3-large"},{"id":"o5"},{"id":"gpt-6-tts"},{"id":"dall-e-4"}]}`)
		case "/v1/chat/completions":
			writeJSON(w, `{"model":"`+body["model"].(string)+`","choices":[{"message":{"content":"ready"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
		case "/v1/decisions":
			writeJSON(w, `{"model":"gpt-6-luna","answers":[
				{"type":"predicate","name":"upset","probability":0.9},
				{"type":"choice","name":"topic","choice":"shipping","confidence":0.8,"probabilities":[{"value":"shipping","probability":0.85},{"value":"billing","probability":0.15}]},
				{"type":"score","name":"urgency","score":2.4,"confidence":0.7,"probabilities":[{"value":0,"label":"low","probability":0.1},{"value":1,"label":"mid","probability":0.4},{"value":2,"label":"high","probability":0.5}]},
				{"type":"refusal","name":"secret"}],
				"usage":{"input_tokens":40,"output_tokens":0}}`)
		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, `{"error":{"message":"not found"}}`)
		}
	})
}

func fakeAnthropic(t *testing.T) *hosted {
	return newHosted(t, func(h *hosted, w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Header.Get("X-Api-Key") != "sk-ant" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			writeJSON(w, `{"data":[{"id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5"},{"id":"claude-opus-5-5","display_name":"Claude Opus 5.5"}]}`)
		case "/v1/messages":
			writeJSON(w, `{"type":"message","model":"`+body["model"].(string)+`","content":[{"type":"text","text":"ready"}],"usage":{"input_tokens":7,"output_tokens":1}}`)
		case "/v1/chat/completions":
			writeJSON(w, `{"model":"`+body["model"].(string)+`","choices":[{"message":{"content":"ready"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func fakeOpenRouter(t *testing.T) *hosted {
	return newHosted(t, func(h *hosted, w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.URL.Path == "/v1/models" {
			// Listed to anyone, key or not.
			if r.URL.Query().Get("output_modalities") == "decisions" {
				writeJSON(w, `{"data":[
					{"id":"typesafe/jev-1.14","name":"TypeSafe: Jev 1.14","endpoints":["/api/alpha/decisions"],"pricing":{"prompt":"0.000000042"}},
					{"id":"liquid/d1","endpoints":["/api/alpha/decisions"],"pricing":{"prompt":"0.00000002"}},
					{"id":"cloudflare/clairic","endpoints":["/api/alpha/extractions"]}]}`)
				return
			}
			writeJSON(w, `{"data":[
				{"id":"anthropic/claude-haiku-5-5","name":"Anthropic: Claude Haiku 5.5","context_length":200000,"endpoints":["/api/v1/chat/completions","/api/v1/messages"],"pricing":{"prompt":"0.000001"}},
				{"id":"google/imagen-5","endpoints":["/api/v1/images"]},
				{"id":"meta/llama-free:free","endpoints":["/api/v1/chat/completions"],"pricing":{"prompt":"0"}}]}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer sk-or" {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, `{"error":{"message":"No auth credentials found"}}`)
			return
		}
		switch r.URL.Path {
		case "/v1/systemone":
			writeJSON(w, `{"model":"`+body["model"].(string)+`","answers":{"q":{"type":"noul","noul":0.7}},"usage":{"input_tokens":12,"output_tokens":0}}`)
		case "/v1/messages", "/v1/chat/completions":
			writeJSON(w, `{"model":"`+body["model"].(string)+`","choices":[{"message":{"content":"ready"}}]}`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func fakeTypeSafe(t *testing.T) *hosted {
	return newHosted(t, func(h *hosted, w http.ResponseWriter, r *http.Request, body map[string]any) {
		if r.Header.Get("Authorization") != "Bearer ts-key" || r.URL.Path != "/v1/systemone" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, `{"model":"jev-1.14.0","answers":{"weather":{"type":"noul","noul":0.96}},"usage":{"input_tokens":30,"output_tokens":0}}`)
	})
}

type cloud struct {
	svc                                  *Service
	ollama                               *fakeOllama
	openai, anthropic, openrouter, types *hosted
	keys                                 *memKeys
	url                                  string
	dbPath                               string
}

// newCloud is a service with every provider faked, and opts on top.
func newCloud(t *testing.T, opts Options) *cloud {
	t.Helper()
	c := &cloud{
		ollama: newFakeOllama(t), openai: fakeOpenAI(t), anthropic: fakeAnthropic(t),
		openrouter: fakeOpenRouter(t), types: fakeTypeSafe(t), keys: &memKeys{},
		dbPath: filepath.Join(t.TempDir(), "ai.db"),
	}
	opts.Path, opts.OllamaURL, opts.Listen, opts.Keys = c.dbPath, c.ollama.srv.URL, "127.0.0.1:47901", c.keys
	opts.Providers = map[string]ProviderConfig{
		ProviderOpenAI:     {BaseURL: c.openai.srv.URL},
		ProviderAnthropic:  {BaseURL: c.anthropic.srv.URL},
		ProviderOpenRouter: {BaseURL: c.openrouter.srv.URL},
		ProviderTypeSafe:   {BaseURL: c.types.srv.URL},
	}
	svc, err := Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	srv := httptest.NewServer(svc)
	t.Cleanup(srv.Close)
	c.svc, c.url = svc, srv.URL
	return c
}

func (c *cloud) keyAll(t *testing.T) {
	t.Helper()
	for p, k := range map[string]string{ProviderOpenAI: "sk-openai", ProviderAnthropic: "sk-ant", ProviderOpenRouter: "sk-or", ProviderTypeSafe: "ts-key"} {
		if err := c.svc.SetKey(p, k, "admin"); err != nil {
			t.Fatal(err)
		}
	}
}

func (c *cloud) post(t *testing.T, path, token, body string, header ...string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.url+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (c *cloud) token(service string) string {
	env, _ := c.svc.Resolve(service)
	return env[EnvToken]
}

func TestHosted_NeedsItsKeyBeforeAnythingElse(t *testing.T) {
	c := newCloud(t, Options{})
	ctx := context.Background()
	if _, err := c.svc.SetModel(ctx, LLM, ProviderOpenAI, "gpt-6", "admin"); err == nil || !strings.Contains(err.Error(), "add OpenAI's API key first") {
		t.Errorf("SetModel with no key = %v", err)
	}
	o := c.svc.Overview(ctx)
	for _, cat := range o.Catalogs {
		if cat.Provider == ProviderOpenAI && cat.Kind == LLM && !strings.Contains(cat.Error, "add OpenAI's API key") {
			t.Errorf("OpenAI's catalog without a key = %+v", cat)
		}
	}
	if err := c.svc.SetKey(ProviderOllama, "x", "admin"); err == nil {
		t.Error("Ollama took an API key")
	}
	if err := c.svc.SetKey(ProviderOpenAI, "  ", "admin"); err == nil {
		t.Error("an empty key was saved")
	}
	if err := c.svc.SetKey(ProviderOpenAI, "sk-openai", "admin"); err != nil {
		t.Fatal(err)
	}
	if v, _ := c.keys.Value("openai-api-key"); v != "sk-openai" {
		t.Errorf("the key was kept as %q in openai-api-key", v)
	}
	var st ProviderStatus
	for _, p := range c.svc.Providers() {
		if p.Name == ProviderOpenAI {
			st = p
		}
	}
	if !st.HasKey || st.KeySecret != "openai-api-key" || st.BaseURL != c.openai.srv.URL {
		t.Errorf("OpenAI's status = %+v", st)
	}
}

func TestHosted_OpenAILLM(t *testing.T) {
	c := newCloud(t, Options{})
	c.keyAll(t)
	ctx := context.Background()

	o := c.svc.Overview(ctx)
	var names []string
	for _, cat := range o.Catalogs {
		if cat.Provider == ProviderOpenAI && cat.Kind == LLM {
			for _, m := range cat.Models {
				names = append(names, m.Name)
			}
		}
	}
	if strings.Join(names, ",") != "gpt-6,gpt-6-mini,o5" {
		t.Errorf("OpenAI's LLMs = %v; want its chat models only", names)
	}
	if _, err := c.svc.SetModel(ctx, LLM, ProviderOpenAI, "text-embedding-3-large", "admin"); err == nil {
		t.Error("an embedding model was taken as the LLM")
	}
	if saved, err := c.svc.SetModel(ctx, LLM, ProviderOpenAI, "gpt-6-mini", "admin"); err != nil || saved != "gpt-6-mini" {
		t.Fatalf("SetModel = %q, %v", saved, err)
	}

	tok := c.token("sous")
	status, body := c.post(t, "/openai/v1/chat/completions", tok, `{"model":"llm","messages":[{"role":"user","content":"hi"}]}`)
	if status != 200 || !strings.Contains(body, "ready") {
		t.Fatalf("%d %s", status, body)
	}
	path, sent, hdr := c.openai.last()
	if path != "/v1/chat/completions" || sent["model"] != "gpt-6-mini" || hdr.Get("Authorization") != "Bearer sk-openai" {
		t.Errorf("OpenAI got %s model=%v auth=%q", path, sent["model"], hdr.Get("Authorization"))
	}
	if strings.Contains(strings.Join(hdr.Values("Authorization"), " "), tok) {
		t.Error("the app's token went to OpenAI")
	}

	// OpenAI doesn't take the Anthropic API.
	status, body = c.post(t, "/anthropic/v1/messages", tok, `{"model":"llm","max_tokens":5,"messages":[]}`)
	if status != 400 || !strings.Contains(body, "doesn't take this API") || !strings.Contains(body, "$OUTPOST_AI_URL/openai/v1/chat/completions") {
		t.Errorf("Anthropic API to an OpenAI LLM: %d %s", status, body)
	}
	if ch := c.svc.Choice(LLM); len(ch.APIs) != 1 || ch.APIs[0] != "/openai/v1/chat/completions" {
		t.Errorf("APIs = %v", ch.APIs)
	}

	if r, err := c.svc.Test(ctx, LLM); err != nil || r.Reply != "ready" || r.Provider != ProviderOpenAI {
		t.Errorf("Test = %+v, %v", r, err)
	}
}

func TestHosted_AnthropicLLMTakesBothAPIs(t *testing.T) {
	c := newCloud(t, Options{LLM: "claude-haiku-5-5", LLMProvider: ProviderAnthropic})
	c.keyAll(t)
	tok := c.token("sous")

	status, body := c.post(t, "/anthropic/v1/messages", tok, `{"model":"llm","max_tokens":5,"messages":[]}`)
	if status != 200 || !strings.Contains(body, `"ready"`) {
		t.Fatalf("%d %s", status, body)
	}
	path, sent, hdr := c.anthropic.last()
	if path != "/v1/messages" || sent["model"] != "claude-haiku-5-5" || hdr.Get("X-Api-Key") != "sk-ant" || hdr.Get("Anthropic-Version") != "2023-06-01" {
		t.Errorf("Anthropic got %s model=%v headers=%v", path, sent["model"], hdr)
	}
	if hdr.Get("Authorization") != "" {
		t.Error("an Authorization header went to Anthropic")
	}
	// An app's own anthropic-version is kept.
	c.post(t, "/anthropic/v1/messages", tok, `{"messages":[]}`, "Anthropic-Version", "2025-01-01")
	if _, _, hdr := c.anthropic.last(); hdr.Get("Anthropic-Version") != "2025-01-01" {
		t.Errorf("Anthropic-Version = %q", hdr.Get("Anthropic-Version"))
	}

	if status, _ := c.post(t, "/openai/v1/chat/completions", tok, `{"messages":[]}`); status != 200 {
		t.Errorf("OpenAI API to an Anthropic LLM = %d", status)
	}
	if path, _, _ := c.anthropic.last(); path != "/v1/chat/completions" {
		t.Errorf("the OpenAI API went to Anthropic's %s", path)
	}
	if _, err := c.svc.SetModel(context.Background(), LLM, ProviderAnthropic, "claude-nope", "admin"); err == nil ||
		!strings.Contains(err.Error(), "doesn't list a LLM model called claude-nope") {
		t.Errorf("an unlisted Claude = %v", err)
	}
	if r, err := c.svc.Test(context.Background(), LLM); err != nil || r.Reply != "ready" {
		t.Errorf("Test = %+v, %v", r, err)
	}
}

func TestHosted_OpenRouter(t *testing.T) {
	c := newCloud(t, Options{})
	ctx := context.Background()

	// Its lists need no key; asking for a model does.
	o := c.svc.Overview(ctx)
	got := map[string][]string{}
	for _, cat := range o.Catalogs {
		if cat.Provider == ProviderOpenRouter {
			for _, m := range cat.Models {
				got[cat.Kind] = append(got[cat.Kind], m.Name+" ["+m.Note+"]")
			}
		}
	}
	if strings.Join(got[LLM], "|") != "anthropic/claude-haiku-5-5 [Anthropic: Claude Haiku 5.5 · 200K context · $1.00 per million input tokens]|meta/llama-free:free [free per million input tokens]" {
		t.Errorf("OpenRouter LLMs = %q", got[LLM])
	}
	if strings.Join(got[System1], "|") != "liquid/d1 [$0.02 per million input tokens]|typesafe/jev-1.14 [TypeSafe: Jev 1.14 · $0.04 per million input tokens]" {
		t.Errorf("OpenRouter System 1 = %q", got[System1])
	}

	if err := c.svc.SetKey(ProviderOpenRouter, "sk-or", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.SetModel(ctx, System1, ProviderOpenRouter, "typesafe/jev-1.14", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.SetModel(ctx, LLM, ProviderOpenRouter, "anthropic/claude-haiku-5-5", "admin"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.svc.SetModel(ctx, System1, ProviderOpenRouter, "cloudflare/clairic", "admin"); err == nil {
		t.Error("an extraction model was taken as System 1")
	}
	tok := c.token("sous")
	status, body := c.post(t, "/systemone/v1/systemone", tok, `{"model":"system1","state":"x","questions":{"q":{"type":"noul","instructions":"?"}}}`)
	if status != 200 || !strings.Contains(body, `"noul":0.7`) {
		t.Fatalf("%d %s", status, body)
	}
	path, sent, hdr := c.openrouter.last()
	if path != "/v1/systemone" || sent["model"] != "typesafe/jev-1.14" || hdr.Get("Authorization") != "Bearer sk-or" || hdr.Get("X-Title") != "Outpost" {
		t.Errorf("OpenRouter got %s model=%v headers=%v", path, sent["model"], hdr)
	}
	if status, _ := c.post(t, "/anthropic/v1/messages", tok, `{"messages":[]}`); status != 200 {
		t.Errorf("Anthropic API to OpenRouter = %d", status)
	}
	if path, _, _ := c.openrouter.last(); path != "/v1/messages" {
		t.Errorf("the Anthropic API went to OpenRouter's %s", path)
	}

	// The list is kept for a while, not fetched on every page.
	c.openrouter.mu.Lock()
	before := c.openrouter.lists
	c.openrouter.mu.Unlock()
	c.svc.Overview(ctx)
	c.openrouter.mu.Lock()
	after := c.openrouter.lists
	c.openrouter.mu.Unlock()
	if after != before {
		t.Errorf("OpenRouter's list was fetched again within its ten minutes (%d → %d)", before, after)
	}
}

func TestHosted_TypeSafeJev(t *testing.T) {
	c := newCloud(t, Options{})
	c.keyAll(t)
	ctx := context.Background()
	// TypeSafe has no list to check against, so any name is taken.
	if saved, err := c.svc.SetModel(ctx, System1, ProviderTypeSafe, "jev-1.14.0", "admin"); err != nil || saved != "jev-1.14.0" {
		t.Fatalf("SetModel = %q, %v", saved, err)
	}
	if _, err := c.svc.SetModel(ctx, LLM, ProviderTypeSafe, "jev-latest", "admin"); err == nil || !strings.Contains(err.Error(), "doesn't serve a LLM") {
		t.Errorf("Jev as the LLM = %v", err)
	}
	r, err := c.svc.Test(ctx, System1)
	if err != nil || !strings.Contains(r.Reply, "96% yes") {
		t.Errorf("Test = %+v, %v", r, err)
	}
	if path, sent, hdr := c.types.last(); path != "/v1/systemone" || sent["model"] != "jev-1.14.0" || hdr.Get("Authorization") != "Bearer ts-key" {
		t.Errorf("TypeSafe got %s model=%v", path, sent["model"])
	}
}

func TestHosted_OpenAIDecisionsAreTranslatedBothWays(t *testing.T) {
	c := newCloud(t, Options{})
	c.keyAll(t)
	if _, err := c.svc.SetModel(context.Background(), System1, ProviderOpenAI, "gpt-6-luna", "admin"); err != nil {
		t.Fatal(err)
	}
	status, body := c.post(t, "/systemone/v1/systemone", c.token("sous"), `{"model":"system1",
		"state":"My order arrived broken.",
		"questions":{
			"upset":{"type":"noul","instructions":"Is the customer upset?"},
			"topic":{"type":"choice","instructions":"What about?","criteria":{"shipping":"delivery","billing":"charges"}},
			"urgency":{"type":"score","instructions":"How urgent?","criteria":["low","mid","high"]},
			"secret":{"type":"noul","criteria":{"true":"mentions a password","false":"doesn't"}}}}`)
	if status != 200 {
		t.Fatalf("%d %s", status, body)
	}

	path, sent, _ := c.openai.last()
	if path != "/v1/decisions" || sent["model"] != "gpt-6-luna" || sent["input"] != "My order arrived broken." {
		t.Fatalf("OpenAI got %s %v", path, sent)
	}
	qs, _ := sent["questions"].([]any)
	var order []string
	for _, q := range qs {
		m := q.(map[string]any)
		order = append(order, m["name"].(string)+":"+m["type"].(string))
	}
	if strings.Join(order, ",") != "upset:predicate,topic:choice,urgency:score,secret:predicate" {
		t.Errorf("questions sent = %v; want the app's order and OpenAI's types", order)
	}
	choices := qs[1].(map[string]any)["choices"].([]any)
	if c0 := choices[0].(map[string]any); c0["value"] != "shipping" || c0["description"] != "delivery" {
		t.Errorf("choices = %v", choices)
	}
	if lv := qs[2].(map[string]any)["levels"].([]any); len(lv) != 3 || lv[2].(map[string]any)["label"] != "high" {
		t.Errorf("levels = %v", lv)
	}
	if ins := qs[3].(map[string]any)["instructions"].(string); !strings.Contains(ins, "mentions a password") {
		t.Errorf("a criteria-only noul was sent as %q", ins)
	}

	var got struct {
		Model   string                    `json:"model"`
		Answers map[string]map[string]any `json:"answers"`
		Usage   map[string]any            `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	a := got.Answers
	if a["upset"]["type"] != "noul" || a["upset"]["noul"] != 0.9 {
		t.Errorf("upset = %v", a["upset"])
	}
	if a["topic"]["choice"] != "shipping" || a["topic"]["probabilities"].(map[string]any)["billing"] != 0.15 || a["topic"]["confidence"] != 0.8 {
		t.Errorf("topic = %v", a["topic"])
	}
	if a["urgency"]["score"] != 2.4 || a["urgency"]["legend"].(map[string]any)["2"] != "high" || a["urgency"]["probabilities"].(map[string]any)["1"] != 0.4 {
		t.Errorf("urgency = %v", a["urgency"])
	}
	if a["secret"]["type"] != "refusal" {
		t.Errorf("secret = %v", a["secret"])
	}
	if got.Model != "gpt-6-luna" || got.Usage["input_tokens"] != float64(40) {
		t.Errorf("model %q usage %v", got.Model, got.Usage)
	}

	for _, bad := range []string{
		`{"state":"x","questions":{"t":{"type":"choice","criteria":{"only":"one"}}}}`,
		`{"state":"x","questions":{"s":{"type":"score","criteria":["one"]}}}`,
		`{"state":"x","assets":{"a":{}},"questions":{"q":{"type":"noul","instructions":"?"}}}`,
		`{"state":"x","questions":{}}`,
	} {
		if status, body := c.post(t, "/systemone/v1/systemone", c.token("sous"), bad); status != 400 {
			t.Errorf("%s: %d %s", bad, status, body)
		}
	}
}

func TestToDecisions_StateWithImagesOrJSON(t *testing.T) {
	q := json.RawMessage(`{"q":{"type":"noul","instructions":"?"}}`)
	b, err := toDecisions(map[string]json.RawMessage{
		"state": json.RawMessage(`{"order":42,"note":"broken"}`), "questions": q,
		"images": json.RawMessage(`["data:image/png;base64,AAAA"]`),
	}, "gpt-6-luna")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{`"type":"input_json"`, `"order":42`, `"type":"input_image"`, `"image_url":"data:image/png;base64,AAAA"`, `"role":"user"`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s lacks %s", s, want)
		}
	}
}

func TestAccess_OnlyTheAppsGivenAKind(t *testing.T) {
	c := newCloud(t, Options{LLM: "gemma4:e4b", System1: "laya:latest", System1Apps: []string{"tasking"}})
	sous, tasking := c.token("sous"), c.token("tasking")

	if status, body := c.post(t, "/systemone/v1/systemone", sous, `{"state":"x","questions":{}}`); status != 403 || !strings.Contains(body, "sous isn't one of the apps given the System 1") {
		t.Errorf("sous, not in config.toml's list: %d %s", status, body)
	}
	if status, _ := c.post(t, "/systemone/v1/systemone", tasking, `{"state":"x","questions":{}}`); status != 200 {
		t.Errorf("tasking: %d", status)
	}
	if status, _ := c.post(t, "/openai/v1/chat/completions", sous, `{"messages":[]}`); status != 200 {
		t.Errorf("sous and the LLM, every app's: %d", status)
	}

	if err := c.svc.SetApps(LLM, false, []string{"tasking", " ", "tasking"}, "admin"); err != nil {
		t.Fatal(err)
	}
	ch := c.svc.Choice(LLM)
	if ch.Every || strings.Join(ch.Apps, ",") != "tasking" || !ch.AppsFromPortal {
		t.Errorf("Choice = %+v", ch)
	}
	if status, _ := c.post(t, "/openai/v1/chat/completions", sous, `{"messages":[]}`); status != 403 {
		t.Errorf("sous after the LLM went to tasking only: %d", status)
	}
	// The model list shows sous nothing now.
	req, _ := http.NewRequest(http.MethodGet, c.url+"/openai/v1/models", nil)
	req.Header.Set("Authorization", "Bearer "+sous)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if strings.Contains(string(b), `"id":"llm"`) || strings.Contains(string(b), `"id":"system1"`) {
		t.Errorf("sous is listed models it can't use: %s", b)
	}

	if err := c.svc.SetApps(System1, true, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if status, _ := c.post(t, "/systemone/v1/systemone", sous, `{"state":"x","questions":{}}`); status != 200 {
		t.Errorf("sous after System 1 went to every app: %d", status)
	}
	if err := c.svc.ResetApps(System1, "admin"); err != nil {
		t.Fatal(err)
	}
	if ch := c.svc.Choice(System1); ch.Every || ch.AppsFromPortal || strings.Join(ch.Apps, ",") != "tasking" {
		t.Errorf("after ResetApps = %+v", ch)
	}
	// No app at all is a choice too.
	if err := c.svc.SetApps(LLM, false, nil, "admin"); err != nil {
		t.Fatal(err)
	}
	if status, _ := c.post(t, "/openai/v1/chat/completions", tasking, `{"messages":[]}`); status != 403 {
		t.Errorf("tasking with the LLM given to no app: %d", status)
	}
}

func TestHosted_MissingKeyAndUnlistedModel(t *testing.T) {
	c := newCloud(t, Options{LLM: "gpt-6", LLMProvider: ProviderOpenAI, System1: "jev-latest", System1Provider: ProviderTypeSafe})
	// config.toml chose hosted models, and no key has been added.
	status, body := c.post(t, "/openai/v1/chat/completions", c.token("sous"), `{"messages":[]}`)
	if status != 503 || !strings.Contains(body, "no API key for OpenAI") {
		t.Errorf("%d %s", status, body)
	}
	o := c.svc.Overview(context.Background())
	if !o.Choices[0].NoKey || !o.Choices[1].NoKey {
		t.Errorf("Overview doesn't say the keys are missing: %+v", o.Choices)
	}
	c.keyAll(t)
	if status, _ := c.post(t, "/openai/v1/chat/completions", c.token("sous"), `{"messages":[]}`); status != 200 {
		t.Errorf("after adding the key: %d", status)
	}
	// A provider's list, once had, says when the chosen model has gone.
	c2 := newCloud(t, Options{LLM: "gpt-5-retired", LLMProvider: ProviderOpenAI})
	c2.keyAll(t)
	if o := c2.svc.Overview(context.Background()); !o.Choices[0].Missing {
		t.Errorf("a model OpenAI doesn't list isn't marked missing: %+v", o.Choices[0])
	}
}

func TestOpen_RefusesAProviderForTheWrongKind(t *testing.T) {
	_, err := Open(Options{Path: filepath.Join(t.TempDir(), "ai.db"), OllamaURL: "http://127.0.0.1:1", Listen: "127.0.0.1:1",
		LLM: "jev-latest", LLMProvider: ProviderTypeSafe})
	if err == nil || !strings.Contains(err.Error(), "TypeSafe can't serve the LLM") {
		t.Errorf("Open = %v", err)
	}
}

func TestStore_UpgradesAV050Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ai.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// ai.db as v0.5.0 wrote it: no provider column, no access table.
	for _, stmt := range []string{
		`CREATE TABLE settings (kind TEXT PRIMARY KEY, model TEXT NOT NULL, updated_at INTEGER NOT NULL, updated_by TEXT NOT NULL)`,
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value BLOB NOT NULL)`,
		`CREATE TABLE tokens (service TEXT PRIMARY KEY, generation INTEGER NOT NULL)`,
		`INSERT INTO settings VALUES ('llm', 'gemma4:e4b', 1, 'root')`,
		`INSERT INTO tokens VALUES ('sous', 2)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	_ = db.Close()

	for i := 0; i < 2; i++ { // and again, already upgraded
		svc, err := Open(Options{Path: path, OllamaURL: "http://127.0.0.1:1", Listen: "127.0.0.1:1"})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if c := svc.Choice(LLM); c.Provider != ProviderOllama || c.Model != "gemma4:e4b" || !c.FromPortal || !c.Every {
			t.Errorf("open %d: Choice = %+v", i, c)
		}
		if svc.store.generation("sous") != 2 {
			t.Errorf("open %d: the token generation was lost", i)
		}
		_ = svc.Close()
	}
}

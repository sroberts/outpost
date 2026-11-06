package ai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// MaxBody bounds a request from an app. Images go inline in both chat APIs
// and in a System 1 state, so it is generous.
const MaxBody = 32 << 20

// maxReply bounds how much of a reply that isn't streamed is held to count
// its tokens. A longer one is passed on uncounted.
const maxReply = 32 << 20

// endpoint is one path apps call: the kind it reaches, and where on Ollama.
type endpoint struct {
	kind     string
	upstream string
}

// endpoints are the paths served. Each protocol has its own prefix, so an
// SDK's base URL can point at it unchanged.
var endpoints = map[string]endpoint{
	"/openai/v1/chat/completions": {LLM, "/v1/chat/completions"},
	"/anthropic/v1/messages":      {LLM, "/v1/messages"},
	"/systemone/v1/systemone":     {System1, "/v1/systemone"},
}

const modelsPath = "/openai/v1/models"

// ServeHTTP is the endpoint apps call.
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	service, ok := s.serviceFor(appToken(r))
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication_error",
			"this endpoint takes the token Outpost gives each app, in OUTPOST_AI_TOKEN")
		return
	}
	if r.URL.Path == modelsPath {
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use GET")
			return
		}
		s.listModels(w)
		return
	}
	ep, ok := endpoints[r.URL.Path]
	if !ok {
		writeError(w, http.StatusNotFound, "not_found_error",
			"Outpost serves /openai/v1/chat/completions, /anthropic/v1/messages and /systemone/v1/systemone")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "invalid_request_error", "use POST")
		return
	}
	s.forward(w, r, service, ep)
}

// appToken is the token an app sent: as a bearer token, which the OpenAI
// SDKs use, or as x-api-key, which Anthropic's do.
func appToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if scheme, tok, ok := strings.Cut(h, " "); ok && strings.EqualFold(scheme, "Bearer") {
			return strings.TrimSpace(tok)
		}
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// listModels answers in the OpenAI shape, with the names apps use.
func (s *Service) listModels(w http.ResponseWriter) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		// Model is what the name reaches now.
		Model string `json:"outpost_model"`
	}
	list := struct {
		Object string  `json:"object"`
		Data   []entry `json:"data"`
	}{Object: "list", Data: []entry{}}
	for _, k := range Kinds {
		if c := s.Choice(k); c.Model != "" {
			list.Data = append(list.Data, entry{ID: k, Object: "model", OwnedBy: "outpost", Model: c.Model})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(jsonBody(list)) // an app that hung up has nothing to be told
}

func (s *Service) forward(w http.ResponseWriter, r *http.Request, service string, ep endpoint) {
	start := s.now()
	model := s.Choice(ep.kind).Model
	fail := func(status int, typ, msg string) {
		writeError(w, status, typ, msg)
		s.count(service, ep.kind, 0, 0, msg)
		s.logf("ai: %s %s %s → %d in %s: %s", service, ep.kind, orNone(model), status, s.since(start), msg)
	}
	if model == "" {
		fail(http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("no %s is configured in Outpost; an admin chooses one on the portal's AI models page", KindName(ep.kind)))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			fail(http.StatusRequestEntityTooLarge, "request_too_large", fmt.Sprintf("the body is over %d MiB", MaxBody>>20))
			return
		}
		fail(http.StatusBadRequest, "invalid_request_error", "the body couldn't be read")
		return
	}
	// The path chooses the model, not the app: whatever it named is replaced.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil || fields == nil {
		fail(http.StatusBadRequest, "invalid_request_error", "the body has to be a JSON object")
		return
	}
	fields["model"] = jsonBody(model)
	out, err := json.Marshal(fields)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", "the body couldn't be re-encoded")
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, s.base+ep.upstream, bytes.NewReader(out))
	if err != nil {
		fail(http.StatusInternalServerError, "api_error", "the request to Ollama couldn't be made")
		return
	}
	// Only what the protocols need. Above all, never the app's token.
	req.Header.Set("Content-Type", "application/json")
	if a := r.Header.Get("Accept"); a != "" {
		req.Header.Set("Accept", a)
	}
	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if r.Context().Err() != nil {
			s.count(service, ep.kind, 0, 0, "the app hung up")
			return
		}
		fail(http.StatusBadGateway, "api_error", fmt.Sprintf("can't reach Ollama at %s", s.base))
		return
	}
	defer func() { _ = resp.Body.Close() }() // read-only

	if resp.StatusCode == http.StatusNotFound {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10)) // only for the message
		fail(http.StatusServiceUnavailable, "api_error",
			fmt.Sprintf("Ollama can't serve %s here (%s); pull it, or choose another on the portal's AI models page", model, ollamaError(b)))
		return
	}

	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	var in, outTok int64
	failure := ""
	if streamed(resp) {
		copyFlushing(w, resp.Body)
	} else {
		b, err := io.ReadAll(io.LimitReader(resp.Body, maxReply+1))
		_, _ = w.Write(b) // an app that hung up has nothing to be told
		if err == nil && len(b) <= maxReply {
			in, outTok = tokens(b)
		} else {
			_, _ = io.Copy(w, resp.Body) // the rest, uncounted
		}
		if resp.StatusCode >= 400 {
			failure = ollamaError(b)
		}
	}
	if resp.StatusCode >= 400 && failure == "" {
		failure = resp.Status
	}
	s.count(service, ep.kind, in, outTok, failure)
	s.logf("ai: %s %s %s → %d in %s (%d in, %d out)", service, ep.kind, model, resp.StatusCode, s.since(start), in, outTok)
}

func (s *Service) since(t time.Time) time.Duration {
	return s.now().Sub(t).Round(time.Millisecond)
}

func orNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// streamed reports whether a reply comes in pieces, to be passed on as each
// arrives.
func streamed(resp *http.Response) bool {
	ct := resp.Header.Get("Content-Type")
	return strings.HasPrefix(ct, "text/event-stream") || strings.HasPrefix(ct, "application/x-ndjson")
}

// copyFlushing passes a stream on, flushing after every read so each event
// reaches the app as Ollama sends it.
func copyFlushing(w http.ResponseWriter, body io.Reader) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return // the app hung up
			}
			_ = rc.Flush() // a writer that can't flush delivers at the end, which is all it can do
		}
		if err != nil {
			return
		}
	}
}

// tokens reads the counts from a reply in either chat API's shape, or the
// System 1 one, which is Anthropic's. Anthropic's input_tokens leaves out
// what came from the prompt cache, which OpenAI's prompt_tokens includes,
// so those are added back: the count is of what the model read.
func tokens(b []byte) (in, out int64) {
	var r struct {
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			InputTokens         int64 `json:"input_tokens"`
			CacheReadTokens     int64 `json:"cache_read_input_tokens"`
			CacheCreationTokens int64 `json:"cache_creation_input_tokens"`
			OutputTokens        int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(b, &r) != nil {
		return 0, 0
	}
	u := r.Usage
	return u.PromptTokens + u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens, u.CompletionTokens + u.OutputTokens
}

// writeError answers in a shape both SDKs read a message from:
// {"error": {"type": …, "message": …}}.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(jsonBody(map[string]any{ // an app that hung up has nothing to be told
		"type":  "error",
		"error": map[string]string{"type": typ, "message": msg},
	}))
}

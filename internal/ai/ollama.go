package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

// Ollama's capabilities that decide a model's kind. A model can have both,
// in which case it is offered for both.
const (
	capCompletion = "completion"
	capDecision   = "decision"
)

// Model is one model Ollama has pulled, as the portal offers it.
type Model struct {
	Name          string    `json:"name"`
	Digest        string    `json:"digest"`
	Size          int64     `json:"size"`
	ModifiedAt    time.Time `json:"modified_at"`
	Family        string    `json:"family,omitempty"`
	ParameterSize string    `json:"parameter_size,omitempty"`
	Quantization  string    `json:"quantization,omitempty"`
	Capabilities  []string  `json:"capabilities,omitempty"`
	// Note says what a hosted provider tells about it: its full name, its
	// context, its price.
	Note string `json:"note,omitempty"`
}

// Serves reports whether the model can be used as kind.
func (m Model) Serves(kind string) bool {
	switch kind {
	case LLM:
		return slices.Contains(m.Capabilities, capCompletion)
	case System1:
		return slices.Contains(m.Capabilities, capDecision)
	}
	return false
}

// ollama asks an Ollama server what it has. Capabilities come from one
// /api/show per model, so they're cached by digest: a model is described
// once until it is pulled again.
type ollama struct {
	base   string
	client *http.Client

	mu   sync.Mutex
	caps map[string][]string // by digest
}

func newOllama(base string, client *http.Client) *ollama {
	return &ollama{base: strings.TrimSuffix(base, "/"), client: client, caps: map[string][]string{}}
}

// models lists what Ollama has pulled, by name, with each one's capabilities.
func (o *ollama) models(ctx context.Context) ([]Model, error) {
	var tags struct {
		Models []struct {
			Name       string    `json:"name"`
			Digest     string    `json:"digest"`
			Size       int64     `json:"size"`
			ModifiedAt time.Time `json:"modified_at"`
			Details    struct {
				Family            string `json:"family"`
				ParameterSize     string `json:"parameter_size"`
				QuantizationLevel string `json:"quantization_level"`
			} `json:"details"`
		} `json:"models"`
	}
	if err := o.call(ctx, http.MethodGet, "/api/tags", nil, &tags); err != nil {
		return nil, err
	}
	out := make([]Model, 0, len(tags.Models))
	for _, t := range tags.Models {
		m := Model{
			Name: t.Name, Digest: t.Digest, Size: t.Size, ModifiedAt: t.ModifiedAt,
			Family: t.Details.Family, ParameterSize: t.Details.ParameterSize, Quantization: t.Details.QuantizationLevel,
		}
		caps, err := o.capabilities(ctx, t.Name, t.Digest)
		if err != nil {
			return nil, err
		}
		m.Capabilities = caps
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (o *ollama) capabilities(ctx context.Context, name, digest string) ([]string, error) {
	o.mu.Lock()
	caps, ok := o.caps[digest]
	o.mu.Unlock()
	if ok && digest != "" {
		return caps, nil
	}
	var show struct {
		Capabilities []string `json:"capabilities"`
	}
	if err := o.call(ctx, http.MethodPost, "/api/show", map[string]string{"model": name}, &show); err != nil {
		return nil, fmt.Errorf("describe %s: %w", name, err)
	}
	if digest != "" {
		o.mu.Lock()
		o.caps[digest] = show.Capabilities
		o.mu.Unlock()
	}
	return show.Capabilities, nil
}

// maxOllamaReply bounds what is read from Ollama's own API: a model list or
// one model's description.
const maxOllamaReply = 8 << 20

func (o *ollama) call(ctx context.Context, method, path string, body, into any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, o.base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return fmt.Errorf("can't reach Ollama at %s: %w", o.base, unwrapURL(err))
	}
	defer func() { _ = resp.Body.Close() }() // read-only
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxOllamaReply))
	if err != nil {
		return fmt.Errorf("read Ollama's reply: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("the Ollama server answered %s: %s", resp.Status, ollamaError(b))
	}
	if err := json.Unmarshal(b, into); err != nil {
		return fmt.Errorf("the Ollama server's reply to %s isn't what Outpost expects: %w", path, err)
	}
	return nil
}

// ollamaError is the message in an Ollama error body, or the body itself.
func ollamaError(b []byte) string {
	var e struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != nil {
		switch v := e.Error.(type) {
		case string:
			return v
		case map[string]any:
			if m, ok := v["message"].(string); ok {
				return m
			}
		}
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	if s == "" {
		return "no message"
	}
	return s
}

// unwrapURL drops net/url's "Get \"http://…\":" prefix, which repeats the
// address the caller's message already gives.
func unwrapURL(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

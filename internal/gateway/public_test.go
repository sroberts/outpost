package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

// publicHarness has tasking with Funnel on and yarr with public paths but
// Funnel off, each with its own tailnet name, and serves the public listener.
func publicHarness(t *testing.T) (*harness, *httptest.Server) {
	t.Helper()
	h := newHarness(t)
	h.g.tailnetHosts = func() map[string]string {
		return map[string]string{"tasking": "tasking.tail.ts.net", "yarr": "yarr.tail.ts.net"}
	}
	on := &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}
	h.withServices(
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/webhooks/github", "/hooks/*"}, Tailnet: on},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", TailscaleName: "yarr",
			PublicPaths: []string{"/webhooks/github"}, Tailnet: &supervisor.TailnetStatus{State: supervisor.TailnetOn}},
	)
	pub := httptest.NewServer(http.HandlerFunc(h.g.servePublic))
	t.Cleanup(pub.Close)
	return h, pub
}

// funnelSend is a request as the node passes it on: the node's name as the
// host, and the client's address as X-Forwarded-For.
func funnelSend(t *testing.T, pub *httptest.Server, method, host, rawPath string, body io.Reader, hdr ...string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, pub.URL+rawPath, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	req.Header.Set("X-Forwarded-For", "203.0.113.7")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func TestPublic_PassesAPublicPathThroughAnonymous(t *testing.T) {
	_, pub := publicHarness(t)
	resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github", strings.NewReader(`{"zen":"ok"}`),
		"X-Outpost-User", "root", "X_Outpost_Admin", "true", "Cookie", "outpost=stolen; other=kept",
		"X-Hub-Signature-256", "sha256=abc")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var s seen
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		t.Fatal(err)
	}
	for k := range s.Header {
		if isIdentityName(k) {
			t.Errorf("the app was sent %s", k)
		}
	}
	if c := s.Header.Get("Cookie"); strings.Contains(c, "outpost=") || !strings.Contains(c, "other=kept") {
		t.Errorf("cookies = %q", c)
	}
	if s.Header.Get("X-Hub-Signature-256") != "sha256=abc" || s.Header.Get("X-Forwarded-For") != "203.0.113.7" || s.Header.Get("X-Forwarded-Proto") != "https" {
		t.Errorf("headers = %v", s.Header)
	}
	if s.Host != "tasking.tail.ts.net" || s.Path != "/webhooks/github" {
		t.Errorf("host %q path %q", s.Host, s.Path)
	}
	if resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/hooks/stripe", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("a wildcard path: %d", resp.StatusCode)
	}
}

// Anything but a funnel app's public path is a bare 404: no login redirect,
// no page, nothing about what's there.
func TestPublic_RefusesEverythingElse(t *testing.T) {
	_, pub := publicHarness(t)
	for name, tc := range map[string]struct{ host, path string }{
		"the app's root":            {"tasking.tail.ts.net", "/"},
		"a path not listed":         {"tasking.tail.ts.net", "/admin"},
		"the gateway's own corner":  {"tasking.tail.ts.net", "/.outpost/callback"},
		"climbing out":              {"tasking.tail.ts.net", "/hooks/../admin"},
		"an escaped slash":          {"tasking.tail.ts.net", "/hooks%2f..%2fadmin"},
		"double encoding":           {"tasking.tail.ts.net", "/hooks/%252e%252e/admin"},
		"a servlet parameter":       {"tasking.tail.ts.net", "/hooks/..;/admin"},
		"an app with Funnel off":    {"yarr.tail.ts.net", "/webhooks/github"},
		"the portal":                {"outpost.test", "/"},
		"the portal's MCP":          {"outpost.test", "/mcp/tasking"},
		"an app's gateway address":  {"tasking.outpost.test", "/webhooks/github"},
		"an unknown name":           {"evil.example", "/webhooks/github"},
		"the jwks, on loopback too": {"127.0.0.1", "/.outpost/jwks.json"},
	} {
		resp := funnelSend(t, pub, http.MethodGet, tc.host, tc.path, nil)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusNotFound || len(body) != 0 || resp.Header.Get("Location") != "" {
			t.Errorf("%s: %d %q location %q", name, resp.StatusCode, body, resp.Header.Get("Location"))
		}
	}
}

// A token is refused, not passed on and not ignored: a leaked one mustn't
// become a way in from outside.
func TestPublic_RefusesAccessTokens(t *testing.T) {
	_, pub := publicHarness(t)
	for _, hdr := range [][]string{
		{"Authorization", "Bearer opat_abc"},
		{"Outpost-Token", "opat_abc"},
	} {
		if resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github", nil, hdr...); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%v: %d", hdr, resp.StatusCode)
		}
	}
	// Webhook senders' own bearer tokens are theirs to check.
	if resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github", nil, "Authorization", "Bearer whsec_123"); resp.StatusCode != http.StatusOK {
		t.Errorf("a sender's own bearer token: %d", resp.StatusCode)
	}
}

func TestPublic_CapsTheBodyAndRefusesUpgrades(t *testing.T) {
	_, pub := publicHarness(t)
	big := bytes.NewReader(make([]byte, MaxPublicBody+1))
	if resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github", big); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the cap: %d", resp.StatusCode)
	}
	if resp := funnelSend(t, pub, http.MethodGet, "tasking.tail.ts.net", "/hooks/ws", nil, "Upgrade", "websocket", "Connection", "Upgrade"); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an upgrade: %d", resp.StatusCode)
	}
}

// Refusals slow an address down; one that keeps scanning is told to wait.
func TestPublic_ThrottlesScanners(t *testing.T) {
	_, pub := publicHarness(t)
	var last int
	for i := 0; i < publicThrottleFree+2; i++ {
		last = funnelSend(t, pub, http.MethodGet, "tasking.tail.ts.net", "/wp-login.php", nil).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d refusals: %d", publicThrottleFree+2, last)
	}
}

// Funnel is only as on as the node says: an app whose Funnel hasn't come up
// is still answered, since the node is the only way in and it decides.
func TestPublic_OnlyAppsWithFunnelSet(t *testing.T) {
	h, pub := publicHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking",
		PublicPaths: []string{"/webhooks/github"}, Tailnet: &supervisor.TailnetStatus{State: supervisor.TailnetOn}})
	if resp := funnelSend(t, pub, http.MethodPost, "tasking.tail.ts.net", "/webhooks/github", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("after funnel was turned off: %d", resp.StatusCode)
	}
}

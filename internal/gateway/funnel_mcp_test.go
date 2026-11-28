package gateway

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// funnelMCPHarness has sous with its MCP endpoint on the internet and
// tasking with Funnel for webhooks and an MCP endpoint that isn't, each with
// its own tailnet name, and alice and bob as users. It serves the public
// listener.
func funnelMCPHarness(t *testing.T, sous ...func(*supervisor.ServiceStatus)) (*harness, *httptest.Server) {
	t.Helper()
	h := newHarness(t)
	h.g.tailnetHosts = func() map[string]string {
		return map[string]string{"sous": "sous.tail.ts.net", "tasking": "tasking.tail.ts.net"}
	}
	on := &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}
	s := supervisor.ServiceStatus{Name: "sous", Status: "running", TailscaleName: "sous",
		MCPPath: "/mcp", MCPFunnel: true, Tailnet: on}
	for _, f := range sous {
		f(&s)
	}
	h.withServices(s,
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/webhooks/github"}, MCPPath: "/mcp", Tailnet: on},
	)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	pub := httptest.NewServer(http.HandlerFunc(h.g.servePublic))
	t.Cleanup(pub.Close)
	return h, pub
}

// internetToken makes an internet token for user and app.
func internetToken(t *testing.T, h *harness, user, app string, ttl time.Duration) string {
	t.Helper()
	secret, _, err := h.store.CreateInternetAccessToken(user, "", app, ttl)
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

func funnelBearer(tok string) []string { return []string{"Authorization", "Bearer " + tok} }

func TestFunnelMCP_AGoodTokenReachesTheApp(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	for name, hdr := range map[string][]string{
		"Authorization": funnelBearer(tok),
		"Outpost-Token": {"Outpost-Token", tok},
	} {
		hdr = append(hdr, "X-Outpost-User", "root", "Cookie", "outpost=stolen; other=kept")
		resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", strings.NewReader(`{"jsonrpc":"2.0"}`), hdr...)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", name, resp.StatusCode)
		}
		var s seen
		if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
			t.Fatal(err)
		}
		if s.Path != "/mcp" || s.Host != "sous.tail.ts.net" {
			t.Errorf("%s: path %q host %q", name, s.Path, s.Host)
		}
		if got := s.Header.Get("X-Outpost-User"); got != "alice" {
			t.Errorf("%s: the app was told the user is %q", name, got)
		}
		if s.Header.Get("X-Outpost-Identity") == "" {
			t.Errorf("%s: no identity token", name)
		}
		if strings.Contains(fmt.Sprint(s.Header), tok) {
			t.Errorf("%s: the app was sent the token: %v", name, s.Header)
		}
		if c := s.Header.Get("Cookie"); strings.Contains(c, "outpost=") || !strings.Contains(c, "other=kept") {
			t.Errorf("%s: cookies = %q", name, c)
		}
		if s.Header.Get("X-Forwarded-For") != "203.0.113.7" || s.Header.Get("X-Forwarded-Proto") != "https" {
			t.Errorf("%s: forwarded as %v", name, s.Header)
		}
	}
	// DELETE ends an MCP session, and goes the same way.
	if resp := funnelSend(t, pub, http.MethodDelete, "sous.tail.ts.net", "/mcp", nil, funnelBearer(tok)...); resp.StatusCode != http.StatusOK {
		t.Errorf("DELETE: %d", resp.StatusCode)
	}
}

// Every token that isn't an internet one for this app alone gets the same
// empty 401 as no token, so nothing tells a holder more than "no".
func TestFunnelMCP_RefusesEveryOtherToken(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	tailnetOnly, _, err := h.store.CreateAccessToken("alice", "", []string{"sous"}, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	unscoped, _, err := h.store.CreateAccessToken("alice", "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	otherApp := internetToken(t, h, "alice", "tasking", time.Hour)
	expired := internetToken(t, h, "alice", "sous", time.Nanosecond)
	disabled := internetToken(t, h, "bob", "sous", time.Hour)
	if err := h.store.SetDisabled("bob", true); err != nil {
		t.Fatal(err)
	}
	good := internetToken(t, h, "alice", "sous", time.Hour)

	i := 0
	for name, hdr := range map[string][]string{
		"no token":               nil,
		"a tailnet token":        funnelBearer(tailnetOnly),
		"an unscoped token":      funnelBearer(unscoped),
		"another app's":          funnelBearer(otherApp),
		"an expired token":       funnelBearer(expired),
		"a disabled user's":      funnelBearer(disabled),
		"a made-up token":        funnelBearer(auth.AccessTokenPrefix + "nope"),
		"two tokens":             append(funnelBearer(good), "Outpost-Token", good),
		"an empty Outpost-Token": {"Outpost-Token", ""},
	} {
		// Each from its own address, so the throttle stays out of it.
		i++
		hdr = append(hdr, "X-Forwarded-For", fmt.Sprintf("203.0.113.%d", 100+i))
		resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, hdr...)
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusUnauthorized || len(body) != 0 || resp.Header.Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("%s: %d %q %v", name, resp.StatusCode, body, resp.Header)
		}
	}
}

// Only the MCP path, exactly, on an app with mcp_funnel on. Everything else is
// what servePublic already answered: a 404, or a 401 for anything carrying a
// token, since tokens are refused before the host and path are looked at.
func TestFunnelMCP_OnlyThatPath(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	sous := internetToken(t, h, "alice", "sous", time.Hour)
	tasking := internetToken(t, h, "alice", "tasking", time.Hour)
	i := 0
	for name, tc := range map[string]struct{ host, path, tok string }{
		"the app's root":            {"sous.tail.ts.net", "/", sous},
		"a trailing slash":          {"sous.tail.ts.net", "/mcp/", sous},
		"below it":                  {"sous.tail.ts.net", "/mcp/x", sous},
		"climbing back to it":       {"sous.tail.ts.net", "/x/../mcp", sous},
		"an escaped letter":         {"sous.tail.ts.net", "/%6dcp", sous},
		"a servlet parameter":       {"sous.tail.ts.net", "/mcp;x", sous},
		"another case":              {"sous.tail.ts.net", "/MCP", sous},
		"an app without mcp_funnel": {"tasking.tail.ts.net", "/mcp", tasking},
		"the app's gateway address": {"sous.outpost.test", "/mcp", sous},
		"the portal's MCP route":    {"outpost.test", "/mcp/sous", sous},
	} {
		i++
		for _, withToken := range []bool{false, true} {
			hdr := []string{"X-Forwarded-For", fmt.Sprintf("198.51.100.%d", 2*i+btoi(withToken))}
			want := http.StatusNotFound
			if withToken {
				hdr = append(hdr, funnelBearer(tc.tok)...)
				want = http.StatusUnauthorized
			}
			resp := funnelSend(t, pub, http.MethodPost, tc.host, tc.path, nil, hdr...)
			if resp.StatusCode != want {
				t.Errorf("%s (token %v): %d; want %d", name, withToken, resp.StatusCode, want)
			}
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// A stranger sending bad tokens from the addresses Claude's connectors share
// can't hold a good token off; their own requests are slowed.
func TestFunnelMCP_AGoodTokenGetsPastTheThrottle(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	good := internetToken(t, h, "alice", "sous", time.Hour)
	shared := []string{"X-Forwarded-For", "160.79.104.10"}
	var last int
	for range 10 {
		last = funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil,
			append(funnelBearer(auth.AccessTokenPrefix+"guess"), shared...)...).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("after 10 bad tokens: %d; want 429", last)
	}
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, append(funnelBearer(good), shared...)...); resp.StatusCode != http.StatusOK {
		t.Errorf("a good token from the backed-off address: %d", resp.StatusCode)
	}
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, append(funnelBearer(auth.AccessTokenPrefix+"guess"), shared...)...); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a bad token after the good one: %d; want still 429", resp.StatusCode)
	}
	// The count is the client's, as X-Forwarded-For has it, not the node's.
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil,
		append(funnelBearer(auth.AccessTokenPrefix+"guess"), "X-Forwarded-For", "203.0.113.50")...); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("another address: %d; want 401", resp.StatusCode)
	}
}

// A scanner with no token at all is counted too, unlike on the portal.
func TestFunnelMCP_ThrottlesRequestsWithNoToken(t *testing.T) {
	_, pub := funnelMCPHarness(t)
	var last int
	for range publicThrottleFree + 2 {
		last = funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil).StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Errorf("after %d requests with no token: %d", publicThrottleFree+2, last)
	}
}

// servePublic's own limits come after the branch, so they're applied again.
func TestFunnelMCP_CapsTheBodyAndRefusesUpgrades(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	big := bytes.NewReader(make([]byte, MaxPublicBody+1))
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", big, funnelBearer(tok)...); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("a body over the cap: %d", resp.StatusCode)
	}
	if resp := funnelSend(t, pub, http.MethodGet, "sous.tail.ts.net", "/mcp", nil,
		append(funnelBearer(tok), "Upgrade", "websocket", "Connection", "Upgrade")...); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("an upgrade: %d", resp.StatusCode)
	}
}

// The allow list still decides, as on the portal's MCP route.
func TestFunnelMCP_TheAllowListStillApplies(t *testing.T) {
	h, pub := funnelMCPHarness(t, func(s *supervisor.ServiceStatus) { s.Allow = []string{"bob"} })
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(tok)...); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a user the allow list leaves out: %d", resp.StatusCode)
	}
}

// No cookie the app sets leaves for the internet.
func TestFunnelMCP_DropsSetCookie(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Set-Cookie", "app=1")
		w.Header().Add("Set-Cookie", "outpost=planted")
		w.Header().Set("Mcp-Session-Id", "abc")
	}))
	t.Cleanup(backend.Close)
	port := backend.Listener.Addr().(*net.TCPAddr).Port
	h, pub := funnelMCPHarness(t, func(s *supervisor.ServiceStatus) { s.Port = port })
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(tok)...)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if c := resp.Header.Values("Set-Cookie"); len(c) != 0 {
		t.Errorf("Set-Cookie reached the client: %q", c)
	}
	if resp.Header.Get("Mcp-Session-Id") != "abc" {
		t.Error("the MCP session header was dropped")
	}
}

// An app that doesn't answer is the same bare 502 Funnel gives elsewhere, not
// a page about the app.
func TestFunnelMCP_AnAppThatIsDownIsABare502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// Accept and close, so the port stays this test's and nothing answers.
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	t.Cleanup(func() { ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	h, pub := funnelMCPHarness(t, func(s *supervisor.ServiceStatus) { s.Port = port })
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(tok)...)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(string(body), "sous") {
		t.Errorf("%d %q", resp.StatusCode, body)
	}
}

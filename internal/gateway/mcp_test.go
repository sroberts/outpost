package gateway

import (
	"bufio"
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

// mcpHarness is a gateway whose yarr serves MCP at mcpPath and whose wiki
// doesn't serve it at all, with alice signed up.
func mcpHarness(t *testing.T, mcpPath string) *harness {
	t.Helper()
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", MCPPath: mcpPath},
		supervisor.ServiceStatus{Name: "wiki", Status: "running"},
	)
	h.addUser(auth.User{Username: "alice", DisplayName: "Alice A", Groups: []string{"family"}})
	return h
}

// assertNotFound checks a plain 404 that never reached an app.
func assertNotFound(t *testing.T, resp *http.Response) {
	t.Helper()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d; want 404", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; want plain text", ct)
	}
}

func TestMCP_RoutesToTheAppsEndpoint(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	s := backendSaw(t, h.send(http.MethodGet, portal+"/mcp/yarr?session=1", bearer(h.pat("alice"))))
	if s.Path != "/mcp?session=1" {
		t.Errorf("the app was asked for %q; want its mcp_path with the query kept", s.Path)
	}
	// The request the app would get on its own address.
	if s.Host != "yarr.outpost.test" {
		t.Errorf("Host = %q; want the app's own host", s.Host)
	}
	if got := s.Header.Get("X-Forwarded-Host"); got != "yarr.outpost.test" {
		t.Errorf("X-Forwarded-Host = %q; want the app's own host", got)
	}
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	// A service that checks the identity token against its own host accepts it.
	if c := identityClaimsOf(t, s.Header.Get("X-Outpost-Identity")); c.Aud != "yarr.outpost.test" || c.Sub != "alice" {
		t.Errorf("identity token aud = %q, sub = %q; want the app's host and alice", c.Aud, c.Sub)
	}

	// An app on a nested path, posted to, as Streamable HTTP does.
	h = mcpHarness(t, "/api/v1/mcp")
	req, err := http.NewRequest(http.MethodPost, portal+"/mcp/yarr", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+h.pat("alice"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Mcp-Session-Id", "abc123")
	resp, err := h.client(false).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	s = backendSaw(t, resp)
	if s.Path != "/api/v1/mcp" {
		t.Errorf("the app was asked for %q", s.Path)
	}
	if got := s.Header.Get("Mcp-Session-Id"); got != "abc123" {
		t.Errorf("Mcp-Session-Id = %q; want it passed through", got)
	}
}

// An MCP client can't follow a sign-in page, and a browser's session is no
// credential for one.
func TestMCP_NoTokenIsA401NotARedirect(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	assertRefused(t, h.send(http.MethodPost, portal+"/mcp/yarr", nil))

	c := h.client(false)
	if resp := h.login(c, "alice", "", "/"); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign-in got %d", resp.StatusCode)
	}
	if resp := h.get(c, portal+"/"); resp.StatusCode != http.StatusOK {
		t.Fatalf("the session doesn't work on the portal: %d", resp.StatusCode)
	}
	assertRefused(t, h.get(c, portal+"/mcp/yarr"))
}

// Whatever is wrong with a token, the answer is the same, so a token can't be
// used to learn which apps exist or which serve MCP.
func TestMCP_EveryFailedTokenLooksTheSame(t *testing.T) {
	type result struct{ status, body, auth, ctype string }
	cases := map[string]func(h *harness) (string, http.Header){
		"bad token": func(h *harness) (string, http.Header) {
			return portal + "/mcp/yarr", bearer(auth.AccessTokenPrefix + "wrong")
		},
		"expired": func(h *harness) (string, http.Header) {
			tok, _, err := h.store.CreateAccessToken("alice", "", nil, time.Nanosecond)
			if err != nil {
				h.t.Fatal(err)
			}
			return portal + "/mcp/yarr", bearer(tok)
		},
		"out of scope": func(h *harness) (string, http.Header) {
			return portal + "/mcp/yarr", bearer(h.pat("alice", "wiki"))
		},
		"unknown app": func(h *harness) (string, http.Header) {
			return portal + "/mcp/nosuch", bearer(h.pat("alice"))
		},
		"no mcp_path": func(h *harness) (string, http.Header) {
			return portal + "/mcp/wiki", bearer(h.pat("alice"))
		},
		"two tokens": func(h *harness) (string, http.Header) {
			tok := h.pat("alice")
			return portal + "/mcp/yarr", http.Header{"Authorization": {"Bearer " + tok}, "Outpost-Token": {tok}}
		},
	}
	var first *result
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			h := mcpHarness(t, "/mcp")
			now := time.Now()
			h.g.now = func() time.Time { return now }
			url, hdr := setup(h)

			resp := h.send(http.MethodPost, url, hdr)
			assertRefused(t, resp)
			got := result{resp.Status, body(t, resp), resp.Header.Get("WWW-Authenticate"), resp.Header.Get("Content-Type")}
			if first == nil {
				first = &got
			} else if got != *first {
				t.Errorf("answer = %+v; another failure got %+v", got, *first)
			}

			// And each counts against the address.
			for i := 1; i <= h.g.tokenThrottle.free; i++ {
				h.send(http.MethodPost, url, hdr)
			}
			if resp := h.send(http.MethodPost, url, hdr); resp.StatusCode != http.StatusTooManyRequests {
				t.Errorf("after %d failures got %d; want 429", h.g.tokenThrottle.free+1, resp.StatusCode)
			}
		})
	}
}

func TestMCP_Throttled(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	now := time.Now()
	h.g.now = func() time.Time { return now }
	good := h.pat("alice")
	bad := bearer(auth.AccessTokenPrefix + "wrong")

	// Asking with no token at all isn't guessing, and isn't counted.
	for i := 0; i < h.g.tokenThrottle.free+2; i++ {
		assertRefused(t, h.send(http.MethodPost, portal+"/mcp/yarr", nil))
	}
	backendSaw(t, h.send(http.MethodPost, portal+"/mcp/yarr", bearer(good)))

	for i := 0; i < h.g.tokenThrottle.free+1; i++ {
		assertRefused(t, h.send(http.MethodPost, portal+"/mcp/yarr", bad))
	}
	// The same table as on an app's own host.
	if resp := h.send(http.MethodGet, yarr+"/", bearer(good)); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a good token on the app's host while throttled got %d", resp.StatusCode)
	}
	if resp := h.send(http.MethodPost, portal+"/mcp/yarr", bearer(good)); resp.StatusCode != http.StatusTooManyRequests {
		t.Errorf("a good token while throttled got %d", resp.StatusCode)
	}
}

func TestMCP_OutpostTokenHeaderWorksAndIsRemoved(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	tok := h.pat("alice")
	hdr := http.Header{"Outpost-Token": {tok}, "Authorization": {"Basic YXBwOnNlY3JldA=="}}
	s := backendSaw(t, h.send(http.MethodPost, portal+"/mcp/yarr", hdr))
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q", got)
	}
	if got := s.Header.Get("Outpost-Token"); got != "" {
		t.Errorf("the app was sent Outpost-Token: %q", got)
	}
	// The app's own credential is the app's.
	if got := s.Header.Get("Authorization"); got != "Basic YXBwOnNlY3JldA==" {
		t.Errorf("Authorization = %q; want the app's own left alone", got)
	}
}

func TestMCP_ClientCantChooseIdentity(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	h.addUser(auth.User{Username: "bob", Admin: true})
	s := backendSaw(t, h.send(http.MethodPost, portal+"/mcp/yarr", http.Header{
		"Authorization":      {"Bearer " + h.pat("alice")},
		"X-Outpost-User":     {"bob"},
		"X_outpost_user":     {"bob"},
		"X-Outpost-Groups":   {"admins"},
		"X-Outpost-Identity": {"forged"},
		// Naming an identity header here would have the proxy drop it,
		// were it set before the proxy rather than on the outgoing copy.
		"Connection": {"X-Outpost-User, X-Outpost-Identity"},
	}))
	if got := s.Header.Get("X-Outpost-User"); got != "alice" {
		t.Errorf("X-Outpost-User = %q; want the token's user", got)
	}
	if got := s.Header.Get("X-Outpost-Groups"); got != "family" {
		t.Errorf("X-Outpost-Groups = %q", got)
	}
	for k := range s.Header {
		if isIdentityName(k) && !strings.HasPrefix(k, "X-Outpost-") {
			t.Errorf("a look-alike header reached the app: %s", k)
		}
	}
	if got := s.Header.Get("X-Outpost-Identity"); got == "forged" || identityClaimsOf(t, got).Sub != "alice" {
		t.Errorf("X-Outpost-Identity = %q", got)
	}
}

func TestMCP_AllowListStillDecides(t *testing.T) {
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", MCPPath: "/mcp", Allow: []string{"bob"}})
	h.addUser(auth.User{Username: "alice"})
	resp := h.send(http.MethodPost, portal+"/mcp/yarr", bearer(h.pat("alice")))
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d; want 403", resp.StatusCode)
	}
}

// The response is on the portal's origin, so an app setting a cookie there
// would set it for the portal and every other app's MCP endpoint.
func TestMCP_AppCantSetCookies(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "app", Value: "1"})
		http.SetCookie(w, &http.Cookie{Name: "outpost", Value: "forged"})
		w.Header().Set("Mcp-Session-Id", "s1")
		_, _ = io.WriteString(w, "{}")
	}))
	t.Cleanup(app.Close)
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", MCPPath: "/mcp",
		Port: app.Listener.Addr().(*net.TCPAddr).Port})
	h.addUser(auth.User{Username: "alice"})

	resp := h.send(http.MethodPost, portal+"/mcp/yarr", bearer(h.pat("alice")))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := resp.Header.Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("Set-Cookie reached the client: %q", got)
	}
	if got := resp.Header.Get("Mcp-Session-Id"); got != "s1" {
		t.Errorf("Mcp-Session-Id = %q; want it passed through", got)
	}
}

func TestMCP_AppDownIsAPlain502(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close() // nothing listens there now
	h := newHarness(t)
	h.withServices(supervisor.ServiceStatus{Name: "yarr", Status: "running", MCPPath: "/mcp", Port: port})
	h.addUser(auth.User{Username: "alice"})

	resp := h.send(http.MethodPost, portal+"/mcp/yarr", bearer(h.pat("alice")))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d; want 502", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q; want plain text, not a page", ct)
	}
	if b := body(t, resp); strings.Contains(b, "<") {
		t.Errorf("body looks like a page: %q", b)
	}
}

// Only /mcp/<name>, exactly and in its simplest form, is routed.
func TestMCP_OnlyTheExactPathIsRouted(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	tok := h.pat("alice")
	for _, p := range []string{
		"/mcp/yarr/x",
		"/mcp/yarr/",
		"/mcp/../x",
		"/mcp/../mcp/yarr",
		"/mcp//yarr",
		"/mcp/yarr%2f..",
		"/mcp/yarr%2F",
		"/mcp/%79arr",
		"/mcp/yarr;x",
		"/mcp/yarr%5c",
		"/mcp/yarr%252f",
		"/mcp/YARR",
		"/mcp/",
	} {
		t.Run(p, func(t *testing.T) {
			assertNotFound(t, h.send(http.MethodPost, portal+p, bearer(tok)))
		})
	}
}

func TestMCP_Streams(t *testing.T) {
	h := mcpHarness(t, "/events")
	resp := h.send(http.MethodGet, portal+"/mcp/yarr", http.Header{
		"Authorization": {"Bearer " + h.pat("alice")},
		"Accept":        {"text/event-stream"},
	})
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	line := make(chan string, 1)
	go func() {
		l, _ := bufio.NewReader(resp.Body).ReadString('\n')
		line <- l
	}()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Errorf("first line = %q", l)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event didn't arrive while the stream was open: the proxy is buffering")
	}
}

// Everywhere else on the portal, a token is still turned away.
func TestMCP_TokensStillRefusedOnTheRestOfThePortal(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	tok := h.pat("alice")
	for _, p := range []string{"/", "/login", "/account", "/mcp", "/mcpx/yarr", "/.well-known/oauth-protected-resource", "/.well-known/jwks.json"} {
		t.Run(p, func(t *testing.T) {
			assertRefused(t, h.send(http.MethodGet, portal+p, bearer(tok)))
		})
	}
}

// A client whose token has stopped working looks for an OAuth server. There is
// none, and it is told so plainly, not handed a page.
func TestMCP_OAuthDiscoveryIsAPlain404(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	for _, p := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp/yarr",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/mcp/yarr",
		"/.well-known/openid-configuration",
	} {
		t.Run(p, func(t *testing.T) {
			assertNotFound(t, h.send(http.MethodGet, portal+p, nil))
		})
	}
	// The keys are still where apps fetch them.
	if resp := h.send(http.MethodGet, portal+"/.well-known/jwks.json", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("jwks.json got %d", resp.StatusCode)
	}
}

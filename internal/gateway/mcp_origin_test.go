package gateway

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// An app's MCP response is served on the portal's origin, so the app mustn't
// be able to make it a page that runs there, have it stored for someone
// else, or set the origin's policy.
func TestMCP_AppCantActAsThePortalOrigin(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Type", "text/html")
		hdr.Set("Cache-Control", "public, max-age=31536000")
		hdr.Set("Access-Control-Allow-Origin", "https://evil.example")
		hdr.Set("Access-Control-Allow-Credentials", "true")
		hdr.Set("Clear-Site-Data", `"cookies"`)
		hdr.Set("Strict-Transport-Security", "max-age=0")
		hdr.Set("Refresh", "0; url=https://evil.example")
		hdr.Set("Link", "<https://evil.example/x.js>; rel=preload; as=script")
		hdr.Set("Mcp-Session-Id", "s1")
		_, _ = io.WriteString(w, "<script>alert(document.domain)</script>")
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
	for _, k := range []string{"Access-Control-Allow-Origin", "Access-Control-Allow-Credentials",
		"Clear-Site-Data", "Strict-Transport-Security", "Refresh", "Link"} {
		if v := resp.Header.Get(k); v != "" {
			t.Errorf("%s = %q reached the client", k, v)
		}
	}
	want := map[string]string{
		"Cache-Control":           "no-store",
		"X-Content-Type-Options":  "nosniff",
		"Content-Security-Policy": "sandbox; default-src 'none'; frame-ancestors 'none'",
		"Mcp-Session-Id":          "s1",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q; want %q", k, got, v)
		}
	}
}

// The app gets the gateway's account of the request and nothing the client
// wrote under another name.
func TestMCP_ClientCantForgeForwardedOrSmuggleToken(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	tok := h.pat("alice")
	hdr := bearer(tok)
	hdr["Forwarded"] = []string{"for=10.9.9.9;host=evil.example;proto=http"}
	hdr["Outpost_Token"] = []string{tok}
	hdr["Outpost.token"] = []string{tok}
	s := backendSaw(t, h.send(http.MethodPost, portal+"/mcp/yarr", hdr))
	if got := s.Header.Get("Forwarded"); got != "" {
		t.Errorf("Forwarded = %q reached the app", got)
	}
	for k, vs := range s.Header {
		if isAccessTokenName(k) {
			t.Errorf("%s = %q reached the app", k, vs)
		}
	}

	// The same on an app's own address.
	s = backendSaw(t, h.send(http.MethodGet, yarr+"/", hdr))
	for k, vs := range s.Header {
		if isAccessTokenName(k) || k == "Forwarded" {
			t.Errorf("%s = %q reached the app", k, vs)
		}
	}
}

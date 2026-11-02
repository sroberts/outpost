package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// One app can't send an MCP client, token and all, on to another app's
// endpoint on the same origin.
func TestMCP_AppCantRedirectTheClient(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/mcp/wiki")
		w.Header().Set("Content-Location", "/mcp/wiki")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	t.Cleanup(app.Close)
	h := newHarness(t)
	h.withServices(
		supervisor.ServiceStatus{Name: "yarr", Status: "running", MCPPath: "/mcp", Port: app.Listener.Addr().(*net.TCPAddr).Port},
		supervisor.ServiceStatus{Name: "wiki", Status: "running", MCPPath: "/mcp"},
	)
	h.addUser(auth.User{Username: "alice"})

	resp := h.send(http.MethodPost, portal+"/mcp/yarr", bearer(h.pat("alice")))
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Errorf("Location = %q reached the client", loc)
	}
	if loc := resp.Header.Get("Content-Location"); loc != "" {
		t.Errorf("Content-Location = %q reached the client", loc)
	}
}

package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/supervisor"
)

// The first use of an internet token from the internet is a line in the
// feed, once; later uses, and refusals, aren't.
func TestFunnelMCP_FirstUseIsInTheFeedOnce(t *testing.T) {
	h, pub := funnelMCPHarness(t)
	f := h.recording()
	tok := internetToken(t, h, "alice", "sous", time.Hour)
	for range 3 {
		if resp := funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(tok)...); resp.StatusCode != http.StatusOK {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	funnelSend(t, pub, http.MethodPost, "sous.tail.ts.net", "/mcp", nil, funnelBearer(auth.AccessTokenPrefix+"nope")...)
	got := f.wait(t, 1)
	if got[0].kind != EventTokenInternet ||
		!strings.Contains(got[0].message, "for alice first used from the internet, from 203.0.113.7, to reach sous") {
		t.Errorf("event = %+v", got[0])
	}
	// Give a second line, if one were coming, time to arrive.
	time.Sleep(100 * time.Millisecond)
	f.wait(t, 1)
}

// An admin sees the Public badge for an app whose MCP endpoint is on the
// internet, saying so; nobody else does, and not before the node's Funnel is.
func TestCatalog_PublicBadgeCoversMCPFunnel(t *testing.T) {
	h := newHarness(t)
	h.g.tailnetHosts = func() map[string]string {
		return map[string]string{"sous": "sous.tail.ts.net", "tasking": "tasking.tail.ts.net", "both": "both.tail.ts.net"}
	}
	on := &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Status: "running", TailscaleName: "sous", MCPPath: "/mcp", MCPFunnel: true, Tailnet: on},
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Funnel: true,
			PublicPaths: []string{"/hook"}, Tailnet: on},
		supervisor.ServiceStatus{Name: "both", Status: "running", TailscaleName: "both", Funnel: true, MCPFunnel: true,
			PublicPaths: []string{"/hook"}, MCPPath: "/mcp", Tailnet: on},
	)
	badges := func(u auth.User) map[string]string {
		out := map[string]string{}
		for _, st := range h.g.stations(u) {
			if st.Public {
				out[st.Name] = st.PublicWhat
			}
		}
		return out
	}
	got := badges(auth.User{Username: "root", Admin: true})
	if !strings.Contains(got["sous"], "its MCP endpoint") || strings.Contains(got["sous"], "public paths") {
		t.Errorf("sous badge = %q", got["sous"])
	}
	if got["tasking"] != "The internet reaches its public paths, through Funnel" {
		t.Errorf("tasking badge = %q", got["tasking"])
	}
	if !strings.Contains(got["both"], "public paths and its MCP endpoint") {
		t.Errorf("both badge = %q", got["both"])
	}
	if got := badges(auth.User{Username: "alice"}); len(got) != 0 {
		t.Errorf("a non-admin sees badges: %v", got)
	}

	h.withServices(supervisor.ServiceStatus{Name: "sous", Status: "running", TailscaleName: "sous", MCPPath: "/mcp", MCPFunnel: true,
		Tailnet: &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelStarting}})
	if got := badges(auth.User{Username: "root", Admin: true}); got["sous"] != "" {
		t.Errorf("badge before the node's Funnel is on: %q", got["sous"])
	}
}

package gateway

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/supervisor"
)

// fakeMCPExposure is fakeExposure that can also set an app's MCP endpoint on
// the internet, refusing an app without a tailnet node as the manifest's
// rules do.
type fakeMCPExposure struct {
	*fakeExposure
	mu     sync.Mutex
	mcp    []string
	mcpOff []string
}

func (f *fakeMCPExposure) SetMCPFunnel(_ context.Context, service string, on bool, by string) error {
	if on && service == "notes" {
		return errors.New("mcp_funnel is served on the service's tailnet node, so it needs Tailscale and a tailscale_name")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mcp = append(f.mcp, service+" "+map[bool]string{true: "on", false: "off"}[on]+" by "+by)
	return nil
}

func (f *fakeMCPExposure) ResetMCPFunnel(_ context.Context, service, by string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mcpOff = append(f.mcpOff, service+" by "+by)
	return nil
}

// mcpExposureHarness has sous, with an MCP endpoint the portal put on the
// internet over its manifest; yarr, with one that is off; and tasking, with
// none.
func mcpExposureHarness(t *testing.T) (*harness, *fakeMCPExposure) {
	h, f := exposureHarness(t)
	m := &fakeMCPExposure{fakeExposure: f}
	h.g.exposure = m
	h.g.tailnetHosts = func() map[string]string {
		return map[string]string{"sous": "sous.tail.ts.net", "yarr": "yarr.tail.ts.net", "tasking": "tasking.tail.ts.net"}
	}
	on := &supervisor.TailnetStatus{State: supervisor.TailnetOn, Funnel: supervisor.FunnelOn}
	h.withServices(
		supervisor.ServiceStatus{Name: "sous", Status: "running", TailscaleName: "sous", MCPPath: "/mcp",
			MCPFunnel: true, MCPFromPortal: true, Tailnet: on},
		supervisor.ServiceStatus{Name: "yarr", Status: "running", TailscaleName: "yarr", MCPPath: "/api/mcp", Tailnet: on},
		supervisor.ServiceStatus{Name: "tasking", Status: "running", TailscaleName: "tasking", Tailnet: on},
		supervisor.ServiceStatus{Name: "notes", Status: "running", MCPPath: "/mcp"},
	)
	return h, m
}

// panel is the part of the catalog that is app's Internet access panel.
func panel(t *testing.T, page, app string) string {
	t.Helper()
	i := strings.Index(page, `id="set-public-`+app+`"`)
	if i < 0 {
		t.Fatalf("no Internet access panel for %s", app)
	}
	rest := page[i:]
	if j := strings.Index(rest, "</details>"); j >= 0 {
		rest = rest[:j]
	}
	return rest
}

// An app with an MCP endpoint has its own switch for it under Internet
// access, saying where the internet reaches it and whether the portal set it.
func TestAppSettings_ShowTheMCPEndpointSwitch(t *testing.T) {
	h, _ := mcpExposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	page := body(t, h.get(c, portal+"/"))

	sous := panel(t, page, "sous")
	for _, want := range []string{
		`value="mcp-save"`, `name="mcp_funnel" value="1" checked`, "https://sous.tail.ts.net/mcp",
		"MCP on the internet", "Set here, over the manifest, which has it off.", `value="mcp-reset"`,
		"--internet --app sous",
	} {
		if !strings.Contains(sous, want) {
			t.Errorf("sous's panel lacks %q:\n%s", want, sous)
		}
	}

	yarr := panel(t, page, "yarr")
	if !strings.Contains(yarr, `name="mcp_funnel" value="1">`) || strings.Contains(yarr, "https://yarr.tail.ts.net/api/mcp") ||
		strings.Contains(yarr, `value="mcp-reset"`) || !strings.Contains(yarr, "tailnet only") {
		t.Errorf("yarr's panel, whose MCP endpoint is off and not set here:\n%s", yarr)
	}

	if strings.Contains(panel(t, page, "tasking"), "mcp-save") {
		t.Error("tasking has no MCP endpoint, but its panel has a switch for one")
	}
	if notes := panel(t, page, "notes"); strings.Contains(notes, `name="mcp_funnel"`) || !strings.Contains(notes, "tailscale_name") {
		t.Errorf("notes has no tailnet node, so no switch, and a reason:\n%s", notes)
	}
}

func TestAppSettings_SavesAndResetsTheMCPEndpoint(t *testing.T) {
	h, f := mcpExposureHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")
	rec := h.recording()

	b := body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-save"}, "app": {"yarr"}, "mcp_funnel": {"1"}}))
	if !strings.Contains(b, "Saved.") || !strings.Contains(b, `id="set-public-yarr" open`) {
		t.Errorf("after saving, the catalog isn't open on yarr's panel with the outcome:\n%s", b)
	}
	h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-save"}, "app": {"sous"}})
	if strings.Join(f.mcp, "|") != "yarr on by root|sous off by root" {
		t.Errorf("mcp = %v", f.mcp)
	}
	if len(f.set) != 0 {
		t.Errorf("the MCP switch changed public paths too: %v", f.set)
	}
	got := rec.wait(t, 2)
	if got[0].kind != EventExposure || !strings.Contains(got[0].message, "root put yarr's MCP endpoint /api/mcp on the internet") ||
		!strings.Contains(got[1].message, "root took sous's MCP endpoint /mcp off the internet") {
		t.Errorf("feed = %+v", got)
	}

	if b := body(t, h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-reset"}, "app": {"sous"}})); !strings.Contains(b, "The MCP endpoint is back to what the manifest says.") {
		t.Errorf("after resetting:\n%s", b)
	}
	if strings.Join(f.mcpOff, "|") != "sous by root" || len(f.resets) != 0 {
		t.Errorf("mcp resets = %v, public resets = %v", f.mcpOff, f.resets)
	}

	resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-save"}, "app": {"notes"}, "mcp_funnel": {"1"}})
	if b := body(t, resp); resp.StatusCode != 400 || !strings.Contains(b, "needs Tailscale") || !strings.Contains(b, `id="set-public-notes" open`) {
		t.Errorf("a refused setting: %d\n%s", resp.StatusCode, b)
	}
	// An app with no MCP endpoint has nothing to switch.
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-save"}, "app": {"tasking"}, "mcp_funnel": {"1"}}); resp.StatusCode != 400 {
		t.Errorf("tasking, with no mcp_path = %d", resp.StatusCode)
	}
	if strings.Contains(strings.Join(f.mcp, "|"), "tasking") || strings.Contains(strings.Join(f.mcp, "|"), "notes") {
		t.Errorf("a refused request was saved: %v", f.mcp)
	}
}

func TestAppSettings_MCPEndpointOnlyForAnAdmin(t *testing.T) {
	h, f := mcpExposureHarness(t)
	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, "mcp_funnel") {
		t.Error("a plain user is shown the MCP endpoint switch")
	}
	save := url.Values{"action": {"mcp-save"}, "app": {"yarr"}, "mcp_funnel": {"1"}}
	if resp := h.post(plain, portal+"/settings", portal, save); resp.StatusCode != 403 {
		t.Errorf("POST as a plain user = %d", resp.StatusCode)
	}
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	if resp := h.post(admin, portal+"/settings", "http://evil.test", save); resp.StatusCode != 403 {
		t.Errorf("POST from another site = %d", resp.StatusCode)
	}
	if len(f.mcp) != 0 {
		t.Errorf("a refused request was saved: %v", f.mcp)
	}
}

// With an Exposure that can't set it, there is no switch, and the form is
// refused.
func TestAppSettings_NoMCPSwitchWithoutTheStore(t *testing.T) {
	h, _ := mcpExposureHarness(t)
	h.g.exposure = &fakeExposure{}
	c := h.client(true)
	h.login(c, "root", "", "/")
	if b := body(t, h.get(c, portal+"/")); strings.Contains(b, "mcp-save") {
		t.Error("a switch the Exposure can't set is shown")
	}
	if resp := h.post(c, portal+"/settings", portal, url.Values{"action": {"mcp-save"}, "app": {"sous"}, "mcp_funnel": {"1"}}); resp.StatusCode != 400 {
		t.Errorf("POST = %d", resp.StatusCode)
	}
}

// The panel's summary counts the MCP endpoint: on and failing is down, not
// "tailnet only".
func TestPublicRow_HealthCountsTheMCPEndpoint(t *testing.T) {
	rt := route{Name: "sous", Status: "running", TailscaleName: "sous", MCPPath: "/mcp", MCPFunnel: true,
		FunnelState: "unavailable", FunnelError: "no funnel attribute"}
	if row := publicRowFor(rt); row.Health != "down" {
		t.Errorf("failing = %s (%s); want down", row.Health, row.HealthWords)
	}
	rt.FunnelState = ""
	if row := publicRowFor(rt); row.Health != "starting" {
		t.Errorf("on its way = %s (%s); want starting", row.Health, row.HealthWords)
	}
	if m := mcpRowFor(rt); m == nil || m.URL != "" || m.State != "Starting." {
		t.Errorf("mcp row on its way = %+v", m)
	}
}

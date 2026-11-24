package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/manifest"
)

// gatewayOnlyHarness is the usual harness with yarr declared visibility =
// "gateway", while a node that should not exist still reports a tailnet
// name for it.
func gatewayOnlyHarness(t *testing.T) *harness {
	h := newHarness(t)
	svcs := h.g.services.(*fakeServices)
	svcs.mu.Lock()
	for i := range svcs.list {
		if svcs.list[i].Name == "yarr" {
			svcs.list[i].Visibility = manifest.VisibilityGateway
		}
	}
	svcs.mu.Unlock()
	h.g.tailnetHosts = func() map[string]string { return map[string]string{"yarr": yarrNode} }
	h.g.refreshRoutes(context.Background())
	return h
}

// A gateway-only app is served at its gateway address, behind the login, and
// never at a tailnet name.
func TestGatewayOnly_BehindTheLoginAndOffTheTailnet(t *testing.T) {
	h := gatewayOnlyHarness(t)
	c := h.client(false)

	if resp := h.get(c, "http://"+yarrNode+"/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET on a tailnet name = %d; want 404, as the app has no node", resp.StatusCode)
	}

	resp := h.get(c, yarr+"/feeds", "X-Outpost-User", "mallory")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET without a session = %d; want a redirect to log in", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "outpost.test" || loc.Path != "/login" || loc.Query().Get("app") != "yarr" {
		t.Errorf("sent to %s; want the portal's login for yarr", loc)
	}

	h.addUser(auth.User{Username: "alice"})
	b := h.client(true)
	h.login(b, "alice", "yarr", "/feeds")
	got := backendSaw(t, h.get(b, yarr+"/feeds", "X-Outpost-User", "mallory"))
	if got.Header.Get("X-Outpost-User") != "alice" || got.Host != "yarr.outpost.test" {
		t.Errorf("app saw user %q at host %q; want alice at its gateway address", got.Header.Get("X-Outpost-User"), got.Host)
	}
}

// The catalog links a gateway-only app at its gateway address, and marks it
// for an admin only.
func TestGatewayOnly_CatalogLinksAndMarksIt(t *testing.T) {
	h := gatewayOnlyHarness(t)
	h.addUser(auth.User{Username: "root", Admin: true})
	h.addUser(auth.User{Username: "alice"})

	admin := body(t, h.login(h.client(true), "root", "", "/"))
	if !strings.Contains(admin, `href="http://yarr.outpost.test/"`) || strings.Contains(admin, yarrNode) {
		t.Errorf("portal should link yarr at its gateway address and nowhere on the tailnet:\n%s", admin)
	}
	if strings.Count(admin, ">Gateway only</span>") != 1 {
		t.Errorf("admin should see one Gateway only tag, on yarr:\n%s", admin)
	}

	user := body(t, h.login(h.client(true), "alice", "", "/"))
	if strings.Contains(user, "Gateway only") {
		t.Errorf("a non-admin was shown the tag:\n%s", user)
	}
}

// Funnel needs a node, so the portal's Internet access setting can't offer
// it for a gateway-only app, and says why.
func TestGatewayOnly_NoFunnelOffered(t *testing.T) {
	rt := route{Name: "yarr", TailscaleName: "yarr", GatewayOnly: true}
	row := publicRowFor(rt)
	if row.CanFunnel || !row.GatewayOnly || row.HealthWords != "gateway only" {
		t.Errorf("row = %+v; want no Funnel, said to be gateway only", row)
	}
}

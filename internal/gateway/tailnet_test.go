package gateway

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
)

const yarrNode = "yarr-1.example.ts.net"

// tailnetHarness is the usual harness with yarr also on the tailnet, under
// a name that isn't its own, as Tailscale gives out when one is taken.
func tailnetHarness(t *testing.T) *harness {
	h := newHarness(t)
	h.g.tailnetHosts = func() map[string]string { return map[string]string{"yarr": yarrNode} }
	h.g.refreshRoutes(context.Background())
	return h
}

func TestTailnetHost_RoutesBehindTheLogin(t *testing.T) {
	h := tailnetHarness(t)
	c := h.client(false)

	resp := h.get(c, "http://"+yarrNode+"/feeds", "X-Outpost-User", "mallory")
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("GET on the tailnet name = %d; want a redirect to log in", resp.StatusCode)
	}
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Host != "outpost.test" || loc.Path != "/login" || loc.Query().Get("app") != "yarr" {
		t.Errorf("sent to %s; want the portal's login for yarr", loc)
	}
	// Some other node's name is nothing to do with Outpost.
	if resp := h.get(c, "http://wiki-1.example.ts.net/"); resp.StatusCode != http.StatusNotFound {
		t.Errorf("an unknown tailnet name = %d; want 404", resp.StatusCode)
	}
}

func TestTailnetHost_HandshakeLandsOnTheNode(t *testing.T) {
	h := tailnetHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)

	resp := h.login(c, "alice", "yarr", "/feeds")
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if loc.Scheme != "https" || loc.Host != yarrNode || loc.Path != "/.outpost/callback" {
		t.Fatalf("login sent us to %s; want the node's https callback", loc)
	}

	// The node terminates TLS, so the gateway itself sees plain http. The
	// callback goes round through the portal once, to tie the sign-in to
	// this browser, and comes back to the node's https callback to land.
	loc.Scheme = "http"
	back, err := h.get(c, loc.String()).Location()
	if err != nil || back.Host != "outpost.test" || back.Path != "/continue" {
		t.Fatalf("first callback hop went to %v, %v; want the portal's /continue", back, err)
	}
	loc, err = h.get(c, back.String()).Location()
	if err != nil || loc.Scheme != "https" || loc.Host != yarrNode || loc.Path != "/.outpost/callback" {
		t.Fatalf("portal sent us to %v, %v; want the node's https callback", loc, err)
	}
	loc.Scheme = "http"
	resp = h.get(c, loc.String())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/feeds" {
		t.Fatalf("callback = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	got := backendSaw(t, h.get(c, "http://"+yarrNode+"/feeds"))
	if got.Header.Get("X-Outpost-User") != "alice" || got.Host != yarrNode {
		t.Errorf("app saw user %q at host %q", got.Header.Get("X-Outpost-User"), got.Host)
	}
}

func TestTailnetHost_PortalLinksThere(t *testing.T) {
	h := tailnetHarness(t)
	h.addUser(auth.User{Username: "alice"})
	b := body(t, h.login(h.client(true), "alice", "", "/"))
	if !strings.Contains(b, `href="https://`+yarrNode+`/"`) {
		t.Errorf("portal doesn't link yarr's tailnet address:\n%s", b)
	}
	// wiki has no node, so it keeps its <name>.<domain> address.
	if !strings.Contains(b, `href="http://wiki.outpost.test/"`) {
		t.Errorf("portal lost wiki's address:\n%s", b)
	}
	for _, r := range h.g.Status().Routes {
		if r.Name == "yarr" && r.URL != "https://"+yarrNode {
			t.Errorf("gateway status says yarr is at %s", r.URL)
		}
	}
}

func TestLocalUpstream(t *testing.T) {
	cases := []struct {
		cfg        config.Gateway
		want, name string
	}{
		{config.Gateway{PublicURL: "https://mm.example.ts.net"}, "http://127.0.0.1:8443", ""},
		{config.Gateway{PublicURL: "https://o.example", Listen: "0.0.0.0:9000"}, "http://127.0.0.1:9000", ""},
		{config.Gateway{PublicURL: "https://o.example", Listen: ":9000"}, "http://127.0.0.1:9000", ""},
		{config.Gateway{PublicURL: "https://o.example", TLSCert: "c", TLSKey: "k"}, "https://127.0.0.1:8443", "o.example"},
	}
	for _, tc := range cases {
		got, name := LocalUpstream(tc.cfg)
		if got != tc.want || name != tc.name {
			t.Errorf("LocalUpstream(%+v) = %q, %q; want %q, %q", tc.cfg, got, name, tc.want, tc.name)
		}
	}
}

func TestStatus_SaysWhetherAppsAreOnTheTailnet(t *testing.T) {
	if newHarness(t).g.Status().Tailnet {
		t.Error("a gateway without tailnet hosts says apps are on the tailnet")
	}
	if !tailnetHarness(t).g.Status().Tailnet {
		t.Error("a gateway with tailnet hosts doesn't say so")
	}
}

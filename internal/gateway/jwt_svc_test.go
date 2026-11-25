package gateway

import (
	"net/http"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// svc names the service the token was minted for, whichever of its hosts the
// request came to: a service checks it against OUTPOST_SERVICE, a name that
// doesn't change when Tailscale renames its node (#149).
func TestIdentityToken_SvcIsTheServiceAtEveryHost(t *testing.T) {
	h := tailnetHarness(t)
	h.addUser(auth.User{Username: "alice"})
	secret, _, err := h.store.CreateAccessToken("alice", "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"yarr.outpost.test", yarrNode} {
		got := backendSaw(t, h.send(http.MethodGet, "http://"+host+"/x", bearer(secret)))
		if svc := identityClaimsOf(t, got.Header.Get("X-Outpost-Identity")).Svc; svc != "yarr" {
			t.Errorf("at %s: svc = %q; want yarr", host, svc)
		}
	}
}

func TestIdentityToken_SvcThroughThePortalsMCPRoute(t *testing.T) {
	h := mcpHarness(t, "/mcp")
	s := backendSaw(t, h.send(http.MethodGet, portal+"/mcp/yarr", bearer(h.pat("alice"))))
	if svc := identityClaimsOf(t, s.Header.Get("X-Outpost-Identity")).Svc; svc != "yarr" {
		t.Errorf("svc = %q; want yarr", svc)
	}
}

// The cache is keyed on the service too: one user at one host is never
// handed a token another service was given.
func TestIdentityToken_CacheKeepsServicesApart(t *testing.T) {
	h := newHarness(t)
	u := auth.User{Username: "alice"}
	a, err := h.g.identityToken(u, "shared.outpost.test", "yarr")
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.g.identityToken(u, "shared.outpost.test", "wiki")
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("two services were handed the same token")
	}
	if svc := identityClaimsOf(t, b).Svc; svc != "wiki" {
		t.Errorf("svc = %q; want wiki", svc)
	}
}

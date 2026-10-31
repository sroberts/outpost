package gateway

import (
	"net/http"
	"slices"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// aud is the host the request came to. An app with a tailnet name and a
// subdomain is served at both, and a token that named the other one would
// teach the app to skip the check.
func TestIdentityToken_AudIsTheHostAskedFor(t *testing.T) {
	h := tailnetHarness(t)
	h.addUser(auth.User{Username: "alice"})
	secret, _, err := h.store.CreateAccessToken("alice", "test", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range []string{"yarr.outpost.test", yarrNode} {
		got := backendSaw(t, h.send(http.MethodGet, "http://"+host+"/x", bearer(secret)))
		if aud := identityClaimsOf(t, got.Header.Get("X-Outpost-Identity")).Aud; aud != host {
			t.Errorf("at %s: aud = %q", host, aud)
		}
	}
}

// A cached token must say what the headers say: a user whose groups change
// gets a fresh one, not the last half-minute's.
func TestIdentityToken_FollowsTheClaims(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice", Groups: []string{"family"}})
	c := h.client(true)
	got := backendSaw(t, h.login(c, "alice", "yarr", "/"))
	if claims := identityClaimsOf(t, got.Header.Get("X-Outpost-Identity")); !slices.Equal(claims.Groups, []string{"family"}) {
		t.Fatalf("groups = %v", claims.Groups)
	}
	if _, err := h.store.UpdateGroups("alice", []string{"admins"}, []string{"family"}); err != nil {
		t.Fatal(err)
	}
	got = backendSaw(t, h.get(c, yarr+"/x"))
	claims := identityClaimsOf(t, got.Header.Get("X-Outpost-Identity"))
	if !slices.Equal(claims.Groups, []string{"admins"}) {
		t.Errorf("token groups = %v after the change; header says %q", claims.Groups, got.Header.Get("X-Outpost-Groups"))
	}
}

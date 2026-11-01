package gateway

import (
	"slices"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// The handshake's stamp is the gateway's too: an app can't set or clear it,
// under either name, while its own cookies pass as sent.
func TestProxy_DropsAppSetStateCookie(t *testing.T) {
	h := newHarness(t)
	cookieSetter(h,
		"outpost-state=evil; Path=/",
		"app=1",
		"__Host-outpost-state=evil; Path=/; Secure",
		"outpost-state=; Max-Age=0",
		"outpost-states=fine",
	)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "yarr", "/")

	resp := h.get(c, yarr+"/")
	want := []string{"app=1", "outpost-states=fine"}
	if got := resp.Header.Values("Set-Cookie"); !slices.Equal(got, want) {
		t.Errorf("Set-Cookie reaching the client = %q; want %q", got, want)
	}
	if got := resp.Header.Get("X-Saw-User"); got != "alice" {
		t.Errorf("app saw user %q; want alice", got)
	}
}

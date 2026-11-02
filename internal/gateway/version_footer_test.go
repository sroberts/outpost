package gateway

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/version"
)

// Someone signed in can see what's running; someone who isn't shouldn't learn
// which build to aim at.
func TestFooterNamesTheBuildOnlyWhenSignedIn(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	want := `<footer class="foot">Outpost ` + version.Get().Version + `</footer>`

	c := h.client(true)
	if b := body(t, h.get(c, portal+"/login")); strings.Contains(b, "<footer") {
		t.Errorf("the sign-in page gives away the build:\n%s", b)
	}
	if b := body(t, h.login(c, "alice", "", "/")); !strings.Contains(b, want) {
		t.Errorf("the catalog lacks %q:\n%s", want, b)
	}
	if b := body(t, h.get(c, portal+"/account")); !strings.Contains(b, want) {
		t.Errorf("the account page lacks %q:\n%s", want, b)
	}
}

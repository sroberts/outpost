package gateway

import (
	"net/http"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// Another port on the portal's name is another origin: what answers there
// may be an app, and an app, or a script in one, mustn't be able to sign the
// user out everywhere or start an update.
func TestCSRF_AnotherPortIsAnotherOrigin(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})

	for _, origin := range []string{"http://outpost.test:3000", "http://outpost.test:443", "https://outpost.test:80"} {
		if resp := h.post(h.client(false), portal+"/logout", origin, nil); resp.StatusCode != http.StatusForbidden {
			t.Errorf("logout with Origin %q = %d; want 403", origin, resp.StatusCode)
		}
	}
	// The scheme's default port is the same origin, written out or not.
	for _, origin := range []string{"http://outpost.test:80", "http://OUTPOST.test.", "http://outpost.test"} {
		if resp := h.post(h.client(false), portal+"/logout", origin, nil); resp.StatusCode != http.StatusSeeOther {
			t.Errorf("logout with Origin %q = %d; want 303", origin, resp.StatusCode)
		}
	}
}

func TestOrigin(t *testing.T) {
	for _, c := range []struct{ scheme, host, want string }{
		{"http", "outpost.test", "http://outpost.test"},
		{"http", "outpost.test:80", "http://outpost.test"},
		{"HTTPS", "Outpost.Test.:443", "https://outpost.test"},
		{"https", "outpost.test:8443", "https://outpost.test:8443"},
		{"http", "[::1]:8080", "http://[::1]:8080"},
		{"http", "[::1]", "http://[::1]"},
	} {
		if got := origin(c.scheme, c.host); got != c.want {
			t.Errorf("origin(%q, %q) = %q; want %q", c.scheme, c.host, got, c.want)
		}
	}
}

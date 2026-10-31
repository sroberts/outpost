package gateway

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/supervisor"
)

// cookieSetter is yarr, answering every request with these Set-Cookie
// headers and the user the gateway said was calling.
func cookieSetter(h *harness, setCookies ...string) {
	h.t.Helper()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, c := range setCookies {
			w.Header().Add("Set-Cookie", c)
		}
		w.Header().Set("X-Saw-User", r.Header.Get("X-Outpost-User"))
	}))
	h.t.Cleanup(backend.Close)
	h.withServices(supervisor.ServiceStatus{
		Name: "yarr", Status: "running", Port: backend.Listener.Addr().(*net.TCPAddr).Port,
	})
}

// Both of the gateway's cookie names, whichever it uses, and the nameless
// form a browser would send back as one of them.
var plantedCookies = []string{
	"outpost=evil; Path=/",
	"app=1",
	"__Host-outpost=evil; Path=/; Secure",
	" outpost =evil; Domain=outpost.test; Path=/x",
	"=outpost=evil",
	"outpost",
	"app2=2; Path=/",
	"outposts=fine",
}

var keptCookies = []string{"app=1", "app2=2; Path=/", "outposts=fine"}

// An app can't overwrite, clear or plant the gateway's session cookie.
func TestProxy_DropsAppSetSessionCookie(t *testing.T) {
	h := newHarness(t)
	cookieSetter(h, plantedCookies...)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(true)
	h.login(c, "alice", "yarr", "/")
	appURL, _ := url.Parse(yarr + "/")

	resp := h.get(c, yarr+"/")
	if got := resp.Header.Values("Set-Cookie"); !slices.Equal(got, keptCookies) {
		t.Errorf("Set-Cookie reaching the client = %q; want %q", got, keptCookies)
	}
	if got := resp.Header.Get("X-Saw-User"); got != "alice" {
		t.Fatalf("app saw user %q; want alice", got)
	}
	for _, ck := range c.Jar.Cookies(appURL) {
		if ck.Name == "outpost" && ck.Value == "evil" {
			t.Errorf("the app replaced the session cookie")
		}
	}
}

// Under https the cookie is __Host-outpost; that, and the http name, are
// both kept from the app.
func TestProxy_DropsAppSetSessionCookieUnderHTTPS(t *testing.T) {
	h := newHarness(t, func(c *config.Gateway) { c.PublicURL = "https://outpost.test" })
	cookieSetter(h, plantedCookies...)
	h.addUser(auth.User{Username: "alice"})

	// A token, because the test client won't send a Secure cookie over the
	// harness's plain http; the response goes through the same proxy.
	resp := h.send(http.MethodGet, yarr+"/", bearer(h.pat("alice")))
	if got := resp.Header.Get("X-Saw-User"); got != "alice" {
		t.Fatalf("app saw user %q; want alice", got)
	}
	if got := resp.Header.Values("Set-Cookie"); !slices.Equal(got, keptCookies) {
		t.Errorf("Set-Cookie reaching the client = %q; want %q", got, keptCookies)
	}
}

// An app that sets no session cookie has its headers passed through as sent.
func TestRemoveSetCookies_LeavesOthersAlone(t *testing.T) {
	hdr := http.Header{"Set-Cookie": {"b=2", "a=1"}}
	removeSetCookies(hdr, sessionCookieNames)
	if got := hdr.Values("Set-Cookie"); !slices.Equal(got, []string{"b=2", "a=1"}) {
		t.Errorf("Set-Cookie = %q; want both, in order", got)
	}
}

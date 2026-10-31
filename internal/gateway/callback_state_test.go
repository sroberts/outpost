package gateway

import (
	"net/http"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/auth"
)

// A sign-in link made in one browser used to work in any other: mallory
// signs in with app=yarr, keeps the callback link, and alice opening it
// would have been signed in to yarr as mallory, her own session gone.
func TestCallback_LinkFromAnotherBrowserIsRefused(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "mallory"})
	alice := h.client(true)
	backendSaw(t, h.login(alice, "alice", "yarr", "/"))

	mallory := h.client(false)
	loc, err := h.login(mallory, "mallory", "yarr", "/notes").Location()
	if err != nil || loc.Path != "/.outpost/callback" {
		t.Fatalf("mallory's login didn't hand off to yarr: %v", loc)
	}

	resp := h.get(alice, loc.String())
	if resp.StatusCode == http.StatusOK && strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatal("mallory's link signed alice's browser in")
	}
	if b := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "another browser") {
		t.Errorf("mallory's link in alice's browser = %d\n%s", resp.StatusCode, b)
	}
	// Alice is still herself at yarr.
	if got := backendSaw(t, h.get(alice, yarr+"/x")); got.Header.Get("X-Outpost-User") != "alice" {
		t.Errorf("alice's session at yarr became %q", got.Header.Get("X-Outpost-User"))
	}
}

// The handshake, hop by hop: the first callback stamps the browser and sends
// it to the portal; the portal checks the session and sends it back; only
// then, with the stamp, does the callback hand out a session.
func TestCallback_HandshakeStampsTheBrowser(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	c := h.client(false)

	first, err := h.login(c, "alice", "yarr", "/feeds").Location()
	if err != nil {
		t.Fatal(err)
	}
	resp := h.get(c, first.String())
	back, err := resp.Location()
	if err != nil || resp.StatusCode != http.StatusSeeOther || back.Host != "outpost.test" || back.Path != "/continue" {
		t.Fatalf("first hop = %d to %v; want the portal's /continue", resp.StatusCode, back)
	}
	if !strings.Contains(resp.Header.Get("Set-Cookie"), "outpost-state=") {
		t.Errorf("first hop set no stamp: %q", resp.Header.Values("Set-Cookie"))
	}
	if back.Query().Get("next") != "/feeds" {
		t.Errorf("next was lost on the way to the portal: %s", back)
	}

	resp = h.get(c, back.String())
	last, err := resp.Location()
	if err != nil || resp.StatusCode != http.StatusSeeOther || last.Host != "yarr.outpost.test" || last.Path != "/.outpost/callback" {
		t.Fatalf("portal hop = %d to %v; want yarr's callback", resp.StatusCode, last)
	}
	if last.Query().Get("state") == "" || last.Query().Get("code") == "" {
		t.Fatalf("the last link lacks a code or a stamp: %s", last)
	}

	// The same link in a browser without the stamp is no good, and is spent.
	other := h.client(false)
	if resp := h.get(other, last.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the last link worked without the stamp: %d", resp.StatusCode)
	}
	if resp := h.get(c, last.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("the last link survived being used elsewhere: %d", resp.StatusCode)
	}

	// Done properly, it lands where it was going, with the stamp gone.
	first, _ = h.login(c, "alice", "yarr", "/feeds").Location()
	back, _ = h.get(c, first.String()).Location()
	last, _ = h.get(c, back.String()).Location()
	resp = h.get(c, last.String())
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/feeds" {
		t.Fatalf("last hop = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var cleared bool
	for _, sc := range resp.Header.Values("Set-Cookie") {
		if strings.HasPrefix(sc, "outpost-state=") && strings.Contains(sc, "Max-Age=0") {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("the stamp wasn't cleared: %q", resp.Header.Values("Set-Cookie"))
	}
	got := backendSaw(t, h.get(c, yarr+"/feeds"))
	if got.Header.Get("X-Outpost-User") != "alice" {
		t.Errorf("signed in as %q", got.Header.Get("X-Outpost-User"))
	}
	if strings.Contains(got.Header.Get("Cookie"), "outpost-state=") {
		t.Errorf("the app was handed the stamp: %q", got.Header.Get("Cookie"))
	}
}

// A stamp with the wrong code, or a portal hop for someone else's sign-in,
// is refused: the link alone, without the matching browser, is worthless.
func TestContinue_NeedsTheSessionThatSignedIn(t *testing.T) {
	h := newHarness(t)
	h.addUser(auth.User{Username: "alice"})
	h.addUser(auth.User{Username: "bob"})
	alice := h.client(false)
	bob := h.client(false)
	h.login(bob, "bob", "", "/")

	first, _ := h.login(alice, "alice", "yarr", "/").Location()
	back, _ := h.get(alice, first.String()).Location()

	// Bob's browser, with bob's session, can't continue alice's sign-in.
	if resp := h.get(bob, back.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bob continued alice's sign-in: %d", resp.StatusCode)
	}
	// And the link is spent, even for alice.
	if resp := h.get(alice, back.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a continue link worked twice: %d", resp.StatusCode)
	}

	// A browser with no session at all is sent to sign in, not signed in.
	first, _ = h.login(alice, "alice", "yarr", "/").Location()
	back, _ = h.get(alice, first.String()).Location()
	resp := h.get(h.client(false), back.String())
	if b := body(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(b, "/login?app=yarr") {
		t.Errorf("no session at the portal hop = %d\n%s", resp.StatusCode, b)
	}

	// The stamp in the link has to be the code's own.
	first, _ = h.login(alice, "alice", "yarr", "/").Location()
	back, _ = h.get(alice, first.String()).Location()
	q := back.Query()
	q.Set("state", "someone-elses")
	back.RawQuery = q.Encode()
	if resp := h.get(alice, back.String()); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("a swapped stamp was accepted: %d", resp.StatusCode)
	}
}

package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// The update page is always a tap away for an admin, since a check can be asked
// for there, and the link says so when there's something to install.
func TestCatalogLinksUpdatesAndSaysWhenOneIsWaiting(t *testing.T) {
	h, f := updateHarness(t)
	f.latest["yarr"] = ReleaseCheck{Latest: "v1.0.0"} // what's installed
	c := h.client(true)
	h.login(c, "root", "", "/")
	h.settled()

	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/updates">Updates</a>`) || strings.Contains(b, "available</a>") {
		t.Errorf("with none waiting, the catalog should link to updates plainly:\n%s", b)
	}
	if resp := h.get(c, portal+"/updates"); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /updates = %d; the page should still be there", resp.StatusCode)
	}

	f.mu.Lock()
	f.latest["yarr"] = ReleaseCheck{Latest: "v2.0.0", Newer: true}
	f.mu.Unlock()
	if resp := h.post(c, portal+"/updates", portal, map[string][]string{"action": {"check"}}); resp.StatusCode >= 400 {
		t.Fatalf("check = %d", resp.StatusCode)
	}
	h.settled()
	if b := body(t, h.get(c, portal+"/")); !strings.Contains(b, `href="/updates"`) || !strings.Contains(b, "1 update available") {
		t.Errorf("the catalog doesn't link to the waiting update:\n%s", b)
	}
}

// Once a check has found a waiting update, an admin is told and someone who
// isn't an admin still isn't: the count is an admin's alone.
func TestWaitingUpdateIsNotShownToAPlainUser(t *testing.T) {
	h, _ := updateHarness(t)
	admin := h.client(true)
	h.login(admin, "root", "", "/")
	h.settled()
	if b := body(t, h.get(admin, portal+"/")); !strings.Contains(b, "1 update available") {
		t.Fatalf("the admin isn't told of the waiting update:\n%s", b)
	}

	plain := h.client(true)
	h.login(plain, "alice", "", "/")
	if b := body(t, h.get(plain, portal+"/")); strings.Contains(b, "/updates") || strings.Contains(b, "update available") {
		t.Errorf("a plain user is shown the waiting update:\n%s", b)
	}
}

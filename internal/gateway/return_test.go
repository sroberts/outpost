package gateway

import (
	"net/http"
	"strings"
	"testing"
)

// A catalog with nothing under way still has to catch up when someone comes
// back to it, and it has to do that without a timer: every load renews the
// session, so a page that refreshed itself would never let a forgotten tab
// sign out. poll.js, which does use one, stands down while the tab is hidden
// for the same reason.
func TestCatalogCatchesUpOnlyWhenItsTabReturns(t *testing.T) {
	h, _ := actionHarness(t)
	withNothingComingBack(h)
	c := h.client(true)
	b := body(t, h.login(c, "root", "", "/"))
	if !strings.Contains(b, returnURL) || !strings.Contains(returnURL, "?v=") {
		t.Fatalf("the catalog doesn't load %s:\n%s", returnURL, b)
	}
	resp := h.get(c, portal+returnURL)
	js := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("Content-Type"), "javascript") {
		t.Fatalf("GET %s = %d %q", returnURL, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(js, "visibilitychange") || !strings.Contains(js, "details[open]") {
		t.Errorf("the return script doesn't wait for the tab or hold for a menu:\n%s", js)
	}
	if strings.Contains(js, "setTimeout(") || strings.Contains(js, "setInterval(") {
		t.Errorf("the return script runs on a timer:\n%s", js)
	}
	if poll := body(t, h.get(c, portal+pollURL)); !strings.Contains(poll, "document.hidden") {
		t.Errorf("the poll script keeps reloading a hidden tab:\n%s", poll)
	}
	if strings.Contains(b, `http-equiv="refresh"`) {
		t.Errorf("an idle catalog reloads itself:\n%s", b)
	}
}

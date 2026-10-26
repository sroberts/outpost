package gateway

import "testing"

// A browser derives a form POST's Origin from the page's referrer policy:
// under no-referrer it sends Origin: null and no Referer, and the CSRF check
// then refuses the gateway's own login form. TestCSRF sets Origin by hand, so
// only the header itself shows this.
func TestPagesLetTheirOwnFormsCarryAnOrigin(t *testing.T) {
	h := newHarness(t)
	for _, link := range []string{portal + "/login", h.g.SetupURL()} {
		resp := h.get(h.client(false), link)
		resp.Body.Close()
		if got := resp.Header.Get("Referrer-Policy"); got != "same-origin" {
			t.Errorf("%s: Referrer-Policy = %q; want same-origin", link, got)
		}
	}
}

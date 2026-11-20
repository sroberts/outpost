package gateway

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"strings"
	"testing"
)

// Every page an admin sees ends in the same list of places, the one they're
// on marked rather than linked; the tally under the portal's name keeps only
// the counts and what's waiting.
func TestPlaces_SameListOnEveryAdminPage(t *testing.T) {
	h, _ := logsHarness(t)
	c := h.client(true)
	h.login(c, "root", "", "/")

	for _, path := range []string{"/", "/account", "/logs"} {
		page := body(t, h.get(c, portal+path))
		nav := between(page, `<nav class="places"`, `</nav>`)
		if nav == "" {
			t.Errorf("%s: no places list", path)
			continue
		}
		if !strings.Contains(nav, `href="/logs"`) {
			t.Errorf("%s: places don't link Logs:\n%s", path, nav)
		}
		current := strings.Contains(nav, `href="/logs" aria-current="page"`)
		if current != (path == "/logs") {
			t.Errorf("%s: Logs marked current = %v:\n%s", path, current, nav)
		}
		// Only what's there: this gateway has no secrets, AI or mail.
		for _, absent := range []string{`href="/secrets"`, `href="/ai"`, `href="/mail"`} {
			if strings.Contains(nav, absent) {
				t.Errorf("%s: places link a page that isn't there: %s", path, absent)
			}
		}
	}
	home := body(t, h.get(c, portal+"/"))
	if tally := between(home, `<p class="tally">`, `</p>`); strings.Contains(tally, "href=") && !strings.Contains(tally, "available") {
		t.Errorf("the tally carries links besides an update waiting: %s", tally)
	}
}

func TestPlaces_NoneForANonAdmin(t *testing.T) {
	h, _ := logsHarness(t)
	c := h.client(true)
	h.login(c, "alice", "", "/")
	for _, path := range []string{"/", "/account"} {
		if page := body(t, h.get(c, portal+path)); strings.Contains(page, `class="places"`) {
			t.Errorf("%s: a non-admin is shown the admin's places", path)
		}
	}
}

func between(s, from, to string) string {
	i := strings.Index(s, from)
	if i < 0 {
		return ""
	}
	j := strings.Index(s[i:], to)
	if j < 0 {
		return ""
	}
	return s[i : i+j]
}

// A file asked for by its current hash is the same bytes for good, so a
// phone keeps it; and text goes over the wire compressed when it may.
func TestAssets_CachedForGoodByHashAndGzipped(t *testing.T) {
	h := newHarness(t)
	c := h.client(false)
	want, err := assets.ReadFile("static/portal.css")
	if err != nil {
		t.Fatal(err)
	}

	resp := h.get(c, portal+cssURL, "Accept-Encoding", "gzip")
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("Cache-Control for the current hash = %q", cc)
	}
	if resp.Header.Get("Content-Encoding") != "gzip" || !strings.Contains(resp.Header.Get("Vary"), "Accept-Encoding") {
		t.Fatalf("not gzipped: Content-Encoding %q, Vary %q", resp.Header.Get("Content-Encoding"), resp.Header.Get("Vary"))
	}
	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the gzipped stylesheet isn't the stylesheet")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/css") {
		t.Errorf("Content-Type = %q", ct)
	}

	// No hash, or an old one: an hour, as before, and plain to a client
	// that didn't ask for gzip.
	for _, u := range []string{cssPath, cssPath + "?v=000000000000"} {
		resp := h.get(c, portal+u, "Accept-Encoding", "identity")
		if cc := resp.Header.Get("Cache-Control"); cc != "public, max-age=3600" {
			t.Errorf("%s: Cache-Control = %q", u, cc)
		}
		if resp.Header.Get("Content-Encoding") != "" {
			t.Errorf("%s: compressed for a client that didn't ask", u)
		}
		if b, _ := io.ReadAll(resp.Body); !bytes.Equal(b, want) {
			t.Errorf("%s: body isn't the stylesheet", u)
		}
	}

	// The image is compressed already and goes as it is.
	resp = h.get(c, portal+artURL, "Accept-Encoding", "gzip")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Encoding") != "" {
		t.Errorf("art: %d, Content-Encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
}

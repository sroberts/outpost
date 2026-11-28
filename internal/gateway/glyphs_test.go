package gateway

import (
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"
)

// The pages' CSP takes images from 'self' only, so every icon the stylesheet
// names has to be a file the gateway serves, and none may be a data: URL the
// browser would refuse without a word.
func TestGlyphs_EveryOneTheStylesheetNamesIsServed(t *testing.T) {
	css, err := assets.ReadFile("static/portal.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), `url("data:`) || strings.Contains(string(css), "url(data:") {
		t.Error("the stylesheet has a data: URL, which the pages' CSP refuses")
	}
	urls := regexp.MustCompile(`url\("(`+regexp.QuoteMeta(glyphsPrefix)+`[^"]+)"\)`).FindAllStringSubmatch(string(css), -1)
	if len(urls) == 0 {
		t.Fatal("the stylesheet names no glyphs")
	}

	h := newHarness(t)
	c := h.client(false)
	for _, m := range urls {
		resp := h.get(c, portal+m[1])
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(string(b), "<svg") {
			t.Errorf("%s: %d %q", m[1], resp.StatusCode, resp.Header.Get("Content-Type"))
		}
	}
	for _, bad := range []string{glyphsPrefix, glyphsPrefix + "nope.svg", glyphsPrefix + "..%2fportal.css", glyphsPrefix + "key.svg/x"} {
		if resp := h.get(c, portal+bad); resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s = %d; want 404", bad, resp.StatusCode)
		}
	}
}

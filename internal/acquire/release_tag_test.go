package acquire

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A tag names one release, prerelease or not, and has to reach GitHub intact
// however it is spelled.
func TestReleaseByTag(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.EscapedPath()
		if !strings.HasSuffix(asked, "/releases/tags/rel%2Fv2%20%231") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": "rel/v2 #1", "prerelease": true,
			"assets": []map[string]any{{"name": "svc", "browser_download_url": "x"}},
		})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}

	rel, err := c.ReleaseByTag(context.Background(), Repo{"o", "r"}, "rel/v2 #1")
	if err != nil {
		t.Fatalf("ReleaseByTag: %v (asked for %s)", err, asked)
	}
	if rel.Tag != "rel/v2 #1" || !rel.Prerelease {
		t.Errorf("release = %+v", rel)
	}

	_, err = c.ReleaseByTag(context.Background(), Repo{"o", "r"}, "v9")
	if err == nil || !strings.Contains(err.Error(), `no release tagged "v9"`) {
		t.Errorf("missing tag: err = %v", err)
	}
}

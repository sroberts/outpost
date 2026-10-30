package acquire

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLatestReleaseSince(t *testing.T) {
	const etag = `W/"abc"`
	assets := []map[string]any{{"name": "x"}}
	var sent []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent = append(sent, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", etag)
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v1", "assets": assets})
	}))
	defer srv.Close()
	c := &Client{HTTP: srv.Client(), BaseURL: srv.URL}
	repo := Repo{Owner: "o", Name: "r"}
	ctx := context.Background()

	rel, got, err := c.LatestReleaseSince(ctx, repo, "")
	if err != nil || rel.Tag != "v1" || got != etag {
		t.Fatalf("first ask = %+v, %q, %v", rel, got, err)
	}
	if rel, got, err = c.LatestReleaseSince(ctx, repo, got); !errors.Is(err, ErrNotModified) || rel != nil || got != etag {
		t.Errorf("second ask = %+v, %q, %v; want ErrNotModified and the same etag", rel, got, err)
	}
	if rel, got, err = c.LatestReleaseSince(ctx, repo, `"older"`); err != nil || rel.Tag != "v1" || got != etag {
		t.Errorf("with an old etag = %+v, %q, %v; want the release and the new etag", rel, got, err)
	}
	if len(sent) != 3 || sent[0] != "" || sent[1] != etag {
		t.Errorf("If-None-Match sent = %q", sent)
	}

	// A release with nothing attached is an answer too, so it keeps its etag.
	assets = nil
	var none *NoAssetsError
	if _, got, err = c.LatestReleaseSince(ctx, repo, ""); !errors.As(err, &none) || got != etag {
		t.Errorf("no assets = %q, %v; want a NoAssetsError and the etag", got, err)
	}
	// And LatestRelease itself never sends one.
	sent = nil
	if _, err := c.LatestRelease(ctx, repo); !errors.As(err, &none) || sent[0] != "" {
		t.Errorf("LatestRelease = %v, sent %q", err, sent)
	}
}

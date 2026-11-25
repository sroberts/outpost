package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestPush_Validate(t *testing.T) {
	for _, p := range []Push{
		{Listen: "0.0.0.0:47903"},
		{Listen: "127.0.0.1:0"},
		{Contact: "ops@example.com"},
		{Contact: "http://example.com"},
	} {
		if err := (Config{Push: p}).Validate(); err == nil {
			t.Errorf("%+v was taken", p)
		}
	}
	for _, p := range []Push{{}, {Contact: "mailto:ops@example.com"}, {Contact: "https://example.com", Listen: "127.0.0.1:5000"}} {
		if err := (Config{Push: p}).Validate(); err != nil {
			t.Errorf("%+v: %v", p, err)
		}
	}
}

func TestPush_ContactFallsBackToAnHTTPSPortal(t *testing.T) {
	if got := (Push{}).ContactFor("https://outpost.example.com/"); got != "https://outpost.example.com" {
		t.Errorf("got %q", got)
	}
	if got := (Push{}).ContactFor("http://outpost.local:8080"); got != "" {
		t.Errorf("an http portal gave %q; a push service would refuse it", got)
	}
	if got := (Push{Contact: "mailto:a@b.c"}).ContactFor("https://x.example"); got != "mailto:a@b.c" {
		t.Errorf("got %q", got)
	}
}

// A file that never had [push] is written as it was, and one that did keeps
// it through a save.
func TestPush_RenderRoundTrips(t *testing.T) {
	if strings.Contains(Defaults().Render(), "[push]") {
		t.Error("[push] written when unset")
	}
	path := filepath.Join(t.TempDir(), "config.toml")
	c := Defaults()
	c.Push = Push{Contact: "mailto:ops@example.com", Listen: "127.0.0.1:5000"}
	if err := Save(path, c); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Push != c.Push {
		t.Errorf("loaded %+v, saved %+v", got.Push, c.Push)
	}
	if got.Fingerprint() == Defaults().Fingerprint() {
		t.Error("setting [push] didn't change the fingerprint")
	}
}

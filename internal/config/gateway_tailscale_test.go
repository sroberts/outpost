package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestGateway_TailscaleName(t *testing.T) {
	on, off := true, false
	portal := Gateway{Enabled: true, PublicURL: "https://outpost.tail1234.ts.net", TailscaleName: "outpost"}
	cases := map[string]struct {
		cfg  Config
		want string // "" means valid
	}{
		"matching host":            {Config{Gateway: portal}, ""},
		"tailscale on explicitly":  {Config{Tailscale: Tailscale{Enabled: &on}, Gateway: portal}, ""},
		"tailscale off":            {Config{Tailscale: Tailscale{Enabled: &off}, Gateway: portal}, "needs both the gateway"},
		"host is another name":     {Config{Gateway: with(portal, func(g *Gateway) { g.PublicURL = "https://home.tail1234.ts.net" })}, "public_url's host must be outpost."},
		"host is the bare name":    {Config{Gateway: with(portal, func(g *Gateway) { g.PublicURL = "https://outpost" })}, "public_url's host must be outpost."},
		"name only a prefix":       {Config{Gateway: with(portal, func(g *Gateway) { g.PublicURL = "https://outpost2.tail1234.ts.net" })}, "public_url's host must be"},
		"not a label":              {Config{Gateway: with(portal, func(g *Gateway) { g.TailscaleName = "Out_post" })}, "one DNS label"},
		"dotted":                   {Config{Gateway: with(portal, func(g *Gateway) { g.TailscaleName = "outpost.tail1234" })}, "one DNS label"},
		"unset is fine without ts": {Config{Tailscale: Tailscale{Enabled: &off}, Gateway: with(portal, func(g *Gateway) { g.TailscaleName = "" })}, ""},
	}
	for name, c := range cases {
		err := c.cfg.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v; want one mentioning %q", name, err, c.want)
		}
	}
}

// With the gateway off nothing under [gateway] is read, but a tailnet name
// for a portal that isn't running would only mislead, so it is refused.
func TestGateway_TailscaleNameNeedsTheGateway(t *testing.T) {
	err := Config{Gateway: Gateway{PublicURL: "https://outpost.tail1234.ts.net", TailscaleName: "outpost"}}.Validate()
	if err == nil || !strings.Contains(err.Error(), "[gateway] enabled = true") {
		t.Errorf("error %v; want one saying the gateway has to be on", err)
	}
}

func TestGateway_TailscaleNameSurvivesSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := Gateway{Enabled: true, PublicURL: "https://outpost.tail1234.ts.net", TailscaleName: "outpost"}
	if err := Save(path, Config{Gateway: want}); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Gateway.TailscaleName != "outpost" {
		t.Errorf("tailscale_name = %q after a save", back.Gateway.TailscaleName)
	}
	if !strings.Contains(Defaults().Render(), "# tailscale_name = \"outpost\"") {
		t.Error("the rendered file doesn't show tailscale_name as an example")
	}
}

func with(g Gateway, change func(*Gateway)) Gateway {
	change(&g)
	return g
}

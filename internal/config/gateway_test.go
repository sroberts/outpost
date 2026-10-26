package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGateway_OffIsNotValidated(t *testing.T) {
	cfg := Config{Gateway: Gateway{PublicURL: "not a url", TLSCert: "only-half"}}
	if err := cfg.Validate(); err != nil {
		t.Errorf("a disabled gateway was validated: %v", err)
	}
}

func TestGateway_Validate(t *testing.T) {
	ok := Gateway{Enabled: true, PublicURL: "https://outpost.example"}
	cases := map[string]struct {
		mutate func(*Gateway)
		want   string // "" means valid
	}{
		"minimal":             {func(*Gateway) {}, ""},
		"trailing slash":      {func(g *Gateway) { g.PublicURL += "/" }, ""},
		"port":                {func(g *Gateway) { g.PublicURL = "https://outpost.example:8443" }, ""},
		"http on loopback":    {func(g *Gateway) { g.PublicURL = "http://outpost.example" }, ""},
		"bare address proxy":  {func(g *Gateway) { g.TrustedProxies = []string{"192.0.2.1"} }, ""},
		"no public_url":       {func(g *Gateway) { g.PublicURL = "" }, "public_url is required"},
		"ftp":                 {func(g *Gateway) { g.PublicURL = "ftp://outpost.example" }, "scheme"},
		"path":                {func(g *Gateway) { g.PublicURL = "https://outpost.example/portal" }, "just a scheme and host"},
		"query":               {func(g *Gateway) { g.PublicURL = "https://outpost.example/?x=1" }, "just a scheme and host"},
		"ip":                  {func(g *Gateway) { g.PublicURL = "https://192.0.2.1" }, "IP address"},
		"uppercase domain":    {func(g *Gateway) { g.Domain = "Outpost.Example" }, "domain"},
		"half tls":            {func(g *Gateway) { g.TLSCert = "cert.pem" }, "tls_cert and tls_key"},
		"bad listen":          {func(g *Gateway) { g.Listen = "8443" }, "listen"},
		"bad proxy":           {func(g *Gateway) { g.TrustedProxies = []string{"nope"} }, "trusted_proxies"},
		"http on the network": {func(g *Gateway) { g.PublicURL = "http://outpost.example"; g.Listen = "0.0.0.0:80" }, "allow_insecure"},
		"http, insecure allowed": {func(g *Gateway) {
			g.PublicURL = "http://outpost.example"
			g.Listen = "0.0.0.0:80"
			g.AllowInsecure = true
		}, ""},
	}
	for name, c := range cases {
		g := ok
		c.mutate(&g)
		err := Config{Gateway: g}.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: error %v; want one mentioning %q", name, err, c.want)
		}
	}
}

func TestGateway_Getters(t *testing.T) {
	g := Gateway{PublicURL: "https://Outpost.Example:8443"}
	if g.ListenAddr() != DefaultGatewayListen {
		t.Errorf("ListenAddr = %q", g.ListenAddr())
	}
	if g.AppDomain() != "outpost.example" {
		t.Errorf("AppDomain = %q", g.AppDomain())
	}
	if n := len(g.Proxies()); n != 2 {
		t.Errorf("default proxies: %d; want loopback v4 and v6", n)
	}
	g.TrustedProxies = []string{}
	if n := len(g.Proxies()); n != 0 {
		t.Errorf("an empty list trusts %d ranges", n)
	}
}

// Save rewrites the whole file from Render, so a gateway section must
// survive anything else being changed, such as `config --tailscale`.
func TestGateway_SurvivesSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := Gateway{
		Enabled:        true,
		Listen:         "0.0.0.0:443",
		PublicURL:      "https://outpost.example",
		Domain:         "apps.example",
		TLSCert:        "/etc/cert.pem",
		TLSKey:         "/etc/key.pem",
		TrustedProxies: []string{"10.0.0.0/8"},
		AllowInsecure:  true,
	}
	if err := Save(path, Config{Gateway: want}); err != nil {
		t.Fatal(err)
	}
	back, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back.Gateway, want) {
		t.Errorf("round trip:\n got %+v\nwant %+v", back.Gateway, want)
	}

	// An untouched section stays unset, so the defaults keep applying.
	if err := Save(path, Defaults()); err != nil {
		t.Fatal(err)
	}
	back, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if back.Gateway.TrustedProxies != nil || back.Gateway.Listen != "" || back.Gateway.Enabled {
		t.Errorf("defaults rendered as settings: %+v", back.Gateway)
	}
}

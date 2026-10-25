package gateway

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/config"
)

func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                                  "/",
		"/":                                 "/",
		"/feeds?unread=1":                   "/feeds?unread=1",
		"/a/b#frag":                         "/a/b#frag",
		"//evil.example":                    "/",
		"///evil.example":                   "/",
		"/\\evil.example":                   "/",
		"/a\\b":                             "/",
		"/\t/evil.example":                  "/",
		"/\n/evil.example":                  "/",
		"https://evil.example/":             "/",
		"evil.example":                      "/",
		"javascript:alert(1)":               "/",
		"/" + string(make([]byte, maxNext)): "/",
	}
	for in, want := range cases {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestCodes(t *testing.T) {
	var c codeStore
	now := time.Now()
	code, err := c.mint(pendingCode{user: "alice", app: "yarr", parent: "p"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.redeem(code, "yarr", now.Add(codeTTL)); ok {
		t.Error("an expired code redeemed")
	}

	code, _ = c.mint(pendingCode{user: "alice", app: "yarr", parent: "p"}, now)
	p, ok := c.redeem(code, "yarr", now.Add(time.Second))
	if !ok || p.user != "alice" || p.parent != "p" {
		t.Fatalf("redeem = %+v, %v", p, ok)
	}
	if _, ok := c.redeem(code, "yarr", now.Add(time.Second)); ok {
		t.Error("a code redeemed twice")
	}
	if _, ok := c.redeem("never-minted", "yarr", now); ok {
		t.Error("an unknown code redeemed")
	}
}

func TestThrottle(t *testing.T) {
	var th throttle
	now := time.Now()
	if d := th.wait(now, "a"); d != 0 {
		t.Errorf("fresh key waits %v", d)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60}
	for i, w := range want {
		th.fail(now, "a")
		if d := th.wait(now, "a", "unrelated"); d != w*time.Second {
			t.Errorf("after %d failures: wait %v; want %v", i+1, d, w*time.Second)
		}
	}
	th.reset("a")
	if d := th.wait(now, "a"); d != 0 {
		t.Errorf("after reset: wait %v", d)
	}

	lenient := throttle{free: 2}
	lenient.fail(now, "b")
	lenient.fail(now, "b")
	if d := lenient.wait(now, "b"); d != 0 {
		t.Errorf("within the free allowance: wait %v", d)
	}
	lenient.fail(now, "b")
	if d := lenient.wait(now, "b"); d != time.Second {
		t.Errorf("first failure past the allowance: wait %v; want 1s", d)
	}
}

func TestClientIP(t *testing.T) {
	g := &Gateway{proxies: []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}}
	cases := []struct {
		remote, xff, want string
	}{
		{"192.0.2.1:5000", "", "192.0.2.1"},
		{"192.0.2.1:5000", "203.0.113.9", "192.0.2.1"},                // not a proxy: XFF ignored
		{"10.0.0.2:5000", "203.0.113.9", "203.0.113.9"},               // proxy: its client
		{"10.0.0.2:5000", "198.51.100.1, 203.0.113.9", "203.0.113.9"}, // nearest untrusted hop
		{"10.0.0.2:5000", "203.0.113.9, 10.0.0.7", "203.0.113.9"},     // skipping trusted hops
		{"10.0.0.2:5000", "", "10.0.0.2"},
		{"10.0.0.2:5000", "garbage", "10.0.0.2"},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = c.remote
		if c.xff != "" {
			r.Header.Set("X-Forwarded-For", c.xff)
		}
		if got := g.clientIP(r); got != c.want {
			t.Errorf("remote %s XFF %q: clientIP = %s; want %s", c.remote, c.xff, got, c.want)
		}
	}
}

func TestRemoveCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", "a=1; outpost=secret; b=2")
	h.Add("Cookie", "outpost=again")
	removeCookie(h, "outpost")
	if got := h.Values("Cookie"); len(got) != 1 || got[0] != "a=1; b=2" {
		t.Errorf("Cookie = %q", got)
	}
	only := http.Header{"Cookie": {"outpost=x"}}
	removeCookie(only, "outpost")
	if _, ok := only["Cookie"]; ok {
		t.Error("an empty Cookie header was left behind")
	}
}

func TestServiceEnv(t *testing.T) {
	if env := ServiceEnv(config.Gateway{}); env != nil {
		t.Errorf("gateway off: env = %v", env)
	}
	cases := []struct {
		cfg  config.Gateway
		jwks string
	}{
		{config.Gateway{Enabled: true, PublicURL: "https://outpost.example/"}, "http://127.0.0.1:8443" + jwksPath},
		{config.Gateway{Enabled: true, PublicURL: "https://outpost.example", Listen: "0.0.0.0:9000"}, "http://127.0.0.1:9000" + jwksPath},
		{config.Gateway{Enabled: true, PublicURL: "https://outpost.example", Listen: "[::1]:9000"}, "http://[::1]:9000" + jwksPath},
		{config.Gateway{Enabled: true, PublicURL: "https://outpost.example", Listen: ":443", TLSCert: "c", TLSKey: "k"},
			"https://outpost.example" + jwksPath},
	}
	for _, c := range cases {
		env := ServiceEnv(c.cfg)
		want := []string{"OUTPOST_AUTH=gateway", "OUTPOST_ISSUER=https://outpost.example", "OUTPOST_JWKS_URL=" + c.jwks}
		if len(env) != len(want) {
			t.Fatalf("env = %v", env)
		}
		for i := range want {
			if env[i] != want[i] {
				t.Errorf("listen %q: env[%d] = %q; want %q", c.cfg.Listen, i, env[i], want[i])
			}
		}
	}
}

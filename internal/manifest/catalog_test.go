package manifest

import "testing"

func TestValidate_CatalogFields(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*Service)
		error string // "" means valid
	}{
		{"all set", func(s *Service) {
			s.Title, s.Description = "Yarr", "RSS reader"
			s.Subdomain = "rss"
			s.Allow = []string{"alice", "group:family"}
			s.PublicPaths = []string{"/webhook/*", "/feed.xml"}
		}, ""},
		{"hostname", func(s *Service) { s.Hostname = "yarr.tail1234.ts.net" }, ""},
		{"long title", func(s *Service) { s.Title = string(make([]byte, 61)) }, "title is 61 characters"},
		{"two-line description", func(s *Service) { s.Description = "one\ntwo" }, "one line"},
		{"subdomain with a dot", func(s *Service) { s.Subdomain = "a.b" }, "one DNS label"},
		{"uppercase subdomain", func(s *Service) { s.Subdomain = "RSS" }, "one DNS label"},
		{"subdomain and hostname", func(s *Service) { s.Subdomain, s.Hostname = "rss", "rss.example.com" }, "not both"},
		{"bare hostname", func(s *Service) { s.Hostname = "yarr" }, "full lowercase name"},
		{"IP hostname", func(s *Service) { s.Hostname = "10.0.0.1" }, "IP address"},
		{"bad allow", func(s *Service) { s.Allow = []string{"Alice"} }, "isn't a username"},
		{"bad group", func(s *Service) { s.Allow = []string{"group:"} }, "isn't a group name"},
	}
	for _, tc := range cases {
		svc := processSvc()
		tc.set(svc)
		errs := ValidateAll([]*Service{svc})
		switch {
		case tc.error == "" && len(errs) > 0:
			t.Errorf("%s: rejected: %v", tc.name, errs)
		case tc.error != "" && !errorsContaining(errs, tc.error):
			t.Errorf("%s: want an error containing %q, got %v", tc.name, tc.error, errs)
		}
	}
}

func TestValidPublicPath(t *testing.T) {
	good := []string{"/webhook/*", "/feed.xml", "/api/v1/hook", "/a/"}
	bad := []string{"webhook", "/*", "/web*", "/a/*/b", "/a/../b/*", "//x", "/.outpost/x", "/a?b", "/a%2Fb", `/a\b`}
	for _, p := range good {
		if err := ValidPublicPath(p); err != nil {
			t.Errorf("%q rejected: %v", p, err)
		}
	}
	for _, p := range bad {
		if ValidPublicPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestMatchPublicPath(t *testing.T) {
	pats := []string{"/webhook/*", "/feed.xml"}
	for p, want := range map[string]bool{
		"/webhook/github": true,
		"/webhook/":       true,
		"/webhook":        false,
		"/feed.xml":       true,
		"/feed.xml/x":     false,
		"/admin":          false,
	} {
		if got := MatchPublicPath(pats, p); got != want {
			t.Errorf("%s: %v, want %v", p, got, want)
		}
	}
}

// Two services at one gateway address would leave one unreachable.
func TestValidate_GatewayAddressesAreUnique(t *testing.T) {
	a, b := processSvc(), processSvc()
	b.Name, b.Port, b.TailscaleName = "other", 8081, "other"
	b.Subdomain = "svc"
	if errs := ValidateAll([]*Service{a, b}); !errorsContaining(errs, `gateway address "svc" is already svc's`) {
		t.Errorf("a subdomain equal to another service's name was accepted: %v", errs)
	}
}

// With the gateway on, a name that can't be a DNS label has no address,
// unless subdomain or hostname gives it one.
func TestValidate_GatewayNeedsADNSName(t *testing.T) {
	svc := processSvc()
	svc.Name = "My_App"
	opts := Options{RequireDNSName: true}
	if errs := ValidateWith([]*Service{svc}, opts); !errorsContaining(errs, "can't be a DNS label") {
		t.Errorf("accepted: %v", errs)
	}
	svc.Subdomain = "my-app"
	if errs := ValidateWith([]*Service{svc}, opts); len(errs) > 0 {
		t.Errorf("a subdomain didn't settle it: %v", errs)
	}
	svc.Subdomain = ""
	if errs := ValidateWith([]*Service{svc}, Options{}); len(errs) > 0 {
		t.Errorf("rejected with the gateway off: %v", errs)
	}
}

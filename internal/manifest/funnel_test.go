package manifest

import (
	"strings"
	"testing"
)

func funnelService() *Service {
	return &Service{
		Name: "tasking", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/tasking",
		TailscaleName: "tasking", Healthcheck: "/up", PublicPaths: []string{"/webhooks/github"},
	}
}

var withGateway = Options{RequireTailscale: true, Gateway: true, RequireDNSName: true}

// Funnel on and off are both valid configurations of the same service.
func TestFunnel_OnAndOffAreBothValid(t *testing.T) {
	for _, on := range []bool{false, true} {
		s := funnelService()
		s.Funnel = on
		if errs := ValidateWith([]*Service{s}, withGateway); len(errs) != 0 {
			t.Errorf("funnel = %v: %v", on, errs)
		}
	}
	// Off needs none of what on does.
	s := funnelService()
	s.PublicPaths = nil
	if errs := ValidateWith([]*Service{s}, Options{RequireTailscale: true}); len(errs) != 0 {
		t.Errorf("funnel off, no gateway, no public paths: %v", errs)
	}
}

func TestFunnel_RefusesWhatWouldExposeMoreOrNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		edit func(*Service, *Options)
		want string
	}{
		"no gateway":        {func(_ *Service, o *Options) { o.Gateway = false }, "needs the gateway"},
		"no public paths":   {func(s *Service, _ *Options) { s.PublicPaths = nil }, "there are none"},
		"no tailscale_name": {func(s *Service, _ *Options) { s.TailscaleName = "" }, "needs Tailscale and a tailscale_name"},
		"no tailscale":      {func(_ *Service, o *Options) { o.RequireTailscale = false }, "needs Tailscale"},
	} {
		s, opts := funnelService(), withGateway
		s.Funnel = true
		tc.edit(s, &opts)
		errs := ValidateWith([]*Service{s}, opts)
		found := false
		for _, err := range errs {
			found = found || strings.Contains(err.Error(), tc.want)
		}
		if !found {
			t.Errorf("%s: errors %v; want one saying %q", name, errs, tc.want)
		}
	}
}

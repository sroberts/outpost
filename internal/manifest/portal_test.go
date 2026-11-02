package manifest

import (
	"strings"
	"testing"
)

func portalService(name, tsName string, port int) *Service {
	return &Service{Name: name, Runtime: "process", Binary: "/bin/true", Port: port, Storage: "data/" + name,
		Healthcheck: "/up", TailscaleName: tsName}
}

// The portal's node asks Tailscale for its name too, and Tailscale renames
// whichever comes second, moving one of them off the address it is known by.
func TestValidate_TailscaleNameIsNotThePortals(t *testing.T) {
	services := []*Service{portalService("sous", "outpost", 9001), portalService("yarr", "yarr", 9002)}
	errs := ValidateWith(services, Options{RequireTailscale: true, PortalTailscaleName: "outpost"})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), `tailscale_name "outpost" is the portal's`) {
		t.Fatalf("errs = %v; want one naming the portal's tailscale_name", errs)
	}
	if !strings.HasPrefix(errs[0].Error(), "sous:") {
		t.Errorf("error %q doesn't name the service", errs[0])
	}

	if errs := ValidateWith(services, Options{RequireTailscale: true}); len(errs) != 0 {
		t.Errorf("without a portal node: %v", errs)
	}
}

// The portal's node keeps its state in a directory named for it, beside the
// services' own.
func TestValidate_PortalNodeNameIsReserved(t *testing.T) {
	errs := ValidateWith([]*Service{portalService(PortalNodeName, "p", 9001)}, Options{})
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "reserved") {
		t.Fatalf("errs = %v; want one saying the name is reserved", errs)
	}
}

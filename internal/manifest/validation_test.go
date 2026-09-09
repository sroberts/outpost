package manifest

import (
	"strings"
	"testing"
)

func processSvc() *Service {
	return &Service{
		Name: "svc", Runtime: "process", Binary: "/usr/bin/true",
		Port: 8080, Storage: "/tmp/svc", TailscaleName: "svc",
		Healthcheck: "/up", RestartPolicy: "on-failure",
	}
}

func errorsContaining(errs []error, substr string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), substr) {
			return true
		}
	}
	return false
}

// §5: exactly one of (binary+args) or image, matching runtime.
func TestValidate_RejectsBinaryAndImageTogether(t *testing.T) {
	svc := processSvc()
	svc.Image = "nginx:latest"

	errs := ValidateAll([]*Service{svc})
	if !errorsContaining(errs, "image is set but runtime") {
		t.Errorf("a process service with an image was accepted; got %v", errs)
	}
}

func TestValidate_RejectsContainerArgsOnAProcess(t *testing.T) {
	svc := processSvc()
	svc.ContainerArgs = []string{"--cap-add", "NET_ADMIN"}

	if errs := ValidateAll([]*Service{svc}); !errorsContaining(errs, "container_args is set") {
		t.Errorf("container_args on a process service was accepted; got %v", errs)
	}
}

func TestValidate_RejectsBinaryOnAContainer(t *testing.T) {
	svc := &Service{
		Name: "svc", Runtime: "container", Image: "nginx:latest", Binary: "/usr/bin/true",
		Port: 8080, Storage: "/tmp/svc", TailscaleName: "svc", Healthcheck: "/up",
	}
	if errs := ValidateAll([]*Service{svc}); !errorsContaining(errs, "binary is set but runtime") {
		t.Errorf("a container service with a binary was accepted; got %v", errs)
	}
}

// The port is declared twice; disagreement makes Outpost probe the wrong port
// and restart a perfectly healthy service forever.
func TestValidate_CatchesPortDisagreeingWithArgs(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--port", "9999"},
		{"serve", "-port", "9999"},
		{"serve", "--port=9999"},
	} {
		svc := processSvc()
		svc.Port = 8080
		svc.Args = args

		errs := ValidateAll([]*Service{svc})
		if !errorsContaining(errs, "would probe a port") {
			t.Errorf("args %v with port 8080 was accepted; got %v", args, errs)
		}
	}
}

func TestValidate_AllowsPortAgreeingWithArgs(t *testing.T) {
	for _, args := range [][]string{
		{"serve", "--port", "8080"},
		{"serve", "--port=8080"},
		{"serve", "--listen", "9999"}, // an unrecognised spelling is not checked
		{"serve"},
	} {
		svc := processSvc()
		svc.Args = args
		if errs := ValidateAll([]*Service{svc}); len(errs) != 0 {
			t.Errorf("args %v were rejected: %v", args, errs)
		}
	}
}

// Not everyone uses Tailscale.
func TestValidate_TailscaleNameOptionalWhenDisabled(t *testing.T) {
	svc := processSvc()
	svc.TailscaleName = ""

	if errs := ValidateAll([]*Service{svc}); !errorsContaining(errs, "tailscale_name is required") {
		t.Errorf("tailscale_name should be required by default; got %v", errs)
	}
	if errs := ValidateWith([]*Service{svc}, Options{RequireTailscale: false}); len(errs) != 0 {
		t.Errorf("tailscale_name should be optional when Tailscale is off; got %v", errs)
	}
}

// The default error should say how to opt out.
func TestValidate_TailscaleErrorPointsAtTheConfig(t *testing.T) {
	svc := processSvc()
	svc.TailscaleName = ""
	if errs := ValidateAll([]*Service{svc}); !errorsContaining(errs, "config.toml") {
		t.Errorf("error does not mention how to disable Tailscale: %v", errs)
	}
}

// Two services with no tailscale_name are not a name collision.
func TestValidate_EmptyTailscaleNamesDoNotCollide(t *testing.T) {
	a, b := processSvc(), processSvc()
	a.Name, a.TailscaleName, a.Port = "a", "", 8080
	b.Name, b.TailscaleName, b.Port = "b", "", 8081

	if errs := ValidateWith([]*Service{a, b}, Options{RequireTailscale: false}); len(errs) != 0 {
		t.Errorf("two unnamed services were treated as colliding: %v", errs)
	}
}

// Duplicate names must still collide when they are set.
func TestValidate_DuplicateTailscaleNamesStillCollide(t *testing.T) {
	a, b := processSvc(), processSvc()
	a.Name, a.Port, a.TailscaleName = "a", 8080, "shared"
	b.Name, b.Port, b.TailscaleName = "b", 8081, "shared"

	if errs := ValidateWith([]*Service{a, b}, Options{RequireTailscale: false}); !errorsContaining(errs, "duplicate tailscale_name") {
		t.Errorf("duplicate names were accepted: %v", errs)
	}
}

func TestPortInArgs(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want int
		ok   bool
	}{
		{[]string{"--port", "8080"}, 8080, true},
		{[]string{"-port", "8080"}, 8080, true},
		{[]string{"--port=8080"}, 8080, true},
		{[]string{"serve", "-v", "--port", "80"}, 80, true},
		{[]string{"--port"}, 0, false},           // no value
		{[]string{"--port", "abc"}, 0, false},    // not a number
		{[]string{"--listen", "8080"}, 0, false}, // different flag
		{nil, 0, false},
	} {
		got, ok := portInArgs(tc.args)
		if got != tc.want || ok != tc.ok {
			t.Errorf("portInArgs(%v) = (%d, %v); want (%d, %v)", tc.args, got, ok, tc.want, tc.ok)
		}
	}
}

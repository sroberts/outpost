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

// Outpost supervises native processes only; any other runtime is an error.
func TestValidate_RejectsNonProcessRuntime(t *testing.T) {
	svc := processSvc()
	svc.Runtime = "container"

	errs := ValidateAll([]*Service{svc})
	if !errorsContaining(errs, "native processes only") {
		t.Errorf("runtime = container was accepted; got %v", errs)
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

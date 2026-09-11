package manifest

import (
	"strings"
	"testing"
)

func portSvc(env map[string]string, port int) *Service {
	return &Service{
		Name: "alpha", Runtime: "process", Binary: "/usr/bin/true",
		Port: port, Storage: "/tmp/alpha", TailscaleName: "alpha",
		Healthcheck: "/up", RestartPolicy: "on-failure", Env: env,
	}
}

func portErrors(t *testing.T, svc *Service) bool {
	t.Helper()
	for _, err := range ValidateAll([]*Service{svc}) {
		if strings.Contains(err.Error(), "OUTPOST_PORT") {
			return true
		}
	}
	return false
}

// `outpost add` writes OUTPOST_PORT into every manifest it generates, so
// editing `port` by hand afterwards and forgetting the environment is easy —
// and it produces exactly the restart loop the args check exists to prevent:
// the service binds the stale port while the supervisor probes the new one.
func TestValidate_RejectsOutpostPortDisagreeingWithPort(t *testing.T) {
	if !portErrors(t, portSvc(map[string]string{"OUTPOST_PORT": "9999"}, 8080)) {
		t.Error("a manifest whose OUTPOST_PORT disagrees with port was accepted")
	}
}

func TestValidate_AcceptsOutpostPortThatAgrees(t *testing.T) {
	if portErrors(t, portSvc(map[string]string{"OUTPOST_PORT": "8080"}, 8080)) {
		t.Error("a matching OUTPOST_PORT was rejected")
	}
}

// A value that is not a number is the service's business, not a disagreement —
// the same restraint portInArgs shows.
func TestValidate_IgnoresAnUnreadableOutpostPort(t *testing.T) {
	for _, v := range []string{"fixed", "", "8080a", "-1", "0"} {
		if portErrors(t, portSvc(map[string]string{"OUTPOST_PORT": v}, 8080)) {
			t.Errorf("OUTPOST_PORT = %q was treated as a disagreement", v)
		}
	}
}

func TestValidate_IgnoresAMissingOutpostPort(t *testing.T) {
	if portErrors(t, portSvc(map[string]string{"LOG_LEVEL": "debug"}, 8080)) {
		t.Error("a manifest without OUTPOST_PORT was rejected")
	}
	if portErrors(t, portSvc(nil, 8080)) {
		t.Error("a manifest with no env at all was rejected")
	}
}

func TestPortInEnv(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		port int
		ok   bool
	}{
		{map[string]string{"OUTPOST_PORT": "8080"}, 8080, true},
		{map[string]string{"OUTPOST_PORT": " 8080 "}, 8080, true},
		{map[string]string{"OUTPOST_PORT": "nope"}, 0, false},
		{map[string]string{}, 0, false},
		{nil, 0, false},
	} {
		got, ok := portInEnv(tc.env)
		if got != tc.port || ok != tc.ok {
			t.Errorf("portInEnv(%v) = %d,%v; want %d,%v", tc.env, got, ok, tc.port, tc.ok)
		}
	}
}

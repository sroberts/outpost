package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tcpService(name string, port int, tcp map[string]int) *Service {
	s := &Service{Name: name, Binary: "/bin/true", Port: port, Storage: "/tmp/" + name, TailscaleName: name, TCP: tcp}
	s.setDefault()
	return s
}

func errorsSay(errs []error, want string) bool {
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return true
		}
	}
	return false
}

func TestTCP_ValidForwards(t *testing.T) {
	s := tcpService("soft", 23232, map[string]int{"22": 23231, "9418": 9418})
	if errs := ValidateWith([]*Service{s}, DefaultOptions()); len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	got := s.TCPForwards()
	if len(got) != 2 || got[0] != (TCPForward{22, 23231}) || got[1] != (TCPForward{9418, 9418}) {
		t.Errorf("TCPForwards = %v; want 22 then 9418", got)
	}
}

func TestTCP_Rejections(t *testing.T) {
	cases := []struct {
		tcp  map[string]int
		want string
	}{
		{map[string]int{"ssh": 23231}, "not a port"},
		{map[string]int{"70000": 23231}, "not a port"},
		{map[string]int{"22": 0}, "not a port"},
		{map[string]int{"443": 23231}, "node's own HTTP(S) port"},
		{map[string]int{"80": 23231}, "node's own HTTP(S) port"},
		// Raw TCP to the HTTP port would skip the gateway's login.
		{map[string]int{"8080": 23232}, "bypass the gateway"},
	}
	for _, c := range cases {
		errs := ValidateWith([]*Service{tcpService("soft", 23232, c.tcp)}, DefaultOptions())
		if !errorsSay(errs, c.want) {
			t.Errorf("tcp = %v: errors %v; want one saying %q", c.tcp, errs, c.want)
		}
	}
}

func TestTCP_NeedsTheTailnet(t *testing.T) {
	s := tcpService("soft", 23232, map[string]int{"22": 23231})
	errs := ValidateWith([]*Service{s}, Options{RequireTailscale: false})
	if !errorsSay(errs, "need Tailscale") {
		t.Errorf("without Tailscale: %v; want the forwards refused", errs)
	}
}

func TestTCP_ClashesWithOtherServices(t *testing.T) {
	soft := tcpService("soft", 23232, map[string]int{"22": 8080})
	web := tcpService("web", 8080, nil)
	if errs := ValidateWith([]*Service{soft, web}, DefaultOptions()); !errorsSay(errs, "web's HTTP port") {
		t.Errorf("forward to another service's port: %v", errs)
	}
	a := tcpService("a", 1000, map[string]int{"22": 2222})
	b := tcpService("b", 1001, map[string]int{"2200": 2222})
	if errs := ValidateWith([]*Service{a, b}, DefaultOptions()); !errorsSay(errs, "also one of") {
		t.Errorf("two services forwarding to one port: %v", errs)
	}
}

func TestTCP_ParseForward(t *testing.T) {
	f, err := ParseTCPForward("22=23231")
	if err != nil || f != (TCPForward{22, 23231}) || f.String() != "22=23231" {
		t.Errorf("ParseTCPForward = %v, %v", f, err)
	}
	for _, bad := range []string{"22", "22=", "=22", "a=b", "22=99999"} {
		if _, err := ParseTCPForward(bad); err == nil {
			t.Errorf("ParseTCPForward(%q) accepted", bad)
		}
	}
}

func TestTCP_FromTOML(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "services")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "soft.toml")
	if err := os.WriteFile(path, []byte(`
binary = "/opt/soft"
port = 23232
storage = "/tmp/soft"
tailscale_name = "soft"
tcp = { 22 = 23231 }
`), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.TCPForwards(); len(got) != 1 || got[0] != (TCPForward{22, 23231}) {
		t.Errorf("TCPForwards = %v", got)
	}
}

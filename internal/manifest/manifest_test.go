package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

// TestManifest_Load reads a well-formed TOML file from testdata.
func TestManifest_Load(t *testing.T) {
	svc, err := Load("testdata/valid.toml")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if svc.Name != "testsvc" {
		t.Errorf("Name = %q; want %q", svc.Name, "testsvc")
	}
	if svc.Runtime != "process" {
		t.Errorf("Runtime = %q; want %q", svc.Runtime, "process")
	}
	if svc.Port != 8080 {
		t.Errorf("Port = %d; want 8080", svc.Port)
	}
	if svc.Healthcheck != "/health" {
		t.Errorf("Healthcheck = %q; want %q", svc.Healthcheck, "/health")
	}
}

func TestManifest_Defaults(t *testing.T) {
	svc, err := Load("testdata/defaults.toml")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if svc.Runtime != "process" {
		t.Errorf("default Runtime = %q; want \"process\"", svc.Runtime)
	}
	if svc.Healthcheck != "/up" {
		t.Errorf("default Healthcheck = %q; want \"/up\"", svc.Healthcheck)
	}
	if svc.RestartPolicy != "on-failure" {
		t.Errorf("default RestartPolicy = %q; want \"on-failure\"", svc.RestartPolicy)
	}
	if svc.BackoffSeconds != 5 {
		t.Errorf("default BackoffSeconds = %d; want 5", svc.BackoffSeconds)
	}
}

func TestValidateAll_Errors(t *testing.T) {
	tests := []struct {
		name   string
		setUp  func(t *testing.T) []*Service
		wantN  int
	}{
		{
			name: "duplicate names",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
					{Name: "svc", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
					{Name: "svc", Runtime: "process", Binary: "/usr/bin/true", Port: 8081, Storage: "/tmp/b", TailscaleName: "b"},
				}
			},
			wantN: 1,
		},
		{
			name: "port collision on process services",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
					{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
					{Name: "b", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/b", TailscaleName: "b"},
				}
			},
			wantN: 1,
		},
		{
			name: "duplicate tailscale_name",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
					{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "ts"},
					{Name: "b", Runtime: "process", Binary: "/usr/bin/true", Port: 8081, Storage: "/tmp/b", TailscaleName: "ts"},
				}
			},
			wantN: 1,
		},
		{
			name: "missing name",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			// Funnel itself is valid; this one has neither the gateway nor a
			// public path for it to answer. funnel_test.go has the rest.
			name: "funnel without the gateway or a public path",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a", Funnel: true},
						}
			},
			wantN: 2,
		},
		{
			name: "invalid RestartPolicy",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a", RestartPolicy: "always-invalid"},
						}
			},
			wantN: 1,
		},
		{
			name: "missing port",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 0, Storage: "/tmp/a", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			name: "missing storage",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			name: "missing tailscale_name",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: ""},
						}
			},
			wantN: 1,
		},
		{
			name: "process without binary",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			name: "container without image",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "container", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			name: "invalid runtime",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "kubernetes", Port: 8080, Storage: "/tmp/a", TailscaleName: "a"},
						}
			},
			wantN: 1,
		},
		{
			name: "healthcheck not starting with /",
			setUp: func(t *testing.T) []*Service {
				return []*Service{
						{Name: "a", Runtime: "process", Binary: "/usr/bin/true", Port: 8080, Storage: "/tmp/a", TailscaleName: "a", Healthcheck: "up"},
						}
			},
			wantN: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			services := tt.setUp(t)
			errs := ValidateAll(services)
			if len(errs) < tt.wantN {
				t.Errorf("got %d errors; want >= %d", len(errs), tt.wantN)
				for _, e := range errs {
					t.Logf("  %v", e)
				}
			}
		})
	}
}

func TestExpandStorage(t *testing.T) {
	svc := &Service{Storage: "~/data"}
	result := svc.ExpandStorage()
	home, _ := os.UserHomeDir()
	want := filepath.Join(home, "data")
	if result != want {
		t.Errorf("ExpandStorage = %q; want %q", result, want)
	}
}

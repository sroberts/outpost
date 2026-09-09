package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Service represents a single service declaration in the manifest.
type Service struct {
	Name           string            `toml:"name"`
	Runtime        string            `toml:"runtime"`
	Binary         string            `toml:"binary"`
	Args           []string          `toml:"args"`
	Env            map[string]string `toml:"env"`
	Image          string            `toml:"image"`
	ContainerArgs  []string          `toml:"container_args"`
	Port           int               `toml:"port"`
	Storage        string            `toml:"storage"`
	TailscaleName  string            `toml:"tailscale_name"`
	Funnel         bool              `toml:"funnel"`
	Healthcheck    string            `toml:"healthcheck"`
	MCP            bool              `toml:"mcp"`
	HermesSkill    bool              `toml:"hermes_skill"`
	RestartPolicy  string            `toml:"restart_policy"`
	BackoffSeconds uint              `toml:"backoff_seconds"`
	// StartGraceSeconds is how long after a start the healthcheck may fail
	// without counting against the service, for anything slow to bind its port.
	StartGraceSeconds uint `toml:"start_grace_seconds"`
}

// Load reads and parses a TOML file into a Service.
func Load(path string) (*Service, error) {
	var svc Service
	_, err := toml.DecodeFile(path, &svc)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if svc.Name == "" {
		// Filename minus extension is the service name unless overridden (§5).
		base := filepath.Base(path)
		svc.Name = strings.TrimSuffix(base, filepath.Ext(base))
	}
	svc.setDefault()
	return &svc, nil
}

// LoadDir reads all .toml files in the given directory and returns the parsed
// services along with any validation errors.
func LoadDir(dir string) ([]*Service, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read dir %s: %w", dir, err)
	}

	var services []*Service
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		svc, err := Load(filepath.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		services = append(services, svc)
	}

	return services, nil
}

// validate checks a single Service for schema violations.
func (s *Service) validate() []error {
	var errs []error

	if s.Name == "" {
		errs = append(errs, fmt.Errorf("name is required"))
	}
	if s.Runtime == "" {
		errs = append(errs, fmt.Errorf("runtime is required"))
	} else if s.Runtime != "process" && s.Runtime != "container" {
		errs = append(errs, fmt.Errorf("runtime must be \"process\" or \"container\", got %q", s.Runtime))
	}
	if s.Port <= 0 {
		errs = append(errs, fmt.Errorf("port is required and must be > 0"))
	}
	if s.Storage == "" {
		errs = append(errs, fmt.Errorf("storage is required"))
	}
	if s.TailscaleName == "" {
		errs = append(errs, fmt.Errorf("tailscale_name is required"))
	}

	// runtime-specific checks
	if s.Runtime == "process" && s.Binary == "" {
		errs = append(errs, fmt.Errorf("binary is required for runtime = \"process\""))
	}
	if s.Runtime == "container" && s.Image == "" {
		errs = append(errs, fmt.Errorf("image is required for runtime = \"container\""))
	}

	if s.Healthcheck == "" || !strings.HasPrefix(s.Healthcheck, "/") {
		errs = append(errs, fmt.Errorf("healthcheck must start with \"/\""))
	}

	validPolicies := map[string]bool{"on-failure": true, "always": true, "never": true}
	if s.RestartPolicy == "" {
		// default is set in setDefault
	} else if !validPolicies[s.RestartPolicy] {
		errs = append(errs, fmt.Errorf("restart_policy must be one of \"on-failure\", \"always\", \"never\""))
	}

	if s.Funnel {
		errs = append(errs, fmt.Errorf("funnel is not supported in v1"))
	}

	return errs
}

// ValidateAll validates a set of services for cross-service constraints.
func ValidateAll(services []*Service) []error {
	var errs []error

	names := make(map[string]bool)
	ports := make(map[int]bool)
	tsNames := make(map[string]bool)

	for _, svc := range services {
		// Per-service validation
		for _, err := range svc.validate() {
			errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
		}

		// Cross-service uniqueness
		if names[svc.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", svc.Name))
		}
		names[svc.Name] = true

		// Port uniqueness for process services
		if svc.Runtime == "process" {
			if ports[svc.Port] {
				errs = append(errs, fmt.Errorf("%s: duplicate port %d", svc.Name, svc.Port))
			}
			ports[svc.Port] = true
		}

		if tsNames[svc.TailscaleName] {
			errs = append(errs, fmt.Errorf("%s: duplicate tailscale_name %q", svc.Name, svc.TailscaleName))
		}
		tsNames[svc.TailscaleName] = true
	}

	return errs
}

func (s *Service) setDefault() {
	if s.Runtime == "" {
		s.Runtime = "process"
	}
	if s.Healthcheck == "" {
		s.Healthcheck = "/up"
	}
	if s.RestartPolicy == "" {
		s.RestartPolicy = "on-failure"
	}
	if s.BackoffSeconds == 0 {
		s.BackoffSeconds = 5
	}
	if s.StartGraceSeconds == 0 {
		s.StartGraceSeconds = 30
	}
}

// ExpandStorage expands ~ in the storage path and resolves to an absolute path.
// It does not modify the manifest.
func (s *Service) ExpandStorage() string {
	path := s.Storage
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	return filepath.Clean(path)
}

// EnsureStorage creates the service's storage directory with mode 0700 if it is
// absent, and verifies it is a writable directory if it already exists (§5).
func (s *Service) EnsureStorage() error {
	path := s.ExpandStorage()

	fi, err := os.Stat(path)
	switch {
	case err == nil:
		if !fi.IsDir() {
			return fmt.Errorf("storage path %s exists and is not a directory", path)
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(path, 0o700); err != nil {
			return fmt.Errorf("create storage %s: %w", path, err)
		}
		// MkdirAll honours umask, so set the mode explicitly.
		if err := os.Chmod(path, 0o700); err != nil {
			return fmt.Errorf("chmod storage %s: %w", path, err)
		}
		return nil
	default:
		return fmt.Errorf("stat storage %s: %w", path, err)
	}

	// Probe writability rather than inspecting mode bits: the directory may sit
	// on a read-only mount, or be owned by another user.
	probe, err := os.CreateTemp(path, ".outpost-write-probe-*")
	if err != nil {
		return fmt.Errorf("storage path %s is not writable: %w", path, err)
	}
	probe.Close()
	os.Remove(probe.Name())
	return nil
}

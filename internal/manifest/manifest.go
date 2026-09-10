package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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

	// root is the Outpost home that a relative storage path resolves against.
	// Unexported so TOML cannot set it: it comes from where the manifest was
	// loaded, never from the manifest's own contents.
	root string
}

// SetRoot records the Outpost home a relative storage path resolves against.
// Load and LoadDir do this themselves; callers that build a Service by hand
// need it so `storage = "data/x"` does not depend on the process's working
// directory.
func (s *Service) SetRoot(root string) {
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	s.root = root
}

// Load reads and parses a TOML file into a Service.
func Load(path string) (*Service, error) {
	var svc Service
	_, err := toml.DecodeFile(path, &svc)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	// Manifests live at <root>/services/<name>.toml, so the root is two levels
	// up. A relative storage path resolves against that rather than against
	// whatever working directory launchd or systemd happened to hand us.
	svc.SetRoot(filepath.Dir(filepath.Dir(path)))
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

// validateWith checks a single Service for schema violations.
func (s *Service) validateWith(opts Options) []error {
	var errs []error

	if s.Name == "" {
		errs = append(errs, fmt.Errorf("name is required"))
	}
	if s.Runtime == "" {
		errs = append(errs, fmt.Errorf("runtime is required"))
	} else if s.Runtime != "process" {
		errs = append(errs, fmt.Errorf("runtime must be \"process\", got %q: Outpost supervises native processes only", s.Runtime))
	}
	if s.Port <= 0 {
		errs = append(errs, fmt.Errorf("port is required and must be > 0"))
	}
	if s.Storage == "" {
		errs = append(errs, fmt.Errorf("storage is required"))
	} else if escapesRoot(s.Storage) {
		errs = append(errs, fmt.Errorf("storage %q climbs above the Outpost root; use an absolute path if that is deliberate", s.Storage))
	}
	if opts.RequireTailscale && s.TailscaleName == "" {
		errs = append(errs, fmt.Errorf("tailscale_name is required (or set tailscale.enabled = false in config.toml)"))
	}

	if s.Binary == "" {
		errs = append(errs, fmt.Errorf("binary is required"))
	}

	// The port is declared once for Outpost and again in whatever way the
	// service takes it. If those disagree, the service runs fine and Outpost
	// health-checks a port nothing is listening on, restarting it forever.
	if argPort, ok := portInArgs(s.Args); ok && s.Port > 0 && argPort != s.Port {
		errs = append(errs, fmt.Errorf("port = %d but args pass --port %d; the healthcheck would probe a port the service is not listening on",
			s.Port, argPort))
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

// Options controls the validation rules that depend on global configuration.
type Options struct {
	// RequireTailscale makes tailscale_name mandatory. False when the
	// installation is not using Tailscale at all (config.toml).
	RequireTailscale bool
}

// DefaultOptions is the strictest set: Tailscale in use.
func DefaultOptions() Options { return Options{RequireTailscale: true} }

// ValidateAll validates a set of services with the default options.
func ValidateAll(services []*Service) []error {
	return ValidateWith(services, DefaultOptions())
}

// ValidateWith validates a set of services for cross-service constraints.
func ValidateWith(services []*Service, opts Options) []error {
	var errs []error

	names := make(map[string]bool)
	ports := make(map[int]bool)
	tsNames := make(map[string]bool)

	for _, svc := range services {
		// Per-service validation
		for _, err := range svc.validateWith(opts) {
			errs = append(errs, fmt.Errorf("%s: %w", svc.Name, err))
		}

		// Cross-service uniqueness
		if names[svc.Name] {
			errs = append(errs, fmt.Errorf("%s: duplicate name", svc.Name))
		}
		names[svc.Name] = true

		if ports[svc.Port] {
			errs = append(errs, fmt.Errorf("%s: duplicate port %d", svc.Name, svc.Port))
		}
		ports[svc.Port] = true

		// An empty name is not a collision; it is simply unset.
		if svc.TailscaleName != "" {
			if tsNames[svc.TailscaleName] {
				errs = append(errs, fmt.Errorf("%s: duplicate tailscale_name %q", svc.Name, svc.TailscaleName))
			}
			tsNames[svc.TailscaleName] = true
		}
	}

	return errs
}

// portInArgs finds a port passed in args as --port N, -port N or --port=N.
//
// A heuristic, deliberately: it only fires on the conventional spellings, and
// says nothing about a service that takes its port some other way.
func portInArgs(args []string) (int, bool) {
	for i, arg := range args {
		name, value, hasValue := strings.Cut(arg, "=")
		if name != "--port" && name != "-port" {
			continue
		}
		if !hasValue {
			if i+1 >= len(args) {
				return 0, false
			}
			value = args[i+1]
		}
		port, err := strconv.Atoi(value)
		if err != nil {
			return 0, false
		}
		return port, true
	}
	return 0, false
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

// escapesRoot reports whether a relative storage path climbs out of the Outpost
// root with "..". Absolute and ~-rooted paths are the operator saying plainly
// where they want the data, and are left alone.
func escapesRoot(path string) bool {
	if filepath.IsAbs(path) || strings.HasPrefix(path, "~/") {
		return false
	}
	cleaned := filepath.Clean(path)
	return cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator))
}

// ExpandStorage expands ~ in the storage path and resolves to an absolute path.
// It does not modify the manifest.
//
// A relative path resolves against the Outpost root recorded at load time, so
// the same manifest names the same directory however the daemon was started.
// With no root recorded the path is left relative, which is what a Service
// built by hand outside a manifest directory should get.
func (s *Service) ExpandStorage() string {
	path := s.Storage
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	}
	if !filepath.IsAbs(path) && s.root != "" {
		path = filepath.Join(s.root, path)
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

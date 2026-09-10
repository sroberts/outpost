package acquire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// DescriptorName is the file a service publishes to describe how to run it.
const DescriptorName = "outpost.toml"

// Descriptor is a service declaring its own defaults, so `outpost add` can
// generate a correct manifest instead of guessing. Every field is optional;
// an absent one falls back to the same default a hand-written `add` would get.
type Descriptor struct {
	// Name overrides the repository name as the service name.
	Name string `toml:"name"`
	// Port is a fixed port the service insists on. Usually absent: letting
	// Outpost pick a free one and passing it through Args is what avoids
	// collisions between services.
	Port int `toml:"port"`
	// Args are passed to the binary. "${PORT}" is replaced with the port
	// Outpost assigned, which is how a service that takes its port as a flag
	// gets told what it is.
	Args []string `toml:"args"`
	// Env is added to the service's environment, with the same substitution.
	Env map[string]string `toml:"env"`
	// Healthcheck is the readiness path; defaults to /up.
	Healthcheck string `toml:"healthcheck"`
	// MCP marks a service that serves an MCP endpoint at /mcp.
	MCP bool `toml:"mcp"`
	// RestartPolicy is on-failure, always or never.
	RestartPolicy string `toml:"restart_policy"`
	// StartGraceSeconds is how long the service may take to answer its
	// healthcheck before a failure counts against it.
	StartGraceSeconds uint `toml:"start_grace_seconds"`
	// Asset names the release asset holding the binary, as a template over
	// ${GOOS} and ${GOARCH}. Absent means match on a name containing both,
	// which is what a conventional Go release already produces.
	Asset string `toml:"asset"`
}

// ParseDescriptor reads an outpost.toml.
func ParseDescriptor(data []byte) (*Descriptor, error) {
	var d Descriptor
	if err := toml.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("parse %s: %w", DescriptorName, err)
	}
	if err := d.validate(); err != nil {
		return nil, err
	}
	return &d, nil
}

func (d *Descriptor) validate() error {
	if d.Port < 0 || d.Port > 65535 {
		return fmt.Errorf("%s: port %d is out of range", DescriptorName, d.Port)
	}
	if d.Healthcheck != "" && !strings.HasPrefix(d.Healthcheck, "/") {
		return fmt.Errorf("%s: healthcheck %q must start with /", DescriptorName, d.Healthcheck)
	}
	switch d.RestartPolicy {
	case "", "on-failure", "always", "never":
	default:
		return fmt.Errorf("%s: restart_policy %q must be on-failure, always or never", DescriptorName, d.RestartPolicy)
	}
	return nil
}

// Expand substitutes ${PORT} in args and env. A service is told its port this
// way because Outpost chooses the port, not the service.
func (d *Descriptor) Expand(port int) (args []string, env map[string]string) {
	replace := strings.NewReplacer("${PORT}", strconv.Itoa(port))

	for _, a := range d.Args {
		args = append(args, replace.Replace(a))
	}
	if len(d.Env) > 0 {
		env = make(map[string]string, len(d.Env))
		for k, v := range d.Env {
			env[k] = replace.Replace(v)
		}
	}
	return args, env
}

// assetName is the release asset to look for on this platform.
func (d *Descriptor) assetName(goos, goarch string) string {
	if d == nil || d.Asset == "" {
		return ""
	}
	return strings.NewReplacer("${GOOS}", goos, "${GOARCH}", goarch).Replace(d.Asset)
}

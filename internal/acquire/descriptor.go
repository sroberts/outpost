package acquire

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/sroberts/outpost/internal/manifest"
)

// DescriptorName is the file a service publishes to describe how to run it.
const DescriptorName = "outpost.toml"

// maxStartGraceSeconds caps what a descriptor may ask for. Ten minutes is far
// longer than anything Outpost supervises should need to bind a port, and
// short enough that a wedged service is still noticed the same day.
const maxStartGraceSeconds = 600

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
	// Title and Description are what the gateway's catalog shows. A
	// publisher may set them, and public_paths, but not who may open the app
	// or where it's served: allow, subdomain and hostname are the operator's.
	Title       string   `toml:"title"`
	Description string   `toml:"description"`
	PublicPaths []string `toml:"public_paths"`
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
	// A grace period suppresses healthcheck failures, and unlike healthcheck,
	// args and env there is no flag to override what a descriptor asks for.
	// An unbounded one from a third-party repository would mean a service
	// whose failures never count, recoverable only by hand-editing the
	// generated manifest.
	if d.StartGraceSeconds > maxStartGraceSeconds {
		return fmt.Errorf("%s: start_grace_seconds %d is longer than the %d-second maximum",
			DescriptorName, d.StartGraceSeconds, maxStartGraceSeconds)
	}
	for _, p := range d.PublicPaths {
		if err := manifest.ValidPublicPath(p); err != nil {
			return fmt.Errorf("%s: %w", DescriptorName, err)
		}
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

// TemplatesPort reports whether the descriptor passes the assigned port on to
// the service, through either args or env.
func (d *Descriptor) TemplatesPort() bool {
	for _, a := range d.Args {
		if strings.Contains(a, "${PORT}") {
			return true
		}
	}
	for _, v := range d.Env {
		if strings.Contains(v, "${PORT}") {
			return true
		}
	}
	return false
}

// assetName is the release asset to look for on this platform.
func (d *Descriptor) assetName(goos, goarch string) string {
	if d == nil || d.Asset == "" {
		return ""
	}
	return strings.NewReplacer("${GOOS}", goos, "${GOARCH}", goarch).Replace(d.Asset)
}

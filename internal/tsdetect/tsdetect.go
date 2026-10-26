// Package tsdetect reports whether Tailscale is running on this machine, so
// the installer can offer to turn on per-service Tailscale identities.
//
// It asks the tailscale CLI rather than linking Tailscale's client library,
// so the daemon still links no tailscale.com code outside internal/tsnet.
// Detection only decides what to offer: per-service nodes don't need the
// system Tailscale at all, so its absence never blocks turning them on.
package tsdetect

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds the whole check. A wedged tailscaled must not stall an
// install.
const Timeout = 2 * time.Second

// macAppCLI is where the Mac App Store and standalone apps keep the CLI,
// which isn't on PATH unless the user linked it there.
const macAppCLI = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"

// Result is what detection found.
type Result struct {
	// Found means the CLI exists and its backend is Running.
	Found bool
	// Tailnet is the tailnet's name, such as "tail1234.ts.net", when known.
	Tailnet string
}

// detector holds the seams tests replace: finding the CLI and running it.
type detector struct {
	lookPath func(file string) (string, error)
	exists   func(path string) bool
	run      func(ctx context.Context, path string, args ...string) ([]byte, error)
}

var system = detector{
	lookPath: exec.LookPath,
	exists: func(path string) bool {
		fi, err := os.Stat(path)
		return err == nil && !fi.IsDir()
	},
	run: func(ctx context.Context, path string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, path, args...).Output()
	},
}

// Detect looks for a running Tailscale. Any failure, whether no CLI, a
// stopped backend, a timeout or unreadable output, reads as not found, never
// as an error: the answer only shapes a question.
func Detect(ctx context.Context) Result {
	return system.detect(ctx)
}

// status is the part of `tailscale status --json` this package reads.
type status struct {
	BackendState   string
	MagicDNSSuffix string
	CurrentTailnet *struct {
		Name           string
		MagicDNSSuffix string
	}
}

func (d detector) detect(ctx context.Context) Result {
	path := d.cli()
	if path == "" {
		return Result{}
	}
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	out, err := d.run(ctx, path, "status", "--json")
	if err != nil {
		return Result{}
	}
	var st status
	if err := json.Unmarshal(out, &st); err != nil || st.BackendState != "Running" {
		return Result{}
	}
	return Result{Found: true, Tailnet: tailnetName(st)}
}

// cli finds the tailscale binary, or returns "".
func (d detector) cli() string {
	if p, err := d.lookPath("tailscale"); err == nil {
		return p
	}
	if d.exists(macAppCLI) {
		return macAppCLI
	}
	return ""
}

// tailnetName prefers the MagicDNS suffix, which is the domain people see in
// hostnames. CurrentTailnet.Name can be an email address for a personal
// tailnet, which is less useful in a prompt.
func tailnetName(st status) string {
	if st.CurrentTailnet != nil {
		if s := strings.TrimSuffix(st.CurrentTailnet.MagicDNSSuffix, "."); s != "" {
			return s
		}
	}
	if s := strings.TrimSuffix(st.MagicDNSSuffix, "."); s != "" {
		return s
	}
	if st.CurrentTailnet != nil {
		return st.CurrentTailnet.Name
	}
	return ""
}

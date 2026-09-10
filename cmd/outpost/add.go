package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sroberts/outpost/internal/manifest"
)

// cmdAdd writes a new service manifest (§1: "CLI to add/remove/... services").
//
// It generates the file the operator would otherwise hand-write, picking a free
// port and sensible defaults, and validates the result against the rest of the
// declared set before writing anything.
func (a *app) cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(a.err)
	binary := fs.String("binary", "", "path to the service executable (required)")
	port := fs.Int("port", 0, "port the service listens on (default: a free one)")
	storage := fs.String("storage", "", "storage directory (default: <home>/data/<name>)")
	tsName := fs.String("tailscale-name", "", "tailnet hostname (default: the service name)")
	health := fs.String("healthcheck", "/up", "healthcheck path")
	policy := fs.String("restart", "on-failure", `restart policy: on-failure, always or never`)
	argList := fs.String("args", "", "comma-separated arguments passed to the binary")
	envList := fs.String("env", "", "comma-separated KEY=VALUE pairs")
	mcp := fs.Bool("mcp", false, "the service exposes an MCP endpoint at /mcp")
	force := fs.Bool("force", false, "overwrite an existing manifest")
	dryRun := fs.Bool("print", false, "print the manifest instead of writing it")
	asName := fs.String("name", "", "service name (default: the repository name, when adding from a URL)")

	name, err := parseServiceCommand(fs, "add", args)
	if err != nil {
		return err
	}

	// A reference containing a slash is a repository, never a service name.
	if isRepoRef(name) {
		return a.addFromRepo(name, addOptions{
			set:     flagsSet(fs),
			name:    *asName,
			port:    *port,
			storage: *storage,
			tsName:  *tsName,
			health:  *health,
			policy:  *policy,
			argList: *argList,
			envList: *envList,
			mcp:     *mcp,
			force:   *force,
			dryRun:  *dryRun,
		})
	}

	if *binary == "" {
		return fmt.Errorf("add: --binary is required, or give a GitHub repository URL instead")
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	path := filepath.Join(a.servicesDir(), name+".toml")
	if _, err := os.Stat(path); err == nil && !*force && !*dryRun {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	svc, err := a.buildService(name, *binary, *port, *storage, *tsName, *health, *policy, *argList, *envList, *mcp)
	if err != nil {
		return err
	}

	// Validate against everything else already declared, so a collision is
	// caught before the file lands rather than at the next apply.
	if err := a.validateAgainstExisting(svc, name); err != nil {
		return err
	}

	body := renderManifest(svc)
	if *dryRun {
		fmt.Fprint(a.out, body)
		return nil
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := svc.EnsureStorage(); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "wrote %s\n", path)
	fmt.Fprintf(a.out, "  port %d · storage %s\n", svc.Port, svc.ExpandStorage())
	if c := a.client(); c.Available() {
		fmt.Fprintln(a.out, "\nThe running daemon will pick it up within one interval.")
	} else {
		fmt.Fprintln(a.out, "\nStart it with `outpost serve`.")
	}
	return nil
}

// buildService assembles the manifest from flags and defaults.
func (a *app) buildService(name, binary string, port int, storage, tsName, health, policy, argList, envList string, mcp bool) (*manifest.Service, error) {
	if binary != "" {
		if resolved, err := exec.LookPath(binary); err == nil {
			binary = resolved
		} else {
			return nil, fmt.Errorf("binary %s: %w", binary, err)
		}
		if abs, err := filepath.Abs(binary); err == nil {
			binary = abs
		}
	}

	if port == 0 {
		free, err := allocatePort()
		if err != nil {
			return nil, err
		}
		port = free
	}
	if storage == "" {
		storage = filepath.Join(a.home, "data", name)
	}
	// Only default a tailnet name when Tailscale is actually in use.
	if tsName == "" && a.config.TailscaleEnabled() {
		tsName = name
	}

	svc := &manifest.Service{
		Name:          name,
		Runtime:       "process",
		Binary:        binary,
		Port:          port,
		Storage:       storage,
		TailscaleName: tsName,
		Healthcheck:   health,
		RestartPolicy: policy,
		MCP:           mcp,
	}
	// A hand-written --storage may be relative; anchor it to the Outpost root
	// so it means the same thing here as it will once the daemon loads it.
	svc.SetRoot(a.home)
	if argList != "" {
		svc.Args = splitList(argList)
	}
	if envList != "" {
		env, err := parseEnv(splitList(envList))
		if err != nil {
			return nil, err
		}
		svc.Env = env
	}
	return svc, nil
}

// parseEnv turns KEY=VALUE pairs into a map.
func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := map[string]string{}
	for _, pair := range pairs {
		k, v, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("--env %q is not KEY=VALUE", pair)
		}
		env[k] = v
	}
	return env, nil
}

// validateAgainstExisting checks the new service against the declared set.
func (a *app) validateAgainstExisting(svc *manifest.Service, name string) error {
	existing, err := manifest.LoadDir(a.servicesDir())
	if err != nil {
		return err
	}
	set := []*manifest.Service{svc}
	for _, other := range existing {
		if other.Name == name {
			continue // being replaced
		}
		set = append(set, other)
	}

	opts := manifest.Options{RequireTailscale: a.config.TailscaleEnabled()}
	if errs := manifest.ValidateWith(set, opts); len(errs) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "the resulting manifest set would be invalid:")
		for _, e := range errs {
			fmt.Fprintf(&b, "\n  - %v", e)
		}
		return fmt.Errorf("%s", b.String())
	}
	return nil
}

// cmdRemove deletes a service manifest, and optionally its data.
func (a *app) cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(a.err)
	purge := fs.Bool("purge", false, "also delete the service's storage directory and backups")
	yes := fs.Bool("yes", false, "do not ask for confirmation when purging")

	name, err := parseServiceCommand(fs, "remove", args)
	if err != nil {
		return err
	}

	path := filepath.Join(a.servicesDir(), name+".toml")
	svc, err := manifest.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no service %q declared in %s", name, a.servicesDir())
		}
		return err
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	fmt.Fprintf(a.out, "removed %s\n", path)

	// The daemon stops the service on its next reconcile, because it is no
	// longer declared.
	if c := a.client(); c.Available() {
		fmt.Fprintln(a.out, "The running daemon will stop it within one interval.")
	}

	if !*purge {
		fmt.Fprintf(a.out, "\nIts data is still at %s\n", svc.ExpandStorage())
		fmt.Fprintln(a.out, "Delete it too with `outpost remove "+name+" --purge`.")
		return nil
	}

	// Purging destroys data that backups may be the only other copy of, so
	// confirm unless told not to.
	storage := svc.ExpandStorage()
	backups := filepath.Join(a.backupsDir(), name)
	if !*yes {
		fmt.Fprintf(a.out, "\nThis will permanently delete:\n  %s\n  %s\n", storage, backups)
		fmt.Fprint(a.out, "Type the service name to confirm: ")
		var typed string
		// A read error leaves typed empty, which fails the comparison below —
		// a non-interactive stdin must not count as confirmation.
		_, _ = fmt.Fscanln(os.Stdin, &typed)
		if typed != name {
			return fmt.Errorf("not confirmed; data left in place")
		}
	}

	for _, dir := range []string{storage, backups} {
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("remove %s: %w", dir, err)
		}
		fmt.Fprintf(a.out, "deleted %s\n", dir)
	}
	return nil
}

// renderManifest writes a service out as commented TOML.
func renderManifest(s *manifest.Service) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s — generated by `outpost add`\n", s.Name)
	fmt.Fprintf(&b, "name           = %q\n", s.Name)
	fmt.Fprintf(&b, "runtime        = %q\n", s.Runtime)
	fmt.Fprintf(&b, "binary         = %q\n", s.Binary)
	if len(s.Args) > 0 {
		fmt.Fprintf(&b, "args           = %s\n", tomlStrings(s.Args))
	}
	if len(s.Env) > 0 {
		fmt.Fprintf(&b, "env            = %s\n", tomlTable(s.Env))
	}
	fmt.Fprintf(&b, "port           = %d\n", s.Port)
	fmt.Fprintf(&b, "storage        = %q\n", s.Storage)
	if s.TailscaleName != "" {
		fmt.Fprintf(&b, "tailscale_name = %q\n", s.TailscaleName)
	}
	fmt.Fprintf(&b, "healthcheck    = %q\n", s.Healthcheck)
	fmt.Fprintf(&b, "restart_policy = %q\n", s.RestartPolicy)
	if s.MCP {
		b.WriteString("mcp            = true\n")
	}
	// Only written when set, so a generated manifest stays as short as the
	// service actually needs. Both were previously dropped on the floor: `add`
	// had no flag for either, so nothing ever set them until a descriptor could.
	if s.BackoffSeconds > 0 {
		fmt.Fprintf(&b, "backoff_seconds = %d\n", s.BackoffSeconds)
	}
	if s.StartGraceSeconds > 0 {
		fmt.Fprintf(&b, "start_grace_seconds = %d\n", s.StartGraceSeconds)
	}
	return b.String()
}

func tomlStrings(items []string) string {
	quoted := make([]string, len(items))
	for i, s := range items {
		quoted[i] = fmt.Sprintf("%q", s)
	}
	return "[" + strings.Join(quoted, ", ") + "]"
}

func tomlTable(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	pairs := make([]string, 0, len(keys))
	for _, k := range keys {
		pairs = append(pairs, fmt.Sprintf("%s = %q", k, m[k]))
	}
	return "{ " + strings.Join(pairs, ", ") + " }"
}

// splitList splits a comma-separated flag value, ignoring empty entries.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// allocatePort asks the kernel for an unused port.
func allocatePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("find a free port: %w", err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

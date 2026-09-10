package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/manifest"
)

// addOptions carries the flags `add` was given, plus which of them the
// operator actually set. The distinction matters: a descriptor's value should
// beat a flag's default but never beat an explicit flag.
type addOptions struct {
	set     map[string]bool
	name    string
	port    int
	storage string
	tsName  string
	health  string
	policy  string
	argList string
	envList string
	mcp     bool
	force   bool
	dryRun  bool
}

// flagsSet records which flags were given explicitly.
func flagsSet(fs *flag.FlagSet) map[string]bool {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	return set
}

// acquireClient is a seam. Production builds a client that talks to GitHub;
// tests point it at a local stand-in so none of this reaches the network.
var acquireClient = acquire.NewClient

// isRepoRef distinguishes a repository from a service name. Service names may
// not contain a slash, so anything that does is a reference to a repository.
func isRepoRef(s string) bool {
	return strings.Contains(s, "/") || strings.Contains(s, "://")
}

// addFromRepo installs a service from a GitHub release and writes its manifest.
func (a *app) addFromRepo(ref string, opts addOptions) error {
	repo, err := acquire.ParseRepo(ref)
	if err != nil {
		return fmt.Errorf("add %s: %w", ref, err)
	}
	if err := a.ensureLayout(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	client := acquireClient()
	fmt.Fprintf(a.out, "looking for the latest release of %s…\n", repo)
	rel, err := client.LatestRelease(ctx, repo)
	if err != nil {
		return err
	}

	desc, err := client.Descriptor(ctx, repo, rel)
	if err != nil {
		return err
	}
	if desc == nil {
		desc = &acquire.Descriptor{}
		fmt.Fprintf(a.err, "outpost: %s publishes no %s; using defaults and the flags given\n",
			repo, acquire.DescriptorName)
	}

	name := serviceName(opts.name, desc.Name, repo.Name)
	if err := validServiceName(name); err != nil {
		return fmt.Errorf("%w (use --name to choose one)", err)
	}

	path := filepath.Join(a.servicesDir(), name+".toml")
	if _, err := os.Stat(path); err == nil && !opts.force && !opts.dryRun {
		return fmt.Errorf("%s already exists (use --force to overwrite)", path)
	}

	asset, err := client.SelectAsset(rel, desc)
	if err != nil {
		return err
	}

	// The port has to be settled before the binary is written, because the
	// descriptor's args are templated on it.
	port := opts.port
	if port == 0 {
		port = desc.Port
	}
	if port == 0 {
		if port, err = allocatePort(); err != nil {
			return err
		}
	}

	dest := filepath.Join(a.home, "bin", name)

	// Built and validated before anything is downloaded: a manifest the
	// declared set rejects — a port collision, or descriptor args naming a
	// port Outpost did not assign — must not leave an installed binary
	// behind, least of all one that overwrote a working service's.
	svc, err := a.serviceFromDescriptor(name, dest, port, desc, opts)
	if err != nil {
		return err
	}
	if err := a.validateAgainstExisting(svc, name); err != nil {
		return err
	}

	body := renderManifest(svc)
	if opts.dryRun {
		fmt.Fprintf(a.out, "would install %s from release %s to %s\n", asset.Name, rel.Tag, dest)
		fmt.Fprint(a.out, body)
		return nil
	}

	fmt.Fprintf(a.out, "downloading %s from release %s…\n", asset.Name, rel.Tag)
	res, err := client.Install(ctx, rel, asset, dest)
	if err != nil {
		return err
	}
	if res.Warning != "" {
		fmt.Fprintf(a.err, "outpost: %s\n", res.Warning)
	}
	verified := "unverified"
	if res.Verified {
		verified = "checksum verified"
	}
	fmt.Fprintf(a.out, "installed %s (%s)\n", dest, verified)

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := svc.EnsureStorage(); err != nil {
		return err
	}

	fmt.Fprintf(a.out, "wrote %s\n", path)
	fmt.Fprintf(a.out, "  %s %s · port %d · storage %s\n", repo, rel.Tag, svc.Port, svc.ExpandStorage())
	if c := a.client(); c.Available() {
		fmt.Fprintln(a.out, "\nThe running daemon will pick it up within one interval.")
	} else {
		fmt.Fprintln(a.out, "\nStart it with `outpost serve`.")
	}
	return nil
}

// validServiceName rejects anything that would not be a safe manifest
// filename. The name comes from a repository or a descriptor here, so unlike a
// hand-typed one it is worth checking before it becomes a path.
func validServiceName(name string) error {
	switch {
	case name == "":
		return fmt.Errorf("service name is empty")
	case name == "." || name == "..":
		return fmt.Errorf("service name %q is not usable", name)
	case strings.ContainsAny(name, `/\`), strings.ContainsRune(name, os.PathSeparator):
		return fmt.Errorf("service name %q contains a path separator", name)
	case strings.HasPrefix(name, "."):
		return fmt.Errorf("service name %q starts with a dot", name)
	}
	return nil
}

// serviceName is the first of the explicit name, the descriptor's, and the
// repository's that is set.
func serviceName(flagName, descName, repoName string) string {
	for _, candidate := range []string{flagName, descName, repoName} {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// serviceFromDescriptor merges the descriptor with the flags. An explicitly
// given flag always wins; otherwise the descriptor speaks, because the service
// knows its own shape better than `add`'s defaults do.
func (a *app) serviceFromDescriptor(name, binary string, port int, desc *acquire.Descriptor, opts addOptions) (*manifest.Service, error) {
	args, env := desc.Expand(port)

	if opts.set["args"] {
		args = splitList(opts.argList)
	}
	if opts.set["env"] {
		parsed, err := parseEnv(splitList(opts.envList))
		if err != nil {
			return nil, err
		}
		env = parsed
	}

	health := desc.Healthcheck
	if health == "" || opts.set["healthcheck"] {
		health = opts.health
	}
	policy := desc.RestartPolicy
	if policy == "" || opts.set["restart"] {
		policy = opts.policy
	}
	mcp := desc.MCP
	if opts.set["mcp"] {
		mcp = opts.mcp
	}

	tsName := opts.tsName
	if tsName == "" && a.config.TailscaleEnabled() {
		tsName = name
	}
	storage := opts.storage
	if storage == "" {
		storage = filepath.Join(a.home, "data", name)
	}

	svc := &manifest.Service{
		Name:              name,
		Runtime:           "process",
		Binary:            binary,
		Args:              args,
		Env:               env,
		Port:              port,
		Storage:           storage,
		TailscaleName:     tsName,
		Healthcheck:       health,
		RestartPolicy:     policy,
		MCP:               mcp,
		StartGraceSeconds: desc.StartGraceSeconds,
	}
	svc.Env = withOutpostPort(svc.Env, port)
	svc.SetRoot(a.home)
	return svc, nil
}

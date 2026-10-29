package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/state"
)

// updatePoll is how often an update checks on the restarted service; a
// variable so tests need not wait out real seconds.
var updatePoll = 500 * time.Millisecond

// cmdUpdate replaces a service's binary with another release from the
// repository `outpost add` installed it from, restarts it, and puts the old
// binary back if the new one does not come up healthy.
//
// Only the binary changes. Args, env and the rest of the manifest are the
// operator's by now, and may have been edited since `add` wrote them.
func (a *app) cmdUpdate(args []string) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(a.err)
	check := fs.Bool("check", false, "report whether a newer release exists, and change nothing")
	to := fs.String("to", "", "install this release tag instead of the latest (also how to go back)")
	from := fs.String("from", "", "the GitHub repository a service came from, for one whose manifest does not say")
	force := fs.Bool("force", false, "reinstall even when the release is the one already installed")
	noBackup := fs.Bool("no-backup", false, "skip the backup of the service's storage taken before updating")
	wait := fs.Duration("wait", 0, "how long the new release has to pass its healthcheck (default 30s plus start_grace_seconds)")
	positional, err := parseOptionalService(fs, "update", args)
	if err != nil {
		return err
	}
	switch {
	case len(positional) > 1:
		return fmt.Errorf("update: unexpected argument %q", positional[1])
	case len(positional) == 0 && *check:
		return a.checkAll()
	case len(positional) == 0:
		return fmt.Errorf("update: a service name is required (or `outpost update --check` to see every service)")
	}
	name := positional[0]

	path := filepath.Join(a.servicesDir(), name+".toml")
	original, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no service %q declared in %s", name, a.servicesDir())
		}
		return err
	}
	svc, err := manifest.Load(path)
	if err != nil {
		return err
	}

	source := svc.Source
	if *from != "" {
		repo, err := acquire.ParseRepo(*from)
		if err != nil {
			return fmt.Errorf("update --from %s: %w", *from, err)
		}
		// Switching a service to a different repository is not an update.
		if source != "" && source != repo.String() {
			return fmt.Errorf("%s was installed from %s; edit source in %s if it really comes from %s now",
				name, source, path, repo)
		}
		source = repo.String()
	}
	if source == "" {
		return fmt.Errorf("%s has no source in its manifest, so there is nothing to update it from; "+
			"if it came from a GitHub release, say which: `outpost update %s --from owner/repo`", name, name)
	}
	repo, err := acquire.ParseRepo(source)
	if err != nil {
		return fmt.Errorf("%s: source %q: %w", path, source, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	client := acquireClient()
	client.Logf = func(format string, args ...any) {
		fmt.Fprintf(a.err, "outpost: "+format+"\n", args...)
	}
	var rel *acquire.Release
	if *to != "" {
		rel, err = client.ReleaseByTag(ctx, repo, *to)
	} else {
		rel, err = client.LatestRelease(ctx, repo)
	}
	var none *acquire.NoAssetsError
	if errors.As(err, &none) {
		return fmt.Errorf("release %s of %s has nothing attached to install yet; if it was just published, "+
			"its build may still be uploading. Try again later, or `--to` an earlier release", none.Tag, none.Repo)
	}
	if err != nil {
		return err
	}

	installed := orDash(svc.Release)
	if *check {
		if svc.Release == rel.Tag {
			fmt.Fprintf(a.out, "%s is up to date (%s from %s)\n", name, rel.Tag, repo)
			return nil
		}
		fmt.Fprintf(a.out, "%s: %s installed, %s available from %s\n", name, installed, rel.Tag, repo)
		fmt.Fprintf(a.out, "Install it with `outpost update %s`.\n", name)
		return nil
	}
	if svc.Release == rel.Tag && !*force {
		fmt.Fprintf(a.out, "%s is already at %s (use --force to reinstall it)\n", name, rel.Tag)
		return nil
	}

	// Replacing a binary Outpost did not put there could mean overwriting
	// something the system or another tool owns.
	if !a.ownsBinary(svc.Binary) {
		return fmt.Errorf("%s runs %s, which is outside %s; update only replaces binaries `outpost add` installed",
			name, svc.Binary, filepath.Join(a.home, "bin"))
	}

	desc, err := client.Descriptor(ctx, repo, rel)
	if err != nil {
		return err
	}
	if desc == nil {
		desc = &acquire.Descriptor{}
	}
	asset, err := client.SelectAsset(rel, desc)
	var missing *acquire.NoPlatformAssetError
	if errors.As(err, &missing) {
		return fmt.Errorf("release %s of %s has no build for %s/%s; its build for this platform may have failed. "+
			"Nothing was changed; try again once it is attached, or `--to` an earlier release",
			missing.Tag, repo, missing.GOOS, missing.GOARCH)
	}
	if err != nil {
		return err
	}

	c := a.client()
	daemon := c.Available()
	var before string
	running := false
	if daemon {
		st, err := c.Status(name)
		if err != nil {
			return err
		}
		before = st.InstanceID
		// Halted or backing off counts: a crash loop is a reason to update,
		// and the restart below clears the halt.
		running = st.Desired != state.DesiredStopped
	}

	// A new release may migrate its data on first start, and the backup is
	// the only way back from one that does so badly.
	switch {
	case *noBackup:
	case !daemon:
		fmt.Fprintln(a.err, "outpost: the daemon is not running, so no backup was taken")
	default:
		archives, _, err := c.Backup(name)
		if err != nil {
			return fmt.Errorf("back up %s before updating: %w (use --no-backup to update without one)", name, err)
		}
		for _, ar := range archives {
			fmt.Fprintf(a.out, "backed up %s to %s\n", name, ar.Path)
		}
	}

	// A hard link keeps the running binary's file as it is, under a second
	// name, at no cost; Install then renames the new one over the first name.
	previous := svc.Binary + ".previous"
	_ = os.Remove(previous) // only the latest previous is kept
	if err := os.Link(svc.Binary, previous); err != nil {
		return fmt.Errorf("keep %s to roll back to: %w", svc.Binary, err)
	}

	fmt.Fprintf(a.err, "downloading %s from release %s…\n", asset.Name, rel.Tag)
	res, err := client.Install(ctx, rel, asset, svc.Binary)
	if err != nil {
		_ = os.Remove(previous) // the binary was never replaced
		return err
	}
	if res.Warning != "" {
		fmt.Fprintf(a.err, "outpost: %s\n", res.Warning)
	}
	verified := "unverified"
	if res.Verified {
		verified = "checksum verified"
	}
	fmt.Fprintf(a.out, "installed %s %s (%s)\n", repo, rel.Tag, verified)

	if err := replaceFile(path, setRelease(original, repo.String(), rel.Tag)); err != nil {
		return a.rollback(c, svc, path, original, previous, "", fmt.Errorf("record the release in %s: %w", path, err))
	}

	switch {
	case !daemon:
		fmt.Fprintf(a.out, "\n%s runs %s when the daemon next starts it.\n", name, rel.Tag)
		return nil
	case !running:
		if _, err := c.Apply(); err != nil {
			return err
		}
		fmt.Fprintf(a.out, "\n%s is not running; it runs %s when started.\n", name, rel.Tag)
		return nil
	}

	if _, err := c.Apply(); err != nil {
		return a.rollback(c, svc, path, original, previous, "", err)
	}
	if _, err := c.Restart(name); err != nil {
		return a.rollback(c, svc, path, original, previous, "", err)
	}
	timeout := *wait
	if timeout == 0 {
		timeout = 30*time.Second + time.Duration(svc.StartGraceSeconds)*time.Second
	}
	fmt.Fprintf(a.err, "waiting for %s to pass its healthcheck…\n", name)
	id, err := waitHealthy(c, svc, before, timeout)
	if err != nil {
		return a.rollback(c, svc, path, original, previous, id,
			fmt.Errorf("%s %s did not come up healthy: %w", name, rel.Tag, err))
	}
	fmt.Fprintf(a.out, "%s is running %s and healthy (was %s)\n", name, rel.Tag, installed)
	fmt.Fprintf(a.out, "The previous binary is kept at %s.\n", previous)
	return nil
}

// rollback puts the previous binary and manifest back after a failed update,
// restarts the service on them, and returns why the update failed.
func (a *app) rollback(c *ipc.Client, svc *manifest.Service, path string, original []byte, previous, failedID string, cause error) error {
	var problems []string
	if err := os.Rename(previous, svc.Binary); err != nil {
		problems = append(problems, fmt.Sprintf("restore %s: %v", svc.Binary, err))
	}
	if err := replaceFile(path, original); err != nil {
		problems = append(problems, fmt.Sprintf("restore %s: %v", path, err))
	}
	if len(problems) > 0 {
		return fmt.Errorf("%w; rolling back also failed: %s", cause, strings.Join(problems, "; "))
	}
	if !c.Available() {
		return fmt.Errorf("%w; rolled back to the previous binary", cause)
	}
	if _, err := c.Apply(); err != nil {
		return fmt.Errorf("%w; rolled back the files, but the daemon would not reload them: %v", cause, err)
	}
	if _, err := c.Restart(svc.Name); err != nil {
		return fmt.Errorf("%w; rolled back the files, but restarting failed: %v", cause, err)
	}
	if _, err := waitHealthy(c, svc, failedID, 30*time.Second+time.Duration(svc.StartGraceSeconds)*time.Second); err != nil {
		return fmt.Errorf("%w; rolled back to %s, which is not healthy either: %v", cause, orDash(svc.Release), err)
	}
	return fmt.Errorf("%w; rolled back to %s, which is running and healthy", cause, orDash(svc.Release))
}

// waitHealthy waits for an instance other than before to pass the service's
// healthcheck twice in a row, the same probe the daemon makes. It returns the
// newest instance it saw, healthy or not, so a rollback can tell its own
// restart from the failed one.
func waitHealthy(c *ipc.Client, svc *manifest.Service, before string, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", svc.Port, svc.Healthcheck)
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var id string
	passes := 0
	last := errors.New("it did not start")
	for time.Now().Before(deadline) {
		time.Sleep(updatePoll)
		st, err := c.Status(svc.Name)
		if err != nil {
			last = err
			continue
		}
		if st.InstanceID == "" || st.InstanceID == before {
			continue
		}
		if id != "" && st.InstanceID != id {
			// Passing once and then dying is not healthy.
			return st.InstanceID, fmt.Errorf("it exited and was restarted")
		}
		id = st.InstanceID

		resp, err := probe.Get(url)
		if err != nil {
			passes, last = 0, err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		_ = resp.Body.Close() // read for the status alone
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			passes, last = 0, fmt.Errorf("healthcheck %s returned %s", url, resp.Status)
			continue
		}
		if passes++; passes == 2 {
			return id, nil
		}
	}
	return id, fmt.Errorf("not healthy after %s: %v", timeout, last)
}

// checkAll reports, for every service installed from a release, whether a
// newer one exists.
func (a *app) checkAll() error {
	services, err := manifest.LoadDir(a.servicesDir())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client := acquireClient()

	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSOURCE\tINSTALLED\tLATEST")
	for _, svc := range services {
		if svc.Source == "" {
			fmt.Fprintf(tw, "%s\t-\t-\tnot installed from a release\n", svc.Name)
			continue
		}
		latest := ""
		repo, err := acquire.ParseRepo(svc.Source)
		if err == nil {
			var rel *acquire.Release
			if rel, err = client.LatestRelease(ctx, repo); err == nil {
				latest = rel.Tag
				if rel.Tag == svc.Release {
					latest += " (up to date)"
				}
			}
		}
		if err != nil {
			latest = "error: " + err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", svc.Name, svc.Source, orDash(svc.Release), latest)
	}
	return tw.Flush()
}

// ownsBinary reports whether path is inside the directory `outpost add`
// installs into.
func (a *app) ownsBinary(path string) bool {
	bin, err := filepath.Abs(filepath.Join(a.home, "bin"))
	if err != nil {
		return false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(bin, abs)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

// topLevelKey matches a key at the top of a manifest, before any [table].
var topLevelKey = regexp.MustCompile(`^\s*(source|release)\s*=`)

// setRelease records source and release in a manifest, changing only those
// lines so the operator's own edits and comments survive. A missing key goes
// in before the first [table], where it is still a top-level key.
func setRelease(body []byte, source, release string) []byte {
	want := map[string]string{"source": source, "release": release}
	lines := strings.SplitAfter(string(body), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // the newline ending the file, not a line
	}
	end := len(lines)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			end = i
			break
		}
	}
	for i := 0; i < end; i++ {
		if m := topLevelKey.FindStringSubmatch(lines[i]); m != nil {
			if v, ok := want[m[1]]; ok {
				lines[i] = fmt.Sprintf("%-14s = %s\n", m[1], tomlString(v))
				delete(want, m[1])
			}
		}
	}
	var add []string
	for _, k := range []string{"source", "release"} {
		if v, ok := want[k]; ok {
			add = append(add, fmt.Sprintf("%-14s = %s\n", k, tomlString(v)))
		}
	}
	if len(add) > 0 && end > 0 && !strings.HasSuffix(lines[end-1], "\n") {
		lines[end-1] += "\n"
	}
	out := append(append(append([]string{}, lines[:end]...), add...), lines[end:]...)
	return []byte(strings.Join(out, ""))
}

// replaceFile writes body to path by way of a rename, so a crash cannot leave
// a half-written manifest for the daemon to load.
func replaceFile(path string, body []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".outpost-manifest-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp's 0600 is already what manifests are written with.
	return os.Rename(tmp.Name(), path)
}

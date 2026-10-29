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
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
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
	if *wait < 0 {
		return fmt.Errorf("update: --wait must not be negative")
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
	if err != nil {
		return releaseError(err)
	}

	installed := orDash(svc.Release)
	// The latest release can be older than the installed one: GitHub's latest
	// skips prereleases, so after `--to v2.0.0-rc1` it names v1.9.0. Going
	// back takes asking for it by tag, since a new release may have migrated
	// data the old one can't read.
	newer := false
	if c, ok := compareReleases(svc.Release, rel.Tag); ok && c > 0 && *to == "" {
		newer = true
	}
	if *check {
		switch {
		case svc.Release == rel.Tag:
			fmt.Fprintf(a.out, "%s is up to date (%s from %s)\n", name, rel.Tag, repo)
			return nil
		case newer:
			fmt.Fprintf(a.out, "%s is at %s, newer than %s's latest release, %s\n", name, svc.Release, repo, rel.Tag)
			return nil
		}
		fmt.Fprintf(a.out, "%s: %s installed, %s available from %s\n", name, installed, rel.Tag, repo)
		hint := name
		if svc.Source == "" {
			hint += " --from " + repo.String() // not recorded until an update succeeds
		}
		fmt.Fprintf(a.out, "Install it with `outpost update %s`.\n", hint)
		return nil
	}
	if svc.Release == rel.Tag && !*force {
		fmt.Fprintf(a.out, "%s is already at %s (use --force to reinstall it)\n", name, rel.Tag)
		return nil
	}
	if newer {
		fmt.Fprintf(a.out, "%s is at %s, newer than %s's latest release, %s; nothing to update.\n"+
			"To go back to it, `outpost update %s --to %s`.\n", name, svc.Release, repo, rel.Tag, name, rel.Tag)
		return nil
	}
	if c, ok := compareReleases(svc.Release, rel.Tag); ok && c > 0 {
		fmt.Fprintf(a.err, "outpost: going back from %s to %s; if %s changed its data, %s may not read it\n",
			svc.Release, rel.Tag, svc.Release, rel.Tag)
	}

	// Check the manifest edit before anything changes, so one this can't make
	// cleanly stops the update rather than its rollback.
	updated := setRelease(original, repo.String(), rel.Tag)
	if err := checkRecorded(updated, repo.String(), rel.Tag); err != nil {
		return fmt.Errorf("%s: can't record the new release: %w; nothing was changed", path, err)
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

	// Two updates of one service at once would each keep the other's binary
	// as the one to roll back to.
	unlock, err := lockUpdate(svc.Binary)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer unlock()

	c := a.client()
	daemon := c.Available()
	running := false
	if daemon {
		st, err := c.Status(name)
		if err != nil {
			return err
		}
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

	back := func(restart bool, cause error) error {
		return a.rollback(c, svc, path, original, previous, restart, cause)
	}
	if err := replaceFile(path, updated); err != nil {
		return back(running, fmt.Errorf("record the release in %s: %w", path, err))
	}

	switch {
	case !daemon:
		fmt.Fprintf(a.out, "\n%s runs %s when the daemon next starts it.\n", name, rel.Tag)
		return nil
	case !running:
		if _, err := c.Apply(); err != nil {
			return back(false, err)
		}
		fmt.Fprintf(a.out, "\n%s is not running; it runs %s when started.\n", name, rel.Tag)
		return nil
	}

	if _, err := c.Apply(); err != nil {
		return back(true, err)
	}
	// The instance to wait past is the one running now: the backup above
	// stopped and restarted the service, so any earlier one is long gone.
	before, err := c.Status(name)
	if err != nil {
		return back(true, err)
	}
	if _, err := c.Restart(name); err != nil {
		return back(true, err)
	}
	timeout := *wait
	if timeout == 0 {
		timeout = 30*time.Second + time.Duration(svc.StartGraceSeconds)*time.Second
	}
	fmt.Fprintf(a.err, "waiting for %s to pass its healthcheck…\n", name)
	if _, err := waitHealthy(c, svc, before.InstanceID, timeout); err != nil {
		return back(true, fmt.Errorf("%s %s did not come up healthy: %w", name, rel.Tag, err))
	}
	fmt.Fprintf(a.out, "%s is running %s and healthy (was %s)\n", name, rel.Tag, installed)
	fmt.Fprintf(a.out, "The previous binary is kept at %s.\n", previous)
	return nil
}

// releaseError rewords a release with nothing attached: for an update that
// usually means its build is still uploading, not that the project publishes
// no binaries.
func releaseError(err error) error {
	var none *acquire.NoAssetsError
	if errors.As(err, &none) {
		return fmt.Errorf("release %s of %s has nothing attached to install yet; if it was just published, "+
			"its build may still be uploading. Try again later, or `--to` an earlier release", none.Tag, none.Repo)
	}
	return err
}

// rollback puts the previous binary and manifest back after a failed update,
// restarts the service on them if it was running, and returns why the update
// failed.
func (a *app) rollback(c *ipc.Client, svc *manifest.Service, path string, original []byte, previous string, restart bool, cause error) error {
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
	if !restart {
		return fmt.Errorf("%w; rolled back to %s", cause, orDash(svc.Release))
	}
	// Whatever is running now may be the failed release, so only an instance
	// started after this point is the rolled-back one.
	st, err := c.Status(svc.Name)
	if err != nil {
		return fmt.Errorf("%w; rolled back the files, but the daemon did not answer: %v", cause, err)
	}
	if _, err := c.Restart(svc.Name); err != nil {
		return fmt.Errorf("%w; rolled back the files, but restarting failed: %v", cause, err)
	}
	if _, err := waitHealthy(c, svc, st.InstanceID, 30*time.Second+time.Duration(svc.StartGraceSeconds)*time.Second); err != nil {
		return fmt.Errorf("%w; rolled back to %s, which is not healthy either: %v", cause, orDash(svc.Release), err)
	}
	return fmt.Errorf("%w; rolled back to %s, which is running and healthy", cause, orDash(svc.Release))
}

// maxUpdateRestarts is how many times a new release may be restarted while
// waitHealthy watches before it counts as crash-looping. One is allowed: the
// daemon can restart a slow starter on its own.
const maxUpdateRestarts = 2

// waitHealthy waits for an instance other than before to pass the service's
// healthcheck twice in a row, the same probe the daemon makes. It returns the
// newest instance it saw, healthy or not.
func waitHealthy(c *ipc.Client, svc *manifest.Service, before string, timeout time.Duration) (string, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", svc.Port, svc.Healthcheck)
	probe := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var id string
	passes, restarts := 0, 0
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
			// Both passes have to come from one instance: passing once and
			// then dying is not healthy.
			if restarts++; restarts >= maxUpdateRestarts {
				return st.InstanceID, fmt.Errorf("it kept exiting and was restarted %d times", restarts)
			}
			passes = 0
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

	// One slow lookup shouldn't use up the others' time, so they run a few
	// at a time and print in manifest order.
	latest := make([]string, len(services))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, svc := range services {
		if svc.Source == "" {
			latest[i] = "not installed from a release"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			latest[i] = latestColumn(ctx, client, svc)
		}()
	}
	wg.Wait()

	tw := tabwriter.NewWriter(a.out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tSOURCE\tINSTALLED\tLATEST")
	for i, svc := range services {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", svc.Name, orDash(svc.Source), orDash(svc.Release), latest[i])
	}
	return tw.Flush()
}

// latestColumn is what checkAll says about one service's latest release.
func latestColumn(ctx context.Context, client *acquire.Client, svc *manifest.Service) string {
	repo, err := acquire.ParseRepo(svc.Source)
	if err != nil {
		return "error: " + err.Error()
	}
	rel, err := client.LatestRelease(ctx, repo)
	var none *acquire.NoAssetsError
	switch {
	case errors.As(err, &none):
		return none.Tag + " (nothing attached yet; its build may still be uploading)"
	case err != nil:
		return "error: " + err.Error()
	case rel.Tag == svc.Release:
		return rel.Tag + " (up to date)"
	}
	if c, ok := compareReleases(svc.Release, rel.Tag); ok && c > 0 {
		return rel.Tag + " (older than installed)"
	}
	return rel.Tag
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

// topLevelKey matches a source or release key, bare or quoted, and captures
// what follows the =.
var topLevelKey = regexp.MustCompile(`^\s*(?:(source|release)|"(source|release)"|'(source|release)')\s*=(.*)$`)

// trailingComment returns the comment after a string value, such as
// `  # pinned` in `"v1"  # pinned`, or "" if there is none.
func trailingComment(value string) string {
	v := strings.TrimSpace(value)
	end := -1
	switch {
	case strings.HasPrefix(v, `"`):
		for i := 1; i < len(v); i++ {
			if v[i] == '\\' {
				i++
			} else if v[i] == '"' {
				end = i
				break
			}
		}
	case strings.HasPrefix(v, "'"):
		if i := strings.IndexByte(v[1:], '\''); i >= 0 {
			end = i + 1
		}
	}
	if end < 0 {
		return ""
	}
	if rest := strings.TrimSpace(v[end+1:]); strings.HasPrefix(rest, "#") {
		return "  " + rest
	}
	return ""
}

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
		if m := topLevelKey.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n")); m != nil {
			key := m[1] + m[2] + m[3] // only one of them matched
			if v, ok := want[key]; ok {
				lines[i] = fmt.Sprintf("%-14s = %s%s\n", key, tomlString(v), trailingComment(m[4]))
				delete(want, key)
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

// checkRecorded reports whether a rewritten manifest still parses and records
// the given source and release, for a manifest setRelease can't edit cleanly,
// such as one setting release in a multi-line string.
func checkRecorded(body []byte, source, release string) error {
	var got struct {
		Source  string `toml:"source"`
		Release string `toml:"release"`
	}
	if _, err := toml.Decode(string(body), &got); err != nil {
		return err
	}
	if got.Source != source || got.Release != release {
		return fmt.Errorf("the edit would record source %q and release %q", got.Source, got.Release)
	}
	return nil
}

// lockUpdate takes a lock beside a service's binary for the length of an
// update. The lock is the kernel's, so a crashed update leaves none behind.
func lockUpdate(binary string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(filepath.Dir(binary), "."+filepath.Base(binary)+".update-lock"),
		os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close() // the lock is the error that matters
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errors.New("another update of it is already running")
		}
		return nil, fmt.Errorf("lock for update: %w", err)
	}
	return func() { _ = f.Close() }, nil // closing releases the lock
}

// replaceFile writes body to path by way of a rename, so a crash cannot leave
// a half-written manifest for the daemon to load. A symlinked manifest has its
// target rewritten, so the link stays a link, and the file keeps its mode.
func replaceFile(path string, body []byte) error {
	if target, err := filepath.EvalSymlinks(path); err == nil {
		path = target
	}
	mode := os.FileMode(0o600) // what manifests are written with
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".outpost-manifest-*")
	if err != nil {
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close() // the chmod error is the one worth reporting
		_ = os.Remove(tmp.Name())
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
	return os.Rename(tmp.Name(), path)
}

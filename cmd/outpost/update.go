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
	all := fs.Bool("all", false, "update every service with a newer release, one at a time, stopping at the first that fails")
	to := fs.String("to", "", "install this release tag instead of the latest (also how to go back)")
	from := fs.String("from", "", "the GitHub repository a service came from, for one whose manifest does not say")
	force := fs.Bool("force", false, "reinstall even when the release is the one already installed (with --self, also install over a development build)")
	noBackup := fs.Bool("no-backup", false, "skip the backup of the service's storage taken before updating")
	wait := fs.Duration("wait", 0, "how long the new release has to pass its healthcheck (default 30s plus start_grace_seconds)")
	allowUnverified := fs.Bool("allow-unverified", false, "install a release with no checksum to verify it against, even though the installed one was verified")
	self := fs.Bool("self", false, "update Outpost itself, restarting the daemon and every service with it")
	label := fs.String("label", "", "with --self: the label of the daemon's autostart job, if `outpost install` was given one")
	positional, err := parseOptionalService(fs, "update", args)
	if err != nil {
		return err
	}
	if *wait < 0 {
		return fmt.Errorf("update: --wait must not be negative")
	}
	if *label != "" && !*self {
		return fmt.Errorf("update: --label names the daemon's job, so it only goes with --self")
	}
	if *self {
		switch {
		case len(positional) > 0:
			return fmt.Errorf("update: --self updates Outpost, so it can't be given a service name (%q)", positional[0])
		case *all || *from != "" || *noBackup:
			return fmt.Errorf("update: --all, --from and --no-backup are about services, and can't be used with --self")
		}
		if *wait == 0 {
			*wait = defaultRestartWait
		}
		return a.updateSelf(*check, *force, *allowUnverified, *to, *label, *wait)
	}
	if *all {
		switch {
		case len(positional) > 0:
			return fmt.Errorf("update: --all means every service, so it can't be given a service name (%q)", positional[0])
		case *to != "" || *from != "" || *force || *allowUnverified:
			return fmt.Errorf("update: --to, --from, --force and --allow-unverified are about one service, and can't be used with --all")
		case *check:
			return a.checkAll()
		}
		return a.updateAll(*noBackup, *wait)
	}
	switch {
	case len(positional) > 1:
		return fmt.Errorf("update: unexpected argument %q", positional[1])
	case len(positional) == 0 && *check:
		return a.checkAll()
	case len(positional) == 0:
		return fmt.Errorf("update: a service name is required (or `outpost update --check` to see every service, `--all` to update them)")
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
	order, ordered := compareReleases(svc.Release, rel.Tag)
	newer := ordered && order > 0 && *to == ""
	if *check {
		switch {
		case sameRelease(svc.Release, rel.Tag):
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
		if *to != "" {
			hint += " --to " + *to // the latest may be a different release
		}
		fmt.Fprintf(a.out, "Install it with `outpost update %s`.\n", hint)
		return nil
	}
	if sameRelease(svc.Release, rel.Tag) && !*force {
		fmt.Fprintf(a.out, "%s is already at %s (use --force to reinstall it)\n", name, rel.Tag)
		return nil
	}
	if newer {
		fmt.Fprintf(a.out, "%s is at %s, newer than %s's latest release, %s; nothing to update.\n"+
			"To go back to it, `outpost update %s --to %s`.\n", name, svc.Release, repo, rel.Tag, name, rel.Tag)
		return nil
	}
	if ordered && order > 0 {
		fmt.Fprintf(a.err, "outpost: going back from %s to %s; if %s changed its data, %s may not read it\n",
			svc.Release, rel.Tag, svc.Release, rel.Tag)
	}

	// Check the manifest edit before anything changes, so one this can't make
	// cleanly stops the update rather than its rollback.
	updated := setRelease(original, repo.String(), rel.Tag)
	if err := checkRecorded(updated, repo.String(), rel.Tag); err != nil {
		return fmt.Errorf("%s: can't record the new release: %w; nothing was changed", path, err)
	}
	// Whether the download verifies isn't known yet, so both answers have to
	// be recordable.
	for _, v := range []bool{true, false} {
		if err := checkVerified(setVerified(updated, v), v); err != nil {
			return fmt.Errorf("%s: can't record whether the new release is verified: %w; nothing was changed", path, err)
		}
	}

	// Replacing a binary Outpost did not put there could mean overwriting
	// something the system or another tool owns.
	if !a.ownsBinary(svc.Binary) {
		return fmt.Errorf("%s runs %s, which is outside %s; update only replaces binaries `outpost add` installed",
			name, svc.Binary, filepath.Join(a.home, "bin"))
	}

	_, statErr := os.Stat(svc.Binary)
	noBinary := os.IsNotExist(statErr)
	if noBinary && !*force {
		return fmt.Errorf("%s is missing, so there is nothing to roll back to if %s fails; "+
			"`outpost update %s --force` installs it anyway", svc.Binary, rel.Tag, name)
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
	// A release that drops its checksums would otherwise replace a verified
	// binary with nothing more than a warning, which is the same thing a
	// tampered release would look like.
	refuseUnverified := func(err error) error {
		return fmt.Errorf("%s: %w, and the installed %s was checksum verified; nothing was changed. "+
			"If that is expected, `outpost update %s --allow-unverified` installs it anyway", name, err, installed, name)
	}
	client.RequireChecksum = svc.Verified && !*allowUnverified
	if client.RequireChecksum {
		if err := client.CheckVerifiable(ctx, rel, asset.Name); err != nil {
			var unverified *acquire.UnverifiedError
			if errors.As(err, &unverified) {
				return refuseUnverified(err)
			}
			return err
		}
	}

	// Two updates of one service at once would each keep the other's binary
	// as the one to roll back to.
	unlock, err := lockUpdate(svc.Binary)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer unlock()
	// update --self holds the locks it finds, but a service never updated has
	// none to find, so its restart would cut this update short with no
	// rollback. It takes its own lock before looking for this one, and this
	// looks for its lock after taking this one, so whichever is second sees
	// the first.
	switch held, err := selfUpdateHeld(a.home); {
	case err != nil:
		return fmt.Errorf("%s: %w", name, err)
	case held:
		return fmt.Errorf("%s: Outpost itself is being updated, and its restart would cut this update short; "+
			"nothing was changed. Try again once it's done", name)
	}
	// Everything above was read before the lock, so an update that finished
	// in between would leave this one rolling back to a manifest that no
	// longer matches the binary on disk.
	if current, err := os.ReadFile(path); err != nil || string(current) != string(original) {
		return fmt.Errorf("%s changed while this update was starting; nothing was changed, run it again", path)
	}

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

	// With no binary there is nothing to roll back to, and --force has said
	// to install anyway: it is how a deleted binary gets put back.
	previous := ""
	if !noBinary {
		previous = svc.Binary + ".previous"
		if err := keepPrevious(svc.Binary, previous); err != nil {
			return fmt.Errorf("keep %s to roll back to: %w", svc.Binary, err)
		}
	}

	fmt.Fprintf(a.err, "downloading %s from release %s…\n", asset.Name, rel.Tag)
	res, err := client.Install(ctx, rel, asset, svc.Binary)
	if err != nil {
		if previous != "" {
			_ = os.Remove(previous) // the binary was never replaced
		}
		// The release changed since the check above.
		var unverified *acquire.UnverifiedError
		if errors.As(err, &unverified) {
			return refuseUnverified(err)
		}
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
	updated = setVerified(updated, res.Verified)

	timeout := *wait
	if timeout == 0 {
		timeout = 30*time.Second + time.Duration(svc.StartGraceSeconds)*time.Second
	}
	back := func(restart bool, cause error) error {
		return a.rollback(c, svc, path, original, previous, restart, timeout, cause)
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
	fmt.Fprintf(a.err, "waiting for %s to pass its healthcheck…\n", name)
	if _, err := waitHealthy(c, svc, before.InstanceID, timeout); err != nil {
		return back(true, fmt.Errorf("%s %s did not come up healthy: %w", name, rel.Tag, err))
	}
	fmt.Fprintf(a.out, "%s is running %s and healthy (was %s)\n", name, rel.Tag, installed)
	if previous != "" {
		fmt.Fprintf(a.out, "The previous binary is kept at %s.\n", previous)
	}
	return nil
}

// sameRelease reports whether two tags name the same release: identical, or
// differing only in a leading v, as 1.2.0 and v1.2.0 do. Build metadata still
// counts, since two builds of one version are not the same binary.
func sameRelease(a, b string) bool {
	trim := func(s string) string { return strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V") }
	return a != "" && trim(a) == trim(b)
}

// linkFile is os.Link, a variable so tests can make links fail.
var linkFile = os.Link

// keepPrevious keeps the binary under a second name to roll back to. A hard
// link does it at no cost, and Install then renames the new binary over the
// first name; where links aren't possible, a copy does the same.
func keepPrevious(binary, previous string) error {
	_ = os.Remove(previous) // only the latest previous is kept
	if err := linkFile(binary, previous); err == nil {
		return nil
	}
	src, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }() // read-only; nothing to lose
	fi, err := src.Stat()
	if err != nil {
		return err
	}
	dst, err := os.OpenFile(previous, os.O_CREATE|os.O_EXCL|os.O_WRONLY, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close() // the copy error is the one worth reporting
		_ = os.Remove(previous)
		return err
	}
	if err := dst.Close(); err != nil {
		_ = os.Remove(previous) // a short copy is no use to roll back to
		return err
	}
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
func (a *app) rollback(c *ipc.Client, svc *manifest.Service, path string, original []byte, previous string, restart bool, timeout time.Duration, cause error) error {
	if previous == "" {
		// The manifest stays as it is too, so it names what is on disk.
		return fmt.Errorf("%w; there was no previous binary to roll back to, so the new one is left in place", cause)
	}
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
	// The same allowance as the new release had: --wait is usually given for a
	// service slow to start, and the old release is no quicker.
	if _, err := waitHealthy(c, svc, st.InstanceID, timeout); err != nil {
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
	case sameRelease(svc.Release, rel.Tag):
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

// verifiedKey matches a top-level verified key, bare or quoted, and captures
// what follows the =.
var verifiedKey = regexp.MustCompile(`^\s*(?:verified|"verified"|'verified')\s*=(.*)$`)

// setVerified records whether the installed release was verified, changing
// only that line as setRelease does. An absent key reads as false, so false is
// only written over a line that already says otherwise.
func setVerified(body []byte, verified bool) []byte {
	line := fmt.Sprintf("%-14s = %t\n", "verified", verified)
	lines := strings.SplitAfter(string(body), "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	end := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "[") {
			end = i
			break
		}
	}
	for i := 0; i < end; i++ {
		if m := verifiedKey.FindStringSubmatch(strings.TrimRight(lines[i], "\r\n")); m != nil {
			// A boolean can't contain a #, so the first one starts a comment.
			lines[i] = line
			if c := strings.IndexByte(m[1], '#'); c >= 0 {
				lines[i] = fmt.Sprintf("%-14s = %t  %s\n", "verified", verified, m[1][c:])
			}
			return []byte(strings.Join(lines, ""))
		}
	}
	if !verified {
		return body
	}
	if end > 0 && !strings.HasSuffix(lines[end-1], "\n") {
		lines[end-1] += "\n"
	}
	out := append(append(append([]string{}, lines[:end]...), line), lines[end:]...)
	return []byte(strings.Join(out, ""))
}

// checkVerified reports whether a rewritten manifest still parses and records
// the given verified, as checkRecorded does for source and release.
func checkVerified(body []byte, verified bool) error {
	var got struct {
		Verified bool `toml:"verified"`
	}
	if _, err := toml.Decode(string(body), &got); err != nil {
		return err
	}
	if got.Verified != verified {
		return fmt.Errorf("the edit would record verified = %t", got.Verified)
	}
	return nil
}

// lockUpdate takes a lock beside a service's binary for the length of an
// update. The lock is the kernel's, so a crashed update leaves none behind.
func lockUpdate(binary string) (func(), error) {
	return lockFile(filepath.Join(filepath.Dir(binary), "."+filepath.Base(binary)+".update-lock"))
}

// lockFile takes the kernel's lock on path, creating it if need be.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
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

// selfUpdateHeld reports whether an update --self of this home is running. The
// lock is only tried shared, so two services' updates asking at the same moment
// don't read each other as one.
func selfUpdateHeld(home string) (bool, error) {
	f, err := os.OpenFile(selfUpdateLock(home), os.O_RDWR, 0)
	if os.IsNotExist(err) {
		return false, nil // no update --self has ever run here
	}
	if err != nil {
		return false, fmt.Errorf("check for an update of Outpost: %w", err)
	}
	defer func() { _ = f.Close() }() // closing releases the lock; nothing was written
	switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); {
	case err == nil:
		return false, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	default:
		return false, fmt.Errorf("check for an update of Outpost: %w", err)
	}
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
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close() // the write error is the one worth reporting
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

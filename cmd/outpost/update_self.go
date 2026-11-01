package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/ipc"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/version"
)

// selfRepo is where Outpost's own releases are published, each with a bare
// binary per platform and a SHA256SUMS.
var selfRepo = acquire.Repo{Owner: "sroberts", Name: "outpost"}

// Seams for `update --self`, so tests can be a released build, run from a
// binary of their own, and name a job that can't be the real one.
var (
	selfExecutable = os.Executable
	selfBuild      = version.Get
	selfLabel      = autostart.DefaultLabel
)

// probeBinaryVersion asks a binary which release it is, by running it. It is
// how a download that won't run here, built for another platform or
// corrupted, is caught before anything has stopped. --version is answered
// before the config is read, so a home with a problem can't make it fail.
var probeBinaryVersion = func(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return "", err
	}
	return parseVersionOutput(out)
}

// parseVersionOutput reads what `outpost --version` prints: "outpost <version>"
// and then indented details, one of which marks a build from a modified tree.
func parseVersionOutput(out []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(out))
	if !sc.Scan() {
		return "", errors.New("it printed nothing")
	}
	first := strings.Fields(sc.Text())
	if len(first) != 2 || first[0] != "outpost" {
		return "", fmt.Errorf("it printed %q, not a version", sc.Text())
	}
	v := first[1]
	for sc.Scan() {
		if strings.Contains(sc.Text(), "uncommitted changes") {
			v += " (dirty)"
		}
	}
	return v, nil
}

// gitDescribe and pseudoVersion match the two ways a build that is not a
// release names itself: `make build` between tags stamps git describe's
// v0.1.0-3-gabc1234, and `go install` of a commit stamps a Go pseudo-version.
// Both read as prereleases of a real version, which would make every release
// look newer than they are, or older.
var (
	gitDescribe   = regexp.MustCompile(`-\d+-g[0-9a-f]+$`)
	pseudoVersion = regexp.MustCompile(`\d{14}-[0-9a-f]{12}$`)
)

// buildRelease reads the release tag out of a build identity such as
// "v0.1.0 (abc123def456)". clean is false for anything that is not exactly a
// release: a modified tree, "dev", a commit between tags, or a pseudo-version.
//
// It would be releaseOf, but update_all_test.go already has a helper by that
// name, and tests are not to be changed.
func buildRelease(s string) (tag string, clean bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return "", false
	}
	tag = fields[0]
	core, _, _ := strings.Cut(tag, "+")
	switch {
	case strings.Contains(s, "dirty"), tag == "dev", gitDescribe.MatchString(core), pseudoVersion.MatchString(core):
		return tag, false
	}
	if _, ok := parseVersion(tag); !ok {
		return tag, false // a bare commit, from git describe --always with no tags
	}
	return tag, true
}

// packageManagerDirs are where a package manager keeps the binaries it owns.
// Replacing one leaves the manager believing it installed something else, and
// its next upgrade puts its own back.
var packageManagerDirs = []struct {
	dir      string
	anywhere bool // Homebrew's Cellar can sit under any prefix
}{
	{"/Cellar/", true},
	{"/opt/homebrew/", false},
	{"/home/linuxbrew/", false},
	{"/nix/store/", false},
	{"/usr/bin/", false},
	{"/bin/", false},
}

// refuseReplace says why the binary at path isn't this command's to replace,
// or nil if it is.
func refuseReplace(path string) error {
	for _, p := range packageManagerDirs {
		if (p.anywhere && strings.Contains(path, p.dir)) || strings.HasPrefix(path, p.dir) {
			return fmt.Errorf("%s was installed by a package manager (it is under %s); "+
				"update Outpost through that package manager instead", path, strings.TrimSuffix(p.dir, "/"))
		}
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("%s belongs to another user (uid %d), not to you (uid %d); "+
			"update it as the user who installed it", path, st.Uid, os.Geteuid())
	}
	dir := filepath.Dir(path)
	if err := syscall.Access(dir, 2); err != nil { // 2 is W_OK, which syscall doesn't name on every platform
		return fmt.Errorf("%s can't be written to (%v), so %s can't be replaced; "+
			"update it as the user who installed it", dir, err, path)
	}
	return nil
}

// selfJob finds the job that runs this home's daemon and the binary it runs,
// which is the one to replace: replacing any other would update nothing the
// daemon runs.
func (a *app) selfJob(label string) (daemonJob, string, error) {
	if label == "" {
		label = selfLabel
	}
	job, err := a.loadedDaemonJob(label, false)
	if err != nil {
		return daemonJob{}, "", fmt.Errorf("update --self restarts the daemon through its autostart job, "+
			"and nothing else could start the new one: %w", err)
	}
	body, err := os.ReadFile(job.unit)
	if err != nil {
		return daemonJob{}, "", err
	}
	binary, err := job.mgr.UnitBinary(string(body))
	if err != nil {
		return daemonJob{}, "", fmt.Errorf("can't tell which binary %s runs: %v", job.unit, err)
	}
	if resolved, err := filepath.EvalSymlinks(binary); err == nil {
		binary = resolved
	}
	exe, err := selfExecutable()
	if err != nil {
		return daemonJob{}, "", fmt.Errorf("locate the outpost binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	// The release compared against is this binary's, so it has to be the one
	// that gets replaced.
	if !samePath(binary, exe) {
		return daemonJob{}, "", fmt.Errorf("the daemon runs %s, not this binary (%s); "+
			"run `%s update --self` to update the one it runs", binary, exe, binary)
	}
	return job, binary, nil
}

// updateSelf replaces the binary the daemon runs with another release of
// Outpost, restarts the daemon on it, and puts the old binary back if the new
// daemon doesn't come up as that release.
func (a *app) updateSelf(check, force, allowUnverified bool, to, label string, wait time.Duration) error {
	return a.updateSelfWith(selfOpts{check: check, force: force, allowUnverified: allowUnverified, to: to, label: label, wait: wait})
}

// selfOpts are updateSelf's flags, and the two things the gateway's helper
// does differently from the CLI: how the daemon is restarted, and telling
// someone watching a page how far it has got.
type selfOpts struct {
	check, force, allowUnverified bool
	to, label                     string
	wait                          time.Duration
	// restart restarts the daemon on whatever binary is now in place, for
	// the update and for a rollback alike. nil is the job's own restart
	// commands, as the CLI runs them.
	restart func(job daemonJob) error
	// selfStarts says restart only stops the daemon and leaves starting the
	// next one to the init system, which does so by itself after a crash
	// too. A rollback after a crash loop then has nothing to stop.
	selfStarts bool
	// progress is told each phase as it starts, with the release being
	// installed once that is known. nil tells no one.
	progress func(phase, tag string)
}

// Phases of an update --self, as progress is told them and the status file
// records them.
const (
	selfStarting    = "starting"
	selfDownloading = "downloading"
	selfRestarting  = "restarting"
	selfWaiting     = "waiting"
	selfRollingBack = "rolling back"
	selfDone        = "done"
	selfFailed      = "failed"
)

func (a *app) updateSelfWith(o selfOpts) error {
	check, force, allowUnverified, to, label, wait := o.check, o.force, o.allowUnverified, o.to, o.label, o.wait
	if o.restart == nil {
		o.restart = func(job daemonJob) error { return a.runAll(job.mgr.RestartCommands(job.cfg, effectiveUID())) }
	}
	if o.progress == nil {
		o.progress = func(string, string) {}
	}
	build := selfBuild()
	installed, clean := buildRelease(build.String())
	// A development build has no place in the order of releases, so neither
	// "newer" nor "already at" can be said of it.
	if !clean && !check && to == "" && !force {
		return fmt.Errorf("this outpost is a development build (%s), not a release, so there is no telling "+
			"whether the latest release is newer; nothing was changed. "+
			"`outpost update --self --to TAG` installs a particular release, and --force the latest", build)
	}

	var job daemonJob
	var binary string
	if !check {
		var err error
		if job, binary, err = a.selfJob(label); err != nil {
			return err
		}
		if err := refuseReplace(binary); err != nil {
			return err
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), selfUpdateTimeout)
	defer cancel()
	client := acquireClient()
	client.Logf = func(format string, args ...any) {
		fmt.Fprintf(a.err, "outpost: "+format+"\n", args...)
	}
	var rel *acquire.Release
	var err error
	if to != "" {
		rel, err = client.ReleaseByTag(ctx, selfRepo, to)
	} else {
		rel, err = client.LatestRelease(ctx, selfRepo)
	}
	if err != nil {
		return releaseError(err)
	}

	// The same rules as updating a service: GitHub's latest skips
	// prereleases, so it can be older than what is installed, and going back
	// takes asking for it by tag.
	order, ordered := compareReleases(installed, rel.Tag)
	ordered = ordered && clean
	same := clean && sameRelease(installed, rel.Tag)
	newer := ordered && order > 0 && to == ""
	if check {
		a.checkSelf(build, installed, clean, rel.Tag, to)
		return nil
	}
	if same && !force {
		fmt.Fprintf(a.out, "Outpost is already at %s (use --force to reinstall it)\n", rel.Tag)
		return nil
	}
	if newer {
		fmt.Fprintf(a.out, "Outpost is at %s, newer than its latest release, %s; nothing to update.\n"+
			"To go back to it, `outpost update --self --to %s`.\n", installed, rel.Tag, rel.Tag)
		return nil
	}
	if ordered && order > 0 {
		fmt.Fprintf(a.err, "outpost: going back from %s to %s; if %s changed its data, %s may not read it\n",
			installed, rel.Tag, installed, rel.Tag)
	}

	asset, err := client.SelectAsset(rel, &acquire.Descriptor{})
	var missing *acquire.NoPlatformAssetError
	if errors.As(err, &missing) {
		return fmt.Errorf("release %s of %s has no build for %s/%s; its build for this platform may have failed. "+
			"Nothing was changed; try again once it is attached, or `--to` an earlier release",
			missing.Tag, selfRepo, missing.GOOS, missing.GOARCH)
	}
	if err != nil {
		return err
	}
	// The binary every service runs under is held to a checksum unless the
	// operator says otherwise: a release without one looks the same as a
	// tampered one.
	refuseUnverified := func(err error) error {
		return fmt.Errorf("%w; nothing was changed. If that is expected, "+
			"`outpost update --self --allow-unverified` installs it anyway", err)
	}
	client.RequireChecksum = !allowUnverified
	if client.RequireChecksum {
		if err := client.CheckVerifiable(ctx, rel, asset.Name); err != nil {
			var unverified *acquire.UnverifiedError
			if errors.As(err, &unverified) {
				return refuseUnverified(err)
			}
			return err
		}
	}

	// The home's lock keeps a second update --self of this home out, and the
	// binary's keeps out another home's daemon running the same binary, which
	// would otherwise share the staged and .previous files. Both are held
	// through the rollback, so neither can start on a half-swapped binary.
	unlockSelf, err := lockFile(selfUpdateLock(a.home))
	if err != nil {
		return fmt.Errorf("update --self: %w", err)
	}
	defer unlockSelf()
	unlockBinary, err := lockUpdate(binary)
	if err != nil {
		return fmt.Errorf("update --self: %s: %w", binary, err)
	}
	defer unlockBinary()

	// Staged beside the binary, so the swap is a rename within one directory.
	staged := filepath.Join(filepath.Dir(binary), ".outpost-selfupdate-new")
	_ = os.Remove(staged) // left by an update that was killed; nothing else uses the name
	o.progress(selfDownloading, rel.Tag)
	fmt.Fprintf(a.err, "downloading %s from release %s…\n", asset.Name, rel.Tag)
	res, err := client.Install(ctx, rel, asset, staged)
	if err != nil {
		_ = os.Remove(staged) // the error is what matters, and the binary was never touched
		var unverified *acquire.UnverifiedError
		if errors.As(err, &unverified) {
			return refuseUnverified(err)
		}
		return err
	}
	// From here until the swap, a refusal only has the download to clear up.
	refuse := func(err error) error {
		_ = os.Remove(staged) // refused; the binary in use was never touched
		return err
	}
	if res.Warning != "" {
		fmt.Fprintf(a.err, "outpost: %s\n", res.Warning)
	}
	got, err := probeBinaryVersion(staged)
	if err == nil {
		// A build from a modified tree under the right tag is not that
		// release, however it names itself.
		if tag, ok := buildRelease(got); !ok || !sameRelease(tag, rel.Tag) {
			err = fmt.Errorf("it says it is %s", got)
		}
	}
	if err != nil {
		return refuse(fmt.Errorf("the downloaded %s doesn't run here as %s (%v); nothing was changed", asset.Name, rel.Tag, err))
	}
	verified := "unverified"
	if res.Verified {
		verified = "checksum verified"
	}
	fmt.Fprintf(a.out, "downloaded Outpost %s (%s)\n", rel.Tag, verified)

	snapshot, err := a.snapshotAuth(installed)
	if err != nil {
		return refuse(fmt.Errorf("copy %s before updating: %w; nothing was changed", a.authPath(), err))
	}

	// Taken as late as possible, so a long download doesn't hold up service
	// updates, and held until this returns: through the restart, the wait and
	// any rollback, every one of which a service update would be cut short
	// by. The two locks above are this update's own, and show up here as
	// held by someone else, since a second open of a file conflicts with the
	// first even within one process.
	updating, releaseLocks, err := holdUpdateLocks(a.home)
	if err != nil {
		return refuse(err)
	}
	defer releaseLocks()
	var services []string
	for _, name := range updating {
		own := name == selfUpdateName ||
			(name == filepath.Base(binary) && samePath(filepath.Dir(binary), filepath.Join(a.home, "bin")))
		if !own {
			services = append(services, name)
		}
	}
	// Neither of these is --force's to override: restarting the daemon would
	// end a service's update with no rollback, and a daemon that won't start
	// leaves every service down, both for the sake of an update that can wait.
	if len(services) > 0 {
		return refuse(fmt.Errorf("an update is running (%s), and restarting the daemon would end it where it stands, "+
			"with no rollback; nothing was changed. Wait for it to finish, and run this again", strings.Join(services, ", ")))
	}
	if _, err := a.validateManifests(); err != nil {
		return refuse(fmt.Errorf("the new daemon would refuse to start, leaving every service down: %w\n"+
			"Nothing was changed. Fix the manifest, and run this again", err))
	}

	previous := binary + ".previous"
	if err := keepPrevious(binary, previous); err != nil {
		return refuse(fmt.Errorf("keep %s to roll back to: %w; nothing was changed", binary, err))
	}

	old := a.currentDaemon()
	if old.Version != "" && old.Version != build.String() {
		fmt.Fprintf(a.out, "The daemon is running %s, not this binary's %s; both become %s.\n", old.Version, build, rel.Tag)
	}
	logs := job.mgr.LogHint(job.cfg)
	fmt.Fprintf(a.out, "Restarting the daemon on %s: %s job %s, %s scope.\n", rel.Tag, job.mgr.Name(), job.cfg.Label, job.cfg.Scope)
	fmt.Fprintln(a.out, "Every service stops with it, each with SIGTERM and SIGKILL after 10s. The new daemon starts them")
	fmt.Fprintln(a.out, "again, apart from any stopped by hand or halted after a crash loop, which stay that way.")

	// quiet is a restart after which nothing answered at all, the old daemon
	// included: a crash loop, in which the init system restarts the job on
	// its own.
	quiet := false
	back := func(cause error) error {
		return a.rollbackSelf(o, job, binary, previous, installed, rel.Tag, snapshot, quiet, cause)
	}
	// Nothing comes between the swap and the restart: until the restart, the
	// old daemon is running from a binary that is no longer on disk, and any
	// tailnet node it respawns starts from whatever is.
	if err := os.Rename(staged, binary); err != nil {
		_ = os.Remove(staged)   // the binary in use was never touched
		_ = os.Remove(previous) // a link to the binary in use; nothing to roll back to
		return fmt.Errorf("replace %s: %w; nothing was changed", binary, err)
	}
	o.progress(selfRestarting, rel.Tag)
	if err := o.restart(job); err != nil {
		return back(fmt.Errorf("%w; the daemon may have stopped. Its log: %s", err, logs))
	}
	o.progress(selfWaiting, rel.Tag)
	now, err := a.awaitNewDaemon(old, wait, logs)
	if err != nil {
		var none *noNewDaemonError
		if errors.As(err, &none) {
			quiet = true
			// Said without "probably down", which the rollback may undo.
			err = fmt.Errorf("the old daemon has gone, but no new one answered within %s. Its log: %s", none.wait, none.logs)
		}
		return back(fmt.Errorf("the new daemon (%s) did not come up: %w", rel.Tag, err))
	}
	if tag, ok := buildRelease(now.Version); !ok || !sameRelease(tag, rel.Tag) {
		return back(fmt.Errorf("the new daemon came back as %s, not %s", orDash(now.Version), rel.Tag))
	}
	fmt.Fprintf(a.out, "Outpost is running %s (was %s); the previous binary is kept at %s.\n", rel.Tag, installed, previous)
	fmt.Fprintln(a.out, "`outpost list` shows the services coming up.")
	return nil
}

// selfUpdateTimeout bounds asking GitHub about a release and downloading it.
const selfUpdateTimeout = 10 * time.Minute

// selfUpdateName is what holdUpdateLocks calls an update --self in progress.
const selfUpdateName = "Outpost itself"

// selfUpdateLock is the lock update --self holds for a home. It sits outside
// bin/, where a service's binary could be called anything, this name included.
func selfUpdateLock(home string) string { return filepath.Join(home, ".self-update.lock") }

// checkSelf says what update --self would do, for --check. With --to the
// release is the one named, not the latest, and may be older than this one.
func (a *app) checkSelf(build version.Info, installed string, clean bool, tag, to string) {
	which := "the latest release is " + tag
	if to != "" {
		which = "release " + tag + " is available"
	}
	if !clean {
		fmt.Fprintf(a.out, "This outpost is a development build (%s); %s.\n", build, which)
		return
	}
	order, ordered := compareReleases(installed, tag)
	switch {
	case sameRelease(installed, tag) && to == "":
		fmt.Fprintf(a.out, "Outpost is up to date (%s)\n", tag)
		return
	case sameRelease(installed, tag):
		fmt.Fprintf(a.out, "Outpost is already at %s\n", tag)
		return
	case ordered && order > 0 && to == "":
		fmt.Fprintf(a.out, "Outpost is at %s, newer than its latest release, %s\n", installed, tag)
		return
	case ordered && order > 0:
		fmt.Fprintf(a.out, "Outpost is at %s, newer than %s, so installing %s would go back.\n"+
			"If that is what you want, `outpost update --self --to %s`.\n", installed, tag, tag, tag)
		return
	}
	if to != "" {
		fmt.Fprintf(a.out, "Outpost: %s installed, release %s available\n", installed, tag)
		fmt.Fprintf(a.out, "Install it with `outpost update --self --to %s`.\n", to)
		return
	}
	fmt.Fprintf(a.out, "Outpost: %s installed, %s available\n", installed, tag)
	fmt.Fprintln(a.out, "Install it with `outpost update --self`.")
}

// snapshotAuth copies auth.db into the backups directory before a new daemon
// can migrate it, and returns where, or "" when there is no auth.db yet. Users
// and password hashes can't be rebuilt from anything, unlike state.db.
func (a *app) snapshotAuth(installed string) (string, error) {
	if _, err := os.Stat(a.authPath()); os.IsNotExist(err) {
		fmt.Fprintln(a.out, "There is no auth.db yet, so there is nothing to copy before updating.")
		return "", nil
	}
	if err := os.MkdirAll(a.backupsDir(), 0o700); err != nil {
		return "", err
	}
	safe := strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, installed)
	dst := filepath.Join(a.backupsDir(), fmt.Sprintf("auth-%s-%s.db", safe, time.Now().UTC().Format("20060102T150405Z")))
	if err := auth.Snapshot(a.authPath(), dst); err != nil {
		return "", err
	}
	fmt.Fprintf(a.out, "copied auth.db to %s\n", dst)
	a.pruneAuthSnapshots(keepAuthSnapshots)
	return dst, nil
}

// keepAuthSnapshots is how many auth.db copies update --self keeps. Each holds
// password hashes and signing keys as they were, including ones since changed
// or revoked, so they aren't kept forever.
const keepAuthSnapshots = 2

// authSnapshotName matches only the names snapshotAuth gives its copies, so
// nothing else in the backups directory is ever touched.
var authSnapshotName = regexp.MustCompile(`^auth-[A-Za-z0-9._-]+-(\d{8}T\d{6}Z)\.db$`)

// pruneAuthSnapshots removes all but the newest keep copies, by the time in
// their names. A copy it can't remove is only reported: the update matters
// more than the tidying.
func (a *app) pruneAuthSnapshots(keep int) {
	entries, err := os.ReadDir(a.backupsDir())
	if err != nil {
		fmt.Fprintf(a.err, "outpost: can't tidy older copies of auth.db: %v\n", err)
		return
	}
	type snap struct{ name, stamp string }
	var snaps []snap
	for _, e := range entries {
		m := authSnapshotName.FindStringSubmatch(e.Name())
		if m == nil || !e.Type().IsRegular() {
			continue
		}
		snaps = append(snaps, snap{e.Name(), m[1]})
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].stamp != snaps[j].stamp {
			return snaps[i].stamp > snaps[j].stamp
		}
		return snaps[i].name > snaps[j].name
	})
	for i := keep; i < len(snaps); i++ {
		path := filepath.Join(a.backupsDir(), snaps[i].name)
		if err := os.Remove(path); err != nil {
			fmt.Fprintf(a.err, "outpost: can't remove an older copy of auth.db: %v\n", err)
			continue
		}
		fmt.Fprintf(a.out, "removed an older copy, %s\n", path)
	}
}

// rollbackSelf puts the previous binary back after a failed update, restarts
// the daemon on it, and returns why the update failed. auth.db is left as the
// new daemon left it: one that migrated it may have done so correctly, and
// only the operator can tell, so the snapshot is named for them to decide.
func (a *app) rollbackSelf(o selfOpts, job daemonJob, binary, previous, installed, tag, snapshot string, quiet bool, cause error) error {
	o.progress(selfRollingBack, tag)
	wait := o.wait
	snap := ""
	if snapshot != "" {
		snap = fmt.Sprintf("\nauth.db as it was before the update is at %s.", snapshot)
	}
	logs := job.mgr.LogHint(job.cfg)
	if err := os.Rename(previous, binary); err != nil {
		return fmt.Errorf("%w\nRestoring the previous binary failed too (%v): %s is still in place at %s, "+
			"and the previous binary is at %s.%s", cause, err, tag, binary, previous, snap)
	}
	fmt.Fprintf(a.out, "Rolling back to %s.\n", installed)
	// After a restart in which nothing answered, the init system is already
	// starting the job again, and starts it on whatever binary is in place:
	// now the previous one. Restarting would only stop that daemon if it
	// came up first, so wait for whichever daemon answers. One running but
	// not answering goes the usual way, which says so.
	if _, pid := a.supervisorPID(); quiet && o.selfStarts && !pid {
		if _, err := a.client().Identity(); err != nil {
			fmt.Fprintf(a.out, "  No daemon is answering after the update; %s starts one on the binary now in place.\n", job.mgr.Name())
		}
		back, err := a.awaitNewDaemon(ipc.DaemonIdentity{}, wait, logs)
		if err != nil {
			return fmt.Errorf("%w\nRolled back to the previous binary, but no daemon answered within %s, "+
				"so the services are probably down. Log: %s%s", cause, wait, logs, snap)
		}
		return fmt.Errorf("%w\nRolled back: the daemon is running %s again.%s", cause, orDash(back.Version), snap)
	}
	old := a.currentDaemon()
	if err := o.restart(job); err != nil {
		return fmt.Errorf("%w\nRolled back to the previous binary, but restarting the daemon on it failed: %v. "+
			"The services are probably down. Log: %s%s", cause, err, logs, snap)
	}
	back, err := a.awaitNewDaemon(old, wait, logs)
	if err != nil {
		return fmt.Errorf("%w\nRolled back to the previous binary, but no daemon answered within %s, "+
			"so the services are probably down. Log: %s%s", cause, wait, logs, snap)
	}
	return fmt.Errorf("%w\nRolled back: the daemon is running %s again.%s", cause, orDash(back.Version), snap)
}

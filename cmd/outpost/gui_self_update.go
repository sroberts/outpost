package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/acquire"
	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/gateway"
)

// selfWeb lets the gateway update Outpost itself. The update is `update
// --self`'s own code, run by a helper process that outlives the daemon's
// restart, and reporting through a file in the home that the next daemon reads.
type selfWeb struct {
	a *app
	// mu keeps two starts apart; fileMu keeps the status file's readers
	// and writers in this process apart, without a page waiting on a start.
	mu     sync.Mutex
	fileMu sync.Mutex
	// readErr is the last status file error logged, so a broken file is
	// reported once rather than on every look.
	readErr string

	checkMu sync.Mutex
	seen    seenRelease
}

// selfResultFor is how long a finished update stays on the page.
const selfResultFor = 24 * time.Hour

// jobLabel is the label of the init-system job this process runs under, or ""
// when it isn't under one. launchd names the job a process runs for in
// XPC_SERVICE_NAME; systemd gives each run of a unit an INVOCATION_ID, and
// the unit is the last part of the process's cgroup. A daemon started by hand
// has nothing to start it again once the update stops it. A variable so tests
// can run under a job.
var jobLabel = func() string {
	switch goruntime.GOOS {
	case "darwin":
		// A terminal sets it to 0.
		if l := os.Getenv("XPC_SERVICE_NAME"); l != "0" {
			return l
		}
	case "linux":
		if os.Getenv("INVOCATION_ID") == "" {
			return ""
		}
		body, err := os.ReadFile("/proc/self/cgroup")
		if err != nil {
			return ""
		}
		return cgroupLabel(string(body))
	}
	return ""
}

// cgroupLabel reads the systemd service a process runs in from its
// /proc/self/cgroup: the unified hierarchy's "0::" line, or under cgroup v1
// the name=systemd line, whichever order they come in. A hybrid system has
// both, and its "0::" line may be only "/", so the v1 line stands in then.
func cgroupLabel(cgroup string) string {
	var v1, v2 string
	for _, line := range strings.Split(cgroup, "\n") {
		line = strings.TrimSpace(line)
		if p, ok := strings.CutPrefix(line, "0::"); ok && v2 == "" {
			v2 = serviceLabel(p)
		}
		if _, p, ok := strings.Cut(line, ":name=systemd:"); ok && v1 == "" {
			v1 = serviceLabel(p)
		}
	}
	if v2 != "" {
		return v2
	}
	return v1
}

// serviceLabel is the label of the service a cgroup path ends in, or "".
func serviceLabel(cgroup string) string {
	if unit := path.Base(cgroup); strings.HasSuffix(unit, ".service") {
		return strings.TrimSuffix(unit, ".service")
	}
	return ""
}

// Latest checks Outpost's releases, remembering the etag so asking hourly
// costs a token's rate limit nothing.
func (w *selfWeb) Latest(ctx context.Context) (gateway.ReleaseCheck, error) {
	w.checkMu.Lock()
	seen := w.seen
	w.checkMu.Unlock()

	client := acquireClient()
	// Each client has its own transport, and an idle connection left behind on
	// one would stay for as long as the daemon runs.
	defer client.HTTP.CloseIdleConnections()
	rel, etag, err := client.LatestReleaseSince(ctx, selfRepo, seen.etag)
	var none *acquire.NoAssetsError
	switch {
	case errors.Is(err, acquire.ErrNotModified):
		return selfVerdict(seen), nil
	case errors.As(err, &none):
		seen = seenRelease{etag: etag, tag: none.Tag, noAssets: true}
	case err != nil:
		return gateway.ReleaseCheck{}, err
	default:
		seen = seenRelease{etag: etag, tag: rel.Tag}
	}
	w.checkMu.Lock()
	w.seen = seen
	w.checkMu.Unlock()
	return selfVerdict(seen), nil
}

// selfVerdict is what the latest release means for this build, by the rules
// `update --self` goes by.
func selfVerdict(latest seenRelease) gateway.ReleaseCheck {
	check := gateway.ReleaseCheck{Latest: latest.tag}
	if latest.noAssets {
		check.Note = latest.tag + " has nothing attached yet; its build may still be uploading."
		check.Pending = true
		return check
	}
	build := selfBuild()
	installed, clean := buildRelease(build.String())
	later, described := laterThanBase(build.String(), latest.tag)
	switch order, ordered := compareReleases(installed, latest.tag); {
	case !clean && described && later:
		check.Newer = true
	case !clean && described:
		base, _ := madeFrom(build.String())
		check.Note = fmt.Sprintf("This development build was made from %s, so it has everything in %s.", base, latest.tag)
	case !clean:
		// The row already shows the build; saying it again only repeats it.
		check.Note = fmt.Sprintf("This is a development build, so Outpost can't tell whether %s is newer.", latest.tag)
	case sameRelease(installed, latest.tag):
	case ordered && order > 0:
		check.Note = fmt.Sprintf("Newer than the latest release, %s.", latest.tag)
	case ordered:
		check.Newer = true
	}
	return check
}

// Refusal says why the page can't update Outpost, or "".
func (w *selfWeb) Refusal() string {
	if _, _, err := w.ready(); err != nil {
		return err.Error()
	}
	return ""
}

// ready makes the checks a start can make cheaply, before anything is
// changed, and returns the job and the binary to replace.
func (w *selfWeb) ready() (daemonJob, string, error) {
	build := selfBuild()
	// One made cleanly from a release is let through: the helper installs
	// only a later one, which is the only kind the page offers it.
	_, described := madeFrom(build.String())
	if _, clean := buildRelease(build.String()); !clean && !described {
		return daemonJob{}, "", fmt.Errorf("this is a development build (%s), not a release; "+
			"`outpost update --self --to TAG` on the host installs one", build)
	}
	label := jobLabel()
	if label == "" {
		return daemonJob{}, "", errors.New("the daemon wasn't started by its autostart job, so nothing would start it again " +
			"after the update; run `outpost update --self` on the host instead")
	}
	job, binary, err := w.a.selfJob(label)
	if err != nil {
		return daemonJob{}, "", err
	}
	if err := refuseScope(job); err != nil {
		return daemonJob{}, "", err
	}
	if err := refuseReplace(binary); err != nil {
		return daemonJob{}, "", err
	}
	return job, binary, nil
}

// refuseScope says why the helper can't outlive a job's restart, or nil.
// systemd stops every process in a system service's cgroup with it, and a
// process of the daemon's can't start a unit of its own at that scope.
func refuseScope(job daemonJob) error {
	if job.mgr.Name() != "systemd" || job.cfg.Scope != autostart.System {
		return nil
	}
	who := job.cfg.User
	if who == "" {
		who = "the user it runs as"
	}
	return fmt.Errorf("this Outpost runs as a system service, which can't be restarted from here; "+
		"run `outpost update --self` on the host as %s (with sudo for the restart)", who)
}

// Start starts the helper, once the checks that can be made now have passed.
// The status file is written first, so the page shows the update from the
// moment this returns, and a second press sees it.
func (w *selfWeb) Start(by string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if st, ok := w.read(); ok && !st.final() && !st.abandoned(time.Now()) {
		return errors.New("an update of Outpost is already running")
	}
	job, binary, err := w.ready()
	if err != nil {
		return err
	}
	// The helper checks again, holding the locks; this is so the page can say
	// so now rather than in the update's result.
	updating, release, err := holdUpdateLocks(w.a.home)
	release()
	if err != nil {
		return err
	}
	if len(updating) > 0 {
		return fmt.Errorf("an update is running (%s), and restarting Outpost would end it where it stands; "+
			"try again once it's done", strings.Join(updating, ", "))
	}

	argv, err := w.helperArgv(job, binary)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(w.a.logsDir(), 0o700); err != nil {
		return err
	}
	log := filepath.Join(w.a.logsDir(), "self-update.log")
	st := selfStatus{
		ID: newSelfID(), Started: time.Now(), Phase: selfStarting,
		From: selfBuild().String(), By: by, Log: log,
	}
	if job.mgr.Name() == "systemd" {
		st.Log = "journalctl --user -u " + selfUpdateUnit(job.cfg.Label)
	}
	if err := w.write(st); err != nil {
		return fmt.Errorf("record the update's progress: %w", err)
	}
	pid, err := spawnSelfUpdate(argv, helperEnv(os.Environ()), log)
	if err != nil {
		st.Phase, st.Failed, st.Finished = selfFailed, true, time.Now()
		st.Output = "Couldn't start the update: " + err.Error()
		// Shown on the page that asked; there's nothing to announce.
		st.Recorded = true
		_ = w.write(st) // the error returned is what the admin needs; this only clears "starting"
		return fmt.Errorf("couldn't start the update: %w", err)
	}
	fmt.Fprintf(w.a.out, "outpost: %s started an update of Outpost (helper pid %d)\n", by, pid)
	return nil
}

// helperArgv is the helper's command line. Nothing in it comes from the
// request: the binary is the one the job runs, which is this one, and the
// home and label are the daemon's own.
func (w *selfWeb) helperArgv(job daemonJob, binary string) ([]string, error) {
	home, err := filepath.Abs(w.a.home)
	if err != nil {
		return nil, err
	}
	helper := []string{binary, "--home", home, selfUpdateCommand, "--label", job.cfg.Label}
	if job.mgr.Name() != "systemd" {
		return helper, nil
	}
	// A unit of its own, outside the daemon's cgroup. It starts from the
	// user manager's environment, not the daemon's, so what it needs to
	// reach GitHub is passed on by name: a token for the rate limit, and any
	// proxy or certificates. systemd-run copies its own value.
	run := []string{"systemd-run", "--user", "--collect", "--quiet", "--unit=" + selfUpdateUnit(job.cfg.Label)}
	for _, name := range helperPassOn {
		if os.Getenv(name) != "" {
			run = append(run, "--setenv="+name)
		}
	}
	return append(append(run, "--"), helper...), nil
}

// helperPassOn is the daemon's environment a helper under systemd-run is
// given. Never a Tailscale key: the helper brings up no node.
var helperPassOn = []string{
	"GITHUB_TOKEN", "GH_TOKEN",
	"HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "https_proxy", "http_proxy", "no_proxy",
	"SSL_CERT_FILE", "SSL_CERT_DIR",
}

// selfUpdateUnit names the helper's transient unit after the daemon's, with
// anything systemd wouldn't take in a unit name made a dash.
func selfUpdateUnit(label string) string {
	safe := strings.Map(func(r rune) rune {
		if r == '.' || r == '-' || r == '_' || r == ':' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '-'
	}, label)
	return "outpost-self-update-" + safe
}

// helperEnv is the daemon's environment without its Tailscale auth keys: the
// helper never brings up a node, and a key it doesn't hold can't leak from it.
func helperEnv(env []string) []string {
	var out []string
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "TS_AUTHKEY", "TS_AUTH_KEY", "OUTPOST_TSNET_AUTHKEY":
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Status is the latest update as the status file has it. One whose helper
// has died is recorded as failed, and a finished one is announced once.
func (w *selfWeb) Status() (gateway.SelfStatus, bool) {
	w.fileMu.Lock()
	defer w.fileMu.Unlock()
	st, ok := w.readLocked()
	if !ok {
		return gateway.SelfStatus{}, false
	}
	now := time.Now()
	if st.abandoned(now) {
		st.Phase, st.Failed, st.Finished = selfFailed, true, now
		st.Output = strings.TrimSpace(st.Output + "\nThe update stopped before it finished. Its log: " + st.Log)
		if err := writeSelfStatus(w.a.home, st); err != nil {
			fmt.Fprintf(w.a.err, "outpost: %v\n", err)
		}
	}
	out := gateway.SelfStatus{
		Phase: st.Phase, Final: st.final(), Failed: st.Failed,
		From: st.From, To: st.To, By: st.By, Output: st.Output, Log: st.Log,
		RolledBack: st.RolledBack, Error: st.Error,
	}
	// Announced before an old outcome is hidden, so one first read after a
	// day still reaches the activity feed, once.
	if st.final() && !st.Recorded {
		st.Recorded = true
		// Announced only once it's marked, or a file that can't be written
		// would have it announced on every look.
		if err := writeSelfStatus(w.a.home, st); err != nil {
			fmt.Fprintf(w.a.err, "outpost: %v\n", err)
		} else {
			out.Announce = true
		}
	}
	if st.final() && now.Sub(st.Finished) > selfResultFor {
		return gateway.SelfStatus{Announce: out.Announce, Phase: out.Phase, Final: true, Failed: out.Failed,
			From: out.From, To: out.To, By: out.By}, false
	}
	return out, true
}

func (w *selfWeb) read() (selfStatus, bool) {
	w.fileMu.Lock()
	defer w.fileMu.Unlock()
	return w.readLocked()
}

// readLocked reads the status file, reporting a broken one to the log: the
// page carries on without it. Called with fileMu held.
func (w *selfWeb) readLocked() (selfStatus, bool) {
	st, ok, err := readSelfStatus(w.a.home)
	if err != nil {
		if err.Error() != w.readErr {
			w.readErr = err.Error()
			fmt.Fprintf(w.a.err, "outpost: %v\n", err)
		}
		return selfStatus{}, false
	}
	w.readErr = ""
	return st, ok
}

func (w *selfWeb) write(st selfStatus) error {
	w.fileMu.Lock()
	defer w.fileMu.Unlock()
	return writeSelfStatus(w.a.home, st)
}

// newSelfID names one update, to tell its status from an earlier one's.
func newSelfID() string {
	var b [8]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails on the platforms Outpost runs on
	return hex.EncodeToString(b[:])
}

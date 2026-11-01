package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/autostart"
	"github.com/sroberts/outpost/internal/ipc"
	"github.com/sroberts/outpost/internal/version"
)

// daemonPoll is how often a restart checks on the daemon; a variable so tests
// need not wait out real time.
var daemonPoll = 200 * time.Millisecond

// defaultRestartWait covers the old daemon's slowest stop (the job's own limit
// is 60s) and then the new one's whole startup deadline.
const defaultRestartWait = 60*time.Second + defaultStartupTimeout

// cmdDaemon handles `outpost daemon restart`.
func (a *app) cmdDaemon(args []string) error {
	if len(args) == 0 || args[0] != "restart" {
		return errors.New("usage: outpost daemon restart [--force] [--wait D] [--system] [--label L]")
	}
	return a.daemonRestart(args[1:])
}

// daemonJob is an installed autostart job that runs this home.
type daemonJob struct {
	mgr    autostart.Manager
	cfg    autostart.Config
	unit   string
	loaded bool
}

// daemonRestart restarts the daemon through the init system, which is the only
// thing that can do it: a daemon can't start its own replacement, and one that
// isn't under an init system isn't ours to kill.
func (a *app) daemonRestart(args []string) error {
	fset := flag.NewFlagSet("daemon restart", flag.ContinueOnError)
	fset.SetOutput(a.err)
	system := fset.Bool("system", false, "restart the boot-time service, without looking for a per-user one first")
	label := fset.String("label", autostart.DefaultLabel, "job label / unit name")
	force := fset.Bool("force", false, "restart even during an update, or with a manifest the new daemon would refuse")
	wait := fset.Duration("wait", defaultRestartWait, "how long the new daemon has to answer")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 0 {
		return fmt.Errorf("daemon restart: unexpected argument %q", fset.Arg(0))
	}

	job, err := a.loadedDaemonJob(*label, *system)
	if err != nil {
		return err
	}
	uid := effectiveUID()

	// The daemon refuses to start on a manifest it can't validate, while a
	// running one only logs it and carries on, so a bad edit can sit unnoticed
	// until a restart that leaves every service down.
	if _, err := a.validateManifests(); err != nil {
		if !*force {
			return fmt.Errorf("the new daemon would refuse to start, leaving every service down: %w\n"+
				"Nothing was restarted. Fix that, or pass --force", err)
		}
		fmt.Fprintf(a.err, "outpost: --force: the new daemon may refuse to start: %v\n", err)
	}

	// Held across the restart command, so an update can't start between this
	// check and the old daemon going.
	updating, release, err := holdUpdateLocks(a.home)
	if err != nil {
		return err
	}
	defer release()
	if len(updating) > 0 {
		if !*force {
			return fmt.Errorf("an update is running (%s), and restarting the daemon would end it where it stands, "+
				"with no rollback; wait for it to finish, or pass --force", strings.Join(updating, ", "))
		}
		fmt.Fprintf(a.err, "outpost: --force: ending the running update of %s with no rollback\n", strings.Join(updating, ", "))
	}

	old := a.currentDaemon()
	fmt.Fprintf(a.out, "Restarting the daemon: %s job %s, %s scope.\n", job.mgr.Name(), job.cfg.Label, job.cfg.Scope)
	fmt.Fprintln(a.out, "Its services stop with it, each with SIGTERM and SIGKILL after 10s. The new daemon starts them")
	fmt.Fprintln(a.out, "again, apart from any stopped by hand or halted after a crash loop, which stay that way.")

	if err := a.runAll(job.mgr.RestartCommands(job.cfg, uid)); err != nil {
		return fmt.Errorf("%w\nThe daemon may have stopped. Its log: %s", err, job.mgr.LogHint(job.cfg))
	}
	release()

	back, err := a.awaitNewDaemon(old, *wait, job.mgr.LogHint(job.cfg))
	if err != nil {
		return err
	}
	fmt.Fprintf(a.out, "The daemon is back: %s", back.Version)
	// A daemon too old to say has no PID to show until it writes its pidfile.
	if back.PID != 0 {
		fmt.Fprintf(a.out, " (pid %d)", back.PID)
	}
	fmt.Fprintln(a.out, ". `outpost list` shows its services coming up.")
	if back.Version != version.Get().String() {
		fmt.Fprintf(a.out, "It is running a different build from this CLI (%s). The job may run another binary;\n"+
			"`outpost install` says if so.\n", version.Get())
	}
	return nil
}

// loadedDaemonJob finds this home's job and checks it is one the init system
// has loaded and this user can restart.
func (a *app) loadedDaemonJob(label string, system bool) (daemonJob, error) {
	job, err := a.findDaemonJob(label, system)
	if err != nil {
		return daemonJob{}, err
	}
	if job.cfg.Scope == autostart.User && effectiveUID() == 0 {
		return daemonJob{}, fmt.Errorf("%s is a per-user job, and root can't reach it; run this without sudo", job.cfg.Label)
	}
	if !job.loaded {
		return daemonJob{}, a.jobNotLoaded(job)
	}
	return job, nil
}

// findDaemonJob looks for this home's job at each scope. The label alone isn't
// enough: --home or OUTPOST_HOME can point here at a scratch home while the
// default label belongs to a real daemon, and restarting that one would take
// down services this command was never asked about.
func (a *app) findDaemonJob(label string, systemOnly bool) (daemonJob, error) {
	scopes := []bool{false, true}
	if systemOnly {
		scopes = []bool{true}
	}
	var found []daemonJob
	var looked []string
	var foreign error
	for _, system := range scopes {
		mgr, cfg, err := a.autostartConfig(label, system)
		if err != nil {
			return daemonJob{}, err
		}
		unit, err := mgr.UnitPath(cfg)
		if err != nil {
			return daemonJob{}, err
		}
		body, err := os.ReadFile(unit)
		if os.IsNotExist(err) {
			looked = append(looked, unit)
			continue
		}
		if err != nil {
			return daemonJob{}, err
		}
		home, err := mgr.UnitHome(string(body))
		if err != nil {
			foreign = fmt.Errorf("can't tell which Outpost home %s runs (%v), so it was left alone", unit, err)
			continue
		}
		if !samePath(home, cfg.Home) {
			foreign = fmt.Errorf("%s runs the Outpost home %s, not %s, so it was left alone; "+
				"to restart that daemon, pass --home %s", unit, home, cfg.Home, home)
			continue
		}
		found = append(found, daemonJob{mgr: mgr, cfg: cfg, unit: unit})
	}

	if len(found) == 0 {
		if foreign != nil {
			return daemonJob{}, fmt.Errorf("no autostart job runs this home: %w%s", foreign, a.handDaemonNote())
		}
		return daemonJob{}, a.noDaemonJob(label, looked)
	}
	for _, job := range found {
		if initProbe(job.mgr.LoadedCommand(job.cfg, effectiveUID())) {
			job.loaded = true
			return job, nil
		}
	}
	return found[0], nil
}

// samePath compares two paths the way the filesystem would: on macOS a temp
// directory is reached through a symlink, so spellings differ.
func samePath(a, b string) bool {
	resolve := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return filepath.Clean(p)
	}
	return resolve(a) == resolve(b)
}

// noDaemonJob explains that there is nothing to restart through, and leaves
// alone a daemon someone is running in a terminal.
func (a *app) noDaemonJob(label string, looked []string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no autostart job named %s is installed (looked for %s).", label, strings.Join(looked, ", "))
	b.WriteString(a.handDaemonNote())
	b.WriteString("\n`outpost install --enable` sets up a job, and from then on this command works. " +
		"A job with another label takes --label.")
	return errors.New(b.String())
}

// handDaemonNote is what to say about a daemon that's up with no job to restart
// it through, as its own line of an error, or nothing if none is up.
func (a *app) handDaemonNote() string {
	pid := a.currentDaemon().PID
	if pid == 0 {
		return ""
	}
	return fmt.Sprintf("\nA daemon is running (pid %d), so it was started some other way, and it was left alone. "+
		"Stop it where it runs, with Ctrl-C if that's a terminal, and start it again.", pid)
}

// jobNotLoaded says how to start a job whose file exists but isn't running.
func (a *app) jobNotLoaded(job daemonJob) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is installed at %s, but %s doesn't have it loaded, so there is nothing to restart.",
		job.cfg.Label, job.unit, job.mgr.Name())
	b.WriteString(a.handDaemonNote())
	b.WriteString("\nTo start the job:")
	for _, cmd := range job.mgr.EnableCommands(job.unit, job.cfg) {
		fmt.Fprintf(&b, "\n  %s", strings.Join(cmd, " "))
	}
	return errors.New(b.String())
}

// restartHint is the restart command for this job, with whichever flags it
// takes to find the same job again.
func (a *app) restartHint(cfg autostart.Config) string {
	hint := "outpost"
	if def, _, err := parseGlobals(nil); err == nil && !samePath(def, cfg.Home) {
		hint += " --home " + cfg.Home
	}
	hint += " daemon restart"
	if cfg.Label != autostart.DefaultLabel {
		hint += " --label " + cfg.Label
	}
	return hint + scopeFlag(cfg)
}

// currentDaemon identifies whichever daemon is up now; PID 0 means none. The
// pidfile fills in for a daemon that can't say, which includes the one running
// when this command is first used: it predates being able to.
func (a *app) currentDaemon() ipc.DaemonIdentity {
	id, err := a.client().Identity()
	if err != nil || id.PID == 0 {
		if pid, running := a.supervisorPID(); running {
			id.PID = pid
		}
	}
	return id
}

// isSameRun reports whether an answer came from the daemon identified by old.
// With either start time missing, the PID has to do.
func isSameRun(old, now ipc.DaemonIdentity) bool {
	if old.PID != now.PID {
		return false
	}
	return old.Started.IsZero() || now.Started.IsZero() || old.Started.Equal(now.Started)
}

// awaitNewDaemon waits for a daemon other than old to answer on the socket.
// Whatever answers first after the restart command may still be the old one,
// so "the socket answers" alone would report success too early. Each stage has
// a different cause when it fails, so the message comes from the last one seen.
func (a *app) awaitNewDaemon(old ipc.DaemonIdentity, wait time.Duration, logs string) (ipc.DaemonIdentity, error) {
	deadline := time.Now().Add(wait)
	c := a.client()
	stage := ""
	for {
		id, err := c.Identity()
		if err == nil {
			if id.PID == 0 {
				if pid, running := a.supervisorPID(); running {
					id.PID = pid
				}
			}
			if old.PID == 0 || !isSameRun(old, id) {
				return id, nil
			}
		}

		var now, failure string
		switch {
		case err == nil:
			now = fmt.Sprintf("Waiting for the old daemon (pid %d) to go.", old.PID)
			failure = fmt.Sprintf("the old daemon (pid %d) is still answering after %s; if it was started by hand "+
				"rather than by this job, a restart doesn't reach it, so stop it where it runs. Log: %s", old.PID, wait, logs)
		case processAlive(old.PID):
			now = fmt.Sprintf("The old daemon (pid %d) is stopping its services.", old.PID)
			failure = fmt.Sprintf("the old daemon (pid %d) is still stopping its services after %s; "+
				"the init system kills it once it has had 60s. Log: %s", old.PID, wait, logs)
		default:
			now = "Waiting for the new daemon to answer."
			failure = fmt.Sprintf("the old daemon has gone, but no new one answered within %s, "+
				"so the services are probably down. Log: %s", wait, logs)
		}
		if now != stage {
			fmt.Fprintf(a.out, "  %s\n", now)
			stage = now
		}
		if time.Now().After(deadline) {
			return ipc.DaemonIdentity{}, errors.New(failure)
		}
		time.Sleep(daemonPoll)
	}
}

// processAlive reports whether pid exists; 0 is no process at all. A process
// that can't be signalled exists, and belongs to someone else.
func processAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// holdUpdateLocks probes the lock lockUpdate takes beside each binary, and keeps
// every one that was free until release is called. It returns the binaries
// with an update running. The files are found on disk rather than through the
// manifests, because a manifest that doesn't parse shouldn't hide an update, and
// update refuses any binary outside the home's bin directory.
func holdUpdateLocks(home string) (updating []string, release func(), err error) {
	var held []*os.File
	release = func() {
		for _, f := range held {
			_ = f.Close() // closing releases the lock, and there's nothing to do if it fails
		}
		held = nil
	}
	walkErr := filepath.WalkDir(filepath.Join(home, "bin"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := d.Name()
		if d.IsDir() || !strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".update-lock") {
			return nil
		}
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		switch err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); {
		case err == nil:
			held = append(held, f)
		case errors.Is(err, syscall.EWOULDBLOCK):
			_ = f.Close() // only the lock's state mattered
			updating = append(updating, strings.TrimSuffix(strings.TrimPrefix(name, "."), ".update-lock"))
		default:
			_ = f.Close() // the flock error is the one that matters
			return fmt.Errorf("check for a running update: %w", err)
		}
		return nil
	})
	if walkErr != nil {
		release()
		return nil, func() {}, walkErr
	}
	return updating, release, nil
}

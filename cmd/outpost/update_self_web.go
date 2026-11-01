package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// selfUpdateCommand is the hidden command the gateway starts to update
// Outpost. It is a process of its own because the daemon can't outlive its
// own restart: whatever replaces the binary and waits for the new daemon has
// to still be there once the old one has gone.
const selfUpdateCommand = "__self-update"

// signalDaemon stops the daemon so its init system starts it again, on the
// binary now in place. A variable so no test signals a real process.
var signalDaemon = func(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// selfWebWait is how long the helper gives a new daemon to answer: the same
// as `update --self`. A variable so tests need not wait it out.
var selfWebWait = defaultRestartWait

// selfStatus is what the helper has done so far, kept in the home for the
// gateway to show. The daemon that started the helper is gone before it
// finishes, so the answer can't be kept in memory.
type selfStatus struct {
	ID        string    `json:"id"`
	Started   time.Time `json:"started"`
	HelperPID int       `json:"helper_pid"`
	Phase     string    `json:"phase"`
	From      string    `json:"from"`
	To        string    `json:"to,omitempty"`
	Output    string    `json:"output,omitempty"`
	Failed    bool      `json:"failed,omitempty"`
	Finished  time.Time `json:"finished,omitzero"`
	// By is the admin who asked, for the activity feed's record of the
	// outcome, which the daemon after the restart makes.
	By string `json:"by,omitempty"`
	// Log is where the helper's own output goes, for when it never reports.
	Log string `json:"log,omitempty"`
	// Recorded says the outcome is in the activity feed, so a daemon
	// restarted again within the day doesn't record it twice.
	Recorded bool `json:"recorded,omitempty"`
	// RolledBack says the previous binary was put back, which is what lets
	// the page say "went back to" only when something actually went back.
	RolledBack bool `json:"rolled_back,omitempty"`
	// Error is why it failed, in one line, for the page's summary; the
	// whole of it is in Output.
	Error string `json:"error,omitempty"`
}

// final reports whether the helper has finished, one way or the other.
func (s selfStatus) final() bool { return s.Phase == selfDone || s.Phase == selfFailed }

// selfAbandonAfter is how long a helper the daemon couldn't name a PID for has
// to write its own before it's taken never to have started.
const selfAbandonAfter = time.Minute

// selfLongest is longer than any helper runs: the download, then a wait for
// the new daemon and another for a rollback, and a margin.
func selfLongest() time.Duration { return selfUpdateTimeout + 2*selfWebWait + 5*time.Minute }

// abandoned reports whether a status that isn't final will never be: its
// helper has died, or never started. One older than any helper runs is
// abandoned whatever its PID says, since after a reboot that number may be
// another process's, which would keep the update looking busy for good.
func (s selfStatus) abandoned(now time.Time) bool {
	if s.final() {
		return false
	}
	if now.Sub(s.Started) > selfLongest() {
		return true
	}
	if s.HelperPID != 0 {
		return !processAlive(s.HelperPID)
	}
	return now.Sub(s.Started) > selfAbandonAfter
}

func selfStatusPath(home string) string { return filepath.Join(home, "self-update.json") }

// readSelfStatus reads the status file; false with no error means there is none.
func readSelfStatus(home string) (selfStatus, bool, error) {
	body, err := os.ReadFile(selfStatusPath(home))
	if os.IsNotExist(err) {
		return selfStatus{}, false, nil
	}
	if err != nil {
		return selfStatus{}, false, err
	}
	var s selfStatus
	if err := json.Unmarshal(body, &s); err != nil {
		return selfStatus{}, false, fmt.Errorf("%s: %w", selfStatusPath(home), err)
	}
	return s, true, nil
}

// writeSelfStatus replaces the status file in one step, so a page polling it
// never reads half of one. It holds an admin's name and the update's output,
// so only this user can read it.
func writeSelfStatus(home string, s selfStatus) error {
	f, err := os.CreateTemp(home, ".self-update-*.json")
	if err != nil {
		return err
	}
	tmp := f.Name()
	fail := func(err error) error {
		_ = f.Close()      // the write already failed
		_ = os.Remove(tmp) // a partial file is no use to anyone
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		return fail(err)
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp) // a partial file is no use to anyone
		return err
	}
	if err := os.Rename(tmp, selfStatusPath(home)); err != nil {
		_ = os.Remove(tmp) // the rename is what failed; the old status stands
		return err
	}
	return nil
}

// selfRecorder is the helper's output, copied to where it would have gone and
// into the status file at each phase, so the page shows what the CLI would.
type selfRecorder struct {
	home string
	to   io.Writer
	mu   sync.Mutex
	st   selfStatus
	out  bytes.Buffer
}

func (r *selfRecorder) Write(p []byte) (int, error) {
	r.mu.Lock()
	r.out.Write(p)
	r.mu.Unlock()
	return r.to.Write(p)
}

// phase records a new phase, and with it the output so far.
func (r *selfRecorder) phase(phase, tag string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Phase = phase
	if phase == selfRollingBack {
		r.st.RolledBack = true
	}
	if tag != "" {
		r.st.To = tag
	}
	r.save()
}

// finish records how it ended. An update that found nothing to do has done.
func (r *selfRecorder) finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.st.Phase, r.st.Finished = selfDone, time.Now()
	if err != nil {
		r.st.Phase, r.st.Failed = selfFailed, true
		r.st.Error, _, _ = strings.Cut(err.Error(), "\n")
		r.out.WriteString(err.Error() + "\n")
	}
	r.save()
}

// save writes the status. Called with mu held. A status that can't be written
// is only reported: the update itself matters more, and the log has it all.
func (r *selfRecorder) save() {
	r.st.Output = strings.TrimSpace(r.out.String())
	if err := writeSelfStatus(r.home, r.st); err != nil {
		fmt.Fprintf(r.to, "outpost: can't record progress in %s: %v\n", selfStatusPath(r.home), err)
	}
}

// cmdSelfUpdate is the helper: `update --self` with the daemon restarted by a
// signal rather than through the init system, which a process the daemon
// started isn't always allowed to ask. It takes no release and no overrides;
// what it installs is the latest release, by the CLI's own rules.
func (a *app) cmdSelfUpdate(args []string) error {
	fset := flag.NewFlagSet(selfUpdateCommand, flag.ContinueOnError)
	fset.SetOutput(a.err)
	label := fset.String("label", "", "the label of the daemon's autostart job")
	if err := fset.Parse(args); err != nil {
		return err
	}
	if fset.NArg() > 0 || *label == "" {
		return fmt.Errorf("%s is started by the gateway, as %s --label L", selfUpdateCommand, selfUpdateCommand)
	}

	st, ok, err := readSelfStatus(a.home)
	if err != nil || !ok || st.final() {
		// Run by hand, or the daemon's record is unreadable: start a record
		// of its own rather than refuse, since the update is the same either way.
		st = selfStatus{ID: newSelfID(), Started: time.Now(), From: selfBuild().String()}
	}
	st.HelperPID = os.Getpid()
	st.Phase = selfStarting
	rec := &selfRecorder{home: a.home, to: a.out, st: st}
	rec.mu.Lock()
	rec.save()
	rec.mu.Unlock()

	run := *a
	run.out, run.err = rec, rec
	err = run.updateSelfWith(selfOpts{
		label:      *label,
		wait:       selfWebWait,
		restart:    run.signalRestart,
		selfStarts: true,
		progress:   rec.phase,
	})
	rec.finish(err)
	return err
}

// signalRestart stops the daemon answering on this home's socket, for its init
// system to start again. Only that PID is ever signalled: the pidfile can name
// a process that has since gone and whose number was reused. With no daemon
// answering, as in a crash loop, the init system is already starting one, and
// it starts it on whatever binary is now in place.
func (a *app) signalRestart(job daemonJob) error {
	id, err := a.client().Identity()
	if err != nil {
		// Not answering isn't the same as not there: a daemon that is up but
		// stuck would carry on as it is, and nothing would start the new one.
		if pid, running := a.supervisorPID(); running {
			return fmt.Errorf("the daemon (pid %d) isn't answering, so it wasn't restarted (%v)", pid, err)
		}
		fmt.Fprintf(a.out, "  No daemon is answering, and none is running; %s starts one on the binary now in place.\n", job.mgr.Name())
		return nil
	}
	if id.PID == 0 {
		return errors.New("the daemon answering doesn't say which process it is, so it can't be restarted from here")
	}
	fmt.Fprintf(a.out, "  Stopping the daemon (pid %d) for %s to start again.\n", id.PID, job.mgr.Name())
	if err := signalDaemon(id.PID); err != nil {
		return fmt.Errorf("stop the daemon (pid %d): %w", id.PID, err)
	}
	return nil
}

// spawnSelfUpdate starts the helper where the daemon's restart won't take it
// down: in a session of its own, which launchd leaves alone, or through
// systemd-run, in a unit of its own, since systemd stops everything in the
// daemon's cgroup with it. Its output goes to log; under systemd-run that is
// only systemd-run's, and the helper's goes to the journal. A variable so no
// test starts one.
var spawnSelfUpdate = func(argv, env []string, log string) (pid int, err error) {
	out, err := os.OpenFile(log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return 0, err
	}
	defer out.Close() // the child has its own copy
	in, err := os.Open(os.DevNull)
	if err != nil {
		return 0, err
	}
	defer in.Close() // the child has its own copy
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	// Reaped, so it isn't left a zombie if it finishes before the daemon
	// goes; its outcome is in the status file, not its exit code.
	go func() { _ = cmd.Wait() }()
	return cmd.Process.Pid, nil
}

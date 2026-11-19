package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/autostart"
)

// sandboxSystemUnits moves system-scope unit files into a temporary directory,
// which $HOME alone can't do: their path is fixed.
func sandboxSystemUnits(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	prev := unitPath
	t.Cleanup(func() { unitPath = prev })
	unitPath = func(mgr autostart.Manager, cfg autostart.Config) (string, error) {
		path, err := mgr.UnitPath(cfg)
		if err != nil || cfg.Scope != autostart.System {
			return path, err
		}
		return filepath.Join(dir, filepath.Base(path)), nil
	}
}

// writeSystemJob installs a system-scope unit file running the given home, at
// the sandboxed path.
func writeSystemJob(t *testing.T, runs string) string {
	t.Helper()
	mgr, err := autostart.ForHost()
	if err != nil {
		t.Skip(err)
	}
	cfg := autostart.Config{Label: testLabel, Binary: "/bin/outpost", Home: runs, Scope: autostart.System, User: "root"}
	path, err := unitPath(mgr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	body, err := mgr.Render(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// A user job and a system job both loaded for the same home: one daemon holds
// the ports and the other crash-loops. Restarting either is a guess, and
// guessing the wrong one made an update wait on a daemon that never answered
// and roll back, so neither is touched and both are named.
func TestDaemonRestart_RefusesJobsAtBothScopes(t *testing.T) {
	stub := stubInit(t, true)
	sandboxSystemUnits(t)
	home := shortHome(t)
	mgr, cfg, userUnit := writeJob(t, home)
	systemUnit := writeSystemJob(t, home)

	_, err := restart(t, home)
	if err == nil {
		t.Fatal("restarted with a job at each scope")
	}
	wants := []string{userUnit, systemUnit, "nothing was"}
	for _, cmd := range mgr.DisableCommands(userUnit, cfg) {
		wants = append(wants, strings.Join(cmd, " "))
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

// update --self goes through the same lookup, and must refuse before it
// downloads or replaces anything.
func TestSelfJob_RefusesJobsAtBothScopes(t *testing.T) {
	stub := stubInit(t, true)
	sandboxSystemUnits(t)
	home := shortHome(t)
	_, _, userUnit := writeJob(t, home)
	systemUnit := writeSystemJob(t, home)

	a := &app{home: home, out: &strings.Builder{}, err: &strings.Builder{}}
	_, _, err := a.selfJob(testLabel)
	if err == nil || !strings.Contains(err.Error(), userUnit) || !strings.Contains(err.Error(), systemUnit) {
		t.Errorf("err = %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v", stub.commands())
	}
}

// Once the unwanted job is unloaded, its file can stay where it is: only the
// loaded job counts, so the lookup finds it again.
func TestFindDaemonJob_AnUnloadedSecondJobIsNoConflict(t *testing.T) {
	stub := stubInit(t, true)
	sandboxSystemUnits(t)
	home := shortHome(t)
	_, _, userUnit := writeJob(t, home)
	writeSystemJob(t, home)
	initProbe = func(argv []string) bool {
		stub.mu.Lock()
		defer stub.mu.Unlock()
		stub.asked = append(stub.asked, argv)
		// Only the user-scope probe answers yes: launchd's names the gui
		// domain, systemd's passes --user.
		joined := strings.Join(argv, " ")
		return strings.Contains(joined, "gui/") || strings.Contains(joined, "--user")
	}

	a := &app{home: home, out: &strings.Builder{}, err: &strings.Builder{}}
	job, err := a.findDaemonJob(testLabel, false)
	if err != nil {
		t.Fatal(err)
	}
	if job.unit != userUnit || !job.loaded {
		t.Errorf("found %s (loaded %v), want the loaded %s", job.unit, job.loaded, userUnit)
	}
}

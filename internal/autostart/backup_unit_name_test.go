package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #20: the systemd units carry Outpost's name, not a bare "backup" that
// another package could own, and the timer points at the service it sits
// beside.
func TestSystemd_BackupUnitsAreNamespaced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	b := baseSchedule()

	timer, err := systemd{}.BackupUnitPath(b)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(timer); got != "outpost-backup.timer" {
		t.Errorf("timer is %q; want outpost-backup.timer", got)
	}

	sys := b
	sys.Scope = System
	sys.User = "outpost"
	sysTimer, err := systemd{}.BackupUnitPath(sys)
	if err != nil {
		t.Fatal(err)
	}
	if sysTimer != "/etc/systemd/system/outpost-backup.timer" {
		t.Errorf("system timer is %q", sysTimer)
	}

	body, err := systemd{}.RenderBackupTimer(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "Unit=outpost-backup.service\n") {
		t.Errorf("timer does not trigger outpost-backup.service:\n%s", body)
	}
	svc := renderBackup(t, "linux", b)
	if !strings.Contains(svc, "SyslogIdentifier=outpost-backup\n") {
		t.Errorf("journal identifier is not outpost-backup:\n%s", svc)
	}

	for _, cmds := range [][][]string{
		BackupEnableCommands("linux", timer, b),
		BackupDisableCommands("linux", timer, b),
	} {
		last := cmds[len(cmds)-1]
		if got := last[len(last)-1]; got != "outpost-backup.timer" {
			t.Errorf("%v acts on %q; want outpost-backup.timer", last, got)
		}
	}

	paths, err := WriteBackupUnits("linux", b)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range paths {
		if !strings.HasPrefix(filepath.Base(p), "outpost-backup.") {
			t.Errorf("wrote %s; want an outpost-backup unit", p)
		}
	}
}

// Units an older Outpost wrote under the bare name are found so an upgrade can
// clear them, but only when they are Outpost's.
func TestLegacyBackupUnits_OnlyOutpostsOwn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	units := filepath.Join(dir, "systemd", "user")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	b := baseSchedule()

	if got, err := LegacyBackupUnits("linux", b); err != nil || len(got) != 0 {
		t.Fatalf("with nothing there: %v, %v", got, err)
	}

	ours, err := systemd{}.RenderBackup(b)
	if err != nil {
		t.Fatal(err)
	}
	theirs := "[Unit]\nDescription=Nightly database dump\n"
	if err := os.WriteFile(filepath.Join(units, "backup.service"), []byte(theirs), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := LegacyBackupUnits("linux", b); len(got) != 0 {
		t.Errorf("someone else's backup.service was claimed: %v", got)
	}

	if err := os.WriteFile(filepath.Join(units, "backup.service"), []byte(ours), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(units, "backup.timer"), []byte("[Unit]\nDescription=Outpost scheduled backup timer\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := LegacyBackupUnits("linux", b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("found %v; want the old timer and service", got)
	}

	if got, _ := LegacyBackupUnits("darwin", b); len(got) != 0 {
		t.Errorf("launchd was always namespaced, but found %v", got)
	}
}

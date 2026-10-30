package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// legacyBackupUnits plants the backup.service and backup.timer an Outpost
// before #20 wrote, and returns their directory.
func legacyBackupUnits(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("legacy unit names only ever existed on systemd")
	}
	units := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user")
	if err := os.MkdirAll(units, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, desc := range map[string]string{
		"backup.service": "Outpost scheduled backup",
		"backup.timer":   "Outpost scheduled backup timer",
	} {
		body := "[Unit]\nDescription=" + desc + "\n"
		if err := os.WriteFile(filepath.Join(units, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return units
}

func ranCommand(ran [][]string, want string) bool {
	for _, argv := range ran {
		if strings.Join(argv, " ") == want {
			return true
		}
	}
	return false
}

// Rescheduling with --enable stops the old timer and removes both old files,
// so the upgrade does not leave two timers running the same backup.
func TestBackupSchedule_EnableClearsLegacyUnits(t *testing.T) {
	stub := stubInit(t, false)
	units := legacyBackupUnits(t)

	out, stderr, err := runCLI(t, shortHome(t), "backup", "--schedule", "daily", "--enable")
	if err != nil {
		t.Fatalf("schedule: %v\n%s%s", err, out, stderr)
	}
	if !ranCommand(stub.commands(), "systemctl --user disable --now backup.timer") {
		t.Errorf("the old timer was not stopped: %v", stub.commands())
	}
	if !ranCommand(stub.commands(), "systemctl --user enable --now outpost-backup.timer") {
		t.Errorf("the new timer was not enabled: %v", stub.commands())
	}
	for _, name := range []string{"backup.service", "backup.timer"} {
		if _, err := os.Stat(filepath.Join(units, name)); !os.IsNotExist(err) {
			t.Errorf("%s is still there", name)
		}
	}
	for _, name := range []string{"outpost-backup.service", "outpost-backup.timer"} {
		if _, err := os.Stat(filepath.Join(units, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

// Without --enable Outpost runs no systemctl, so it leaves the old units in
// place and says how to clear them.
func TestBackupSchedule_WithoutEnableExplainsLegacyUnits(t *testing.T) {
	stub := stubInit(t, false)
	units := legacyBackupUnits(t)

	out, _, err := runCLI(t, shortHome(t), "backup", "--schedule", "daily")
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(stub.commands()) != 0 {
		t.Errorf("ran %v without --enable", stub.commands())
	}
	if _, err := os.Stat(filepath.Join(units, "backup.timer")); err != nil {
		t.Errorf("the old timer was removed while it may still be enabled: %v", err)
	}
	for _, want := range []string{"systemctl --user disable --now backup.timer", "rm ", "backup.service"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not mention %q:\n%s", want, out)
		}
	}
}

// --unschedule after an upgrade, before ever rescheduling, still finds the
// schedule it is being asked to remove.
func TestBackupUnschedule_RemovesLegacyUnits(t *testing.T) {
	stub := stubInit(t, false)
	units := legacyBackupUnits(t)

	if _, stderr, err := runCLI(t, shortHome(t), "backup", "--unschedule"); err != nil {
		t.Fatalf("unschedule: %v\n%s", err, stderr)
	}
	if !ranCommand(stub.commands(), "systemctl --user disable --now backup.timer") {
		t.Errorf("the old timer was not stopped: %v", stub.commands())
	}
	if _, err := os.Stat(filepath.Join(units, "backup.timer")); !os.IsNotExist(err) {
		t.Error("the old timer is still there")
	}
}

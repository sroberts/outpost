package autostart

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func baseSchedule() BackupSchedule {
	return BackupSchedule{
		Config:  baseConfig(),
		Cadence: Daily,
		Hour:    3,
		Minute:  30,
	}
}

func renderBackup(t *testing.T, goos string, b BackupSchedule) string {
	t.Helper()
	sched, err := SchedulerFor(goos)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sched.RenderBackup(b)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return out
}

func TestParseCadence(t *testing.T) {
	for _, ok := range []string{"hourly", "daily", "weekly", "DAILY"} {
		if _, err := ParseCadence(ok); err != nil {
			t.Errorf("ParseCadence(%q): %v", ok, err)
		}
	}
	if _, err := ParseCadence("fortnightly"); err == nil {
		t.Error("an unknown cadence was accepted")
	}
}

func TestSchedule_Validate(t *testing.T) {
	for name, mutate := range map[string]func(*BackupSchedule){
		"bad hour":    func(b *BackupSchedule) { b.Hour = 24 },
		"bad minute":  func(b *BackupSchedule) { b.Minute = -1 },
		"bad cadence": func(b *BackupSchedule) { b.Cadence = "never" },
		"no binary":   func(b *BackupSchedule) { b.Binary = "" },
	} {
		b := baseSchedule()
		mutate(&b)
		if err := b.Validate(); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
	if err := baseSchedule().Validate(); err != nil {
		t.Errorf("a valid schedule was rejected: %v", err)
	}
}

// The launchd job must run `outpost backup` on a calendar and must not be
// restarted when it finishes.
func TestLaunchd_BackupJob(t *testing.T) {
	out := renderBackup(t, "darwin", baseSchedule())

	for _, want := range []string{
		"<string>backup</string>",
		"<key>StartCalendarInterval</key>",
		"<key>Hour</key>",
		"<integer>3</integer>",
		"<key>Minute</key>",
		"<integer>30</integer>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plist missing %s:\n%s", want, out)
		}
	}
	// A backup is a one-shot, not a daemon.
	if strings.Contains(out, "<key>KeepAlive</key>") {
		t.Error("a scheduled backup must not be kept alive")
	}
	if !strings.Contains(out, "<key>RunAtLoad</key>\n\t<false/>") {
		t.Errorf("a scheduled backup must not run merely because it was loaded:\n%s", out)
	}
	if err := xml.Unmarshal([]byte(out), new(struct{})); err != nil {
		t.Errorf("plist is not well-formed XML: %v", err)
	}
}

// Hourly must not pin an hour, or it would fire once a day.
func TestLaunchd_HourlyOmitsTheHour(t *testing.T) {
	b := baseSchedule()
	b.Cadence = Hourly
	out := renderBackup(t, "darwin", b)

	if strings.Contains(out, "<key>Hour</key>") {
		t.Errorf("hourly pinned an hour, so it would run daily:\n%s", out)
	}
	if !strings.Contains(out, "<key>Minute</key>") {
		t.Error("hourly must still pin a minute")
	}
}

func TestLaunchd_WeeklyPinsAWeekday(t *testing.T) {
	b := baseSchedule()
	b.Cadence = Weekly
	if out := renderBackup(t, "darwin", b); !strings.Contains(out, "<key>Weekday</key>") {
		t.Errorf("weekly did not pin a weekday:\n%s", out)
	}
}

func TestSystemd_BackupServiceIsOneshot(t *testing.T) {
	out := renderBackup(t, "linux", baseSchedule())
	if !strings.Contains(out, "Type=oneshot") {
		t.Errorf("backup service is not oneshot:\n%s", out)
	}
	if strings.Contains(out, "Restart=") {
		t.Error("a scheduled backup must not be restarted when it exits")
	}
	if !strings.Contains(out, "backup") {
		t.Errorf("service does not invoke backup:\n%s", out)
	}
}

func TestSystemd_TimerCalendar(t *testing.T) {
	sd := systemd{}
	for _, tc := range []struct {
		cadence Cadence
		want    string
	}{
		{Daily, "OnCalendar=*-*-* 03:30:00"},
		{Hourly, "OnCalendar=*-*-* *:30:00"},
		{Weekly, "OnCalendar=Sun *-*-* 03:30:00"},
	} {
		b := baseSchedule()
		b.Cadence = tc.cadence
		out, err := sd.RenderBackupTimer(b)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, tc.want) {
			t.Errorf("%s: missing %q:\n%s", tc.cadence, tc.want, out)
		}
	}
}

// A backup missed because the machine was off should still run.
func TestSystemd_TimerIsPersistent(t *testing.T) {
	out, err := systemd{}.RenderBackupTimer(baseSchedule())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Persistent=true") {
		t.Errorf("a missed backup would be skipped entirely:\n%s", out)
	}
	if !strings.Contains(out, "WantedBy=timers.target") {
		t.Errorf("timer is not installed into timers.target:\n%s", out)
	}
}

// systemd needs two files; launchd needs one.
func TestWriteBackupUnits(t *testing.T) {
	for _, tc := range []struct {
		goos      string
		wantFiles int
	}{
		{"darwin", 1},
		{"linux", 2},
	} {
		dir := t.TempDir()
		t.Setenv("HOME", dir)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))

		paths, err := WriteBackupUnits(tc.goos, baseSchedule())
		if err != nil {
			t.Fatalf("%s: %v", tc.goos, err)
		}
		if len(paths) != tc.wantFiles {
			t.Errorf("%s: wrote %d files (%v); want %d", tc.goos, len(paths), paths, tc.wantFiles)
		}
		for _, p := range paths {
			if _, err := os.Stat(p); err != nil {
				t.Errorf("%s: %s not written: %v", tc.goos, p, err)
			}
		}
	}
}

// The scheduled job must not collide with the daemon's own unit.
func TestBackupLabel_DoesNotCollideWithTheDaemon(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		sched, _ := SchedulerFor(goos)
		daemonPath, err := sched.UnitPath(baseConfig())
		if err != nil {
			t.Fatal(err)
		}
		backupPath, err := sched.BackupUnitPath(baseSchedule())
		if err != nil {
			t.Fatal(err)
		}
		if daemonPath == backupPath {
			t.Errorf("%s: the backup job would overwrite the daemon's unit (%s)", goos, daemonPath)
		}
	}
}

func TestBackupCommands_BothScopes(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		for _, scope := range []Scope{User, System} {
			b := baseSchedule()
			b.Scope, b.User = scope, "me"
			sched, _ := SchedulerFor(goos)
			path, _ := sched.BackupUnitPath(b)

			if len(BackupEnableCommands(goos, path, b)) == 0 {
				t.Errorf("%s/%s: no enable commands", goos, scope)
			}
			if len(BackupDisableCommands(goos, path, b)) == 0 {
				t.Errorf("%s/%s: no disable commands", goos, scope)
			}
		}
	}
}

func TestDescribe_ReadsAsEnglish(t *testing.T) {
	b := baseSchedule()
	if got := b.Describe(); !strings.Contains(got, "03:30") || !strings.Contains(got, "every service") {
		t.Errorf("Describe = %q", got)
	}
	b.Service = "plumbline"
	if got := b.Describe(); !strings.Contains(got, "plumbline") {
		t.Errorf("Describe = %q", got)
	}
}

func TestSchedulerFor_UnsupportedPlatform(t *testing.T) {
	if _, err := SchedulerFor("plan9"); err == nil {
		t.Fatal("want an error for an unsupported platform")
	}
}

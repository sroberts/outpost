package autostart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Scheduled backups reuse the init system rather than growing a scheduler
// inside Outpost (spec §10, open question 2): launchd runs a calendar job,
// systemd a timer. `outpost backup` already does the work and reaches the
// daemon over its socket, so all either system has to do is invoke it.

// Cadence is how often a scheduled backup runs.
type Cadence string

const (
	Hourly Cadence = "hourly"
	Daily  Cadence = "daily"
	Weekly Cadence = "weekly"
)

// ParseCadence validates a cadence name.
func ParseCadence(s string) (Cadence, error) {
	switch Cadence(strings.ToLower(s)) {
	case Hourly:
		return Hourly, nil
	case Daily:
		return Daily, nil
	case Weekly:
		return Weekly, nil
	default:
		return "", fmt.Errorf("cadence must be hourly, daily or weekly; got %q", s)
	}
}

// BackupSchedule describes a recurring backup.
type BackupSchedule struct {
	Config
	Cadence Cadence
	// Hour and Minute are the time of day for daily and weekly runs.
	Hour, Minute int
	// Service is backed up alone; empty means every service.
	Service string
}

// Validate checks the schedule before anything is written.
func (b BackupSchedule) Validate() error {
	if err := b.Config.Validate(); err != nil {
		return err
	}
	if _, err := ParseCadence(string(b.Cadence)); err != nil {
		return err
	}
	if b.Hour < 0 || b.Hour > 23 {
		return fmt.Errorf("hour must be 0-23; got %d", b.Hour)
	}
	if b.Minute < 0 || b.Minute > 59 {
		return fmt.Errorf("minute must be 0-59; got %d", b.Minute)
	}
	return nil
}

// backupLabel is the job name for a scheduled backup, derived from the daemon's.
func backupLabel(label string) string { return label + ".backup" }

// backupUnit is the systemd name for a scheduled backup, without a suffix:
// "outpost-backup" from "com.sroberts.outpost". unitName keeps only a label's
// last segment, which for backupLabel would be the bare "backup" -- a name
// that says nothing about Outpost and can collide with another unit in
// /etc/systemd/system (#20).
func backupUnit(label string) string {
	return strings.TrimSuffix(unitName(label), ".service") + "-backup"
}

// legacyBackupUnit is the name systemd backups were written under before
// backupUnit ("backup" for the default label), kept so an upgrade can find and
// clear them. It is derived the way the old code derived it rather than
// hard-coded, so a non-default label finds its own old units.
func legacyBackupUnit(label string) string {
	return strings.TrimSuffix(unitName(backupLabel(label)), ".service")
}

// legacyMarkers are the exact Description lines an older Outpost wrote into
// its backup service and timer. Matching whole lines, not a substring, keeps
// clearing them from ever touching someone else's backup.service.
var legacyMarkers = []string{
	"Description=Outpost scheduled backup",
	"Description=Outpost scheduled backup timer",
}

func isLegacyOutpostUnit(body string) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		for _, m := range legacyMarkers {
			if line == m {
				return true
			}
		}
	}
	return false
}

// args are the command a scheduled run executes.
func (b BackupSchedule) args() []string {
	args := []string{b.Binary, "--home", b.Home, "backup"}
	if b.Service != "" {
		args = append(args, b.Service)
	}
	return args
}

// Describe renders the schedule in words, for confirmation output.
func (b BackupSchedule) Describe() string {
	what := "every service"
	if b.Service != "" {
		what = b.Service
	}
	switch b.Cadence {
	case Hourly:
		return fmt.Sprintf("back up %s every hour, at minute %d", what, b.Minute)
	case Weekly:
		return fmt.Sprintf("back up %s every Sunday at %02d:%02d", what, b.Hour, b.Minute)
	default:
		return fmt.Sprintf("back up %s every day at %02d:%02d", what, b.Hour, b.Minute)
	}
}

// --- launchd ---

// BackupUnitPath is where the scheduled-backup job belongs.
func (l launchd) BackupUnitPath(b BackupSchedule) (string, error) {
	cfg := b.Config
	cfg.Label = backupLabel(b.Label)
	return l.UnitPath(cfg)
}

// RenderBackup produces a launchd job that runs `outpost backup` on a calendar.
func (l launchd) RenderBackup(b BackupSchedule) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	sb.WriteString(`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` + "\n")
	sb.WriteString(`<plist version="1.0">` + "\n<dict>\n")

	fmt.Fprintf(&sb, "\t<key>Label</key>\n\t<string>%s</string>\n", plistEscape(backupLabel(b.Label)))

	sb.WriteString("\t<key>ProgramArguments</key>\n\t<array>\n")
	for _, arg := range b.args() {
		fmt.Fprintf(&sb, "\t\t<string>%s</string>\n", plistEscape(arg))
	}
	sb.WriteString("\t</array>\n")

	// StartCalendarInterval omits the fields that should not be pinned: an
	// absent key means "every value", which is how hourly is expressed.
	sb.WriteString("\t<key>StartCalendarInterval</key>\n\t<dict>\n")
	fmt.Fprintf(&sb, "\t\t<key>Minute</key>\n\t\t<integer>%d</integer>\n", b.Minute)
	if b.Cadence != Hourly {
		fmt.Fprintf(&sb, "\t\t<key>Hour</key>\n\t\t<integer>%d</integer>\n", b.Hour)
	}
	if b.Cadence == Weekly {
		sb.WriteString("\t\t<key>Weekday</key>\n\t\t<integer>0</integer>\n")
	}
	sb.WriteString("\t</dict>\n")

	// A backup is a one-shot: it must not be restarted when it exits.
	sb.WriteString("\t<key>RunAtLoad</key>\n\t<false/>\n")

	if b.Scope == System {
		fmt.Fprintf(&sb, "\t<key>UserName</key>\n\t<string>%s</string>\n", plistEscape(b.User))
	}
	fmt.Fprintf(&sb, "\t<key>WorkingDirectory</key>\n\t<string>%s</string>\n", plistEscape(b.Home))
	fmt.Fprintf(&sb, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n",
		plistEscape(filepath.Join(b.Home, "logs", "backup.out.log")))
	fmt.Fprintf(&sb, "\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n",
		plistEscape(filepath.Join(b.Home, "logs", "backup.err.log")))
	sb.WriteString("\t<key>ProcessType</key>\n\t<string>Background</string>\n")

	sb.WriteString("</dict>\n</plist>\n")
	return sb.String(), nil
}

// --- systemd ---

// BackupUnitPath is the timer's path; the service unit sits beside it.
func (sd systemd) BackupUnitPath(b BackupSchedule) (string, error) {
	daemon, err := sd.UnitPath(b.Config)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(daemon), backupUnit(b.Label)+".timer"), nil
}

// LegacyBackupUnits returns the units an older Outpost wrote for this
// schedule under the unnamespaced name, if they are still there. Only files
// Outpost wrote are returned: a backup.service that is not ours is left alone.
func LegacyBackupUnits(goos string, b BackupSchedule) ([]string, error) {
	if goos != "linux" {
		return nil, nil
	}
	daemon, err := systemd{}.UnitPath(b.Config)
	if err != nil {
		return nil, err
	}
	var found []string
	for _, suffix := range []string{".timer", ".service"} {
		p := filepath.Join(filepath.Dir(daemon), legacyBackupUnit(b.Label)+suffix)
		body, err := os.ReadFile(p)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			// Not being able to read it is not the same as it not being
			// there: a silently skipped timer would keep running beside the
			// new one.
			return nil, fmt.Errorf("check for an old backup unit at %s: %w", p, err)
		}
		if isLegacyOutpostUnit(string(body)) {
			found = append(found, p)
		}
	}
	return found, nil
}

// LegacyBackupDisableCommands stop the legacy timer before its files go.
func LegacyBackupDisableCommands(b BackupSchedule) [][]string {
	return systemdDisable(b.Scope, legacyBackupUnit(b.Label)+".timer")
}

// systemdDisable stops and deregisters a systemd unit in the given scope.
func systemdDisable(scope Scope, unit string) [][]string {
	if scope == System {
		return [][]string{{"sudo", "systemctl", "disable", "--now", unit}}
	}
	return [][]string{{"systemctl", "--user", "disable", "--now", unit}}
}

// RenderBackup produces the oneshot service that a systemd timer triggers.
func (sd systemd) RenderBackup(b BackupSchedule) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("[Unit]\n")
	sb.WriteString("Description=Outpost scheduled backup\n")
	sb.WriteString("\n[Service]\n")
	// Oneshot: it runs, finishes, and is not restarted.
	sb.WriteString("Type=oneshot\n")

	quoted := make([]string, 0, 4)
	for _, arg := range b.args() {
		quoted = append(quoted, systemdQuote(arg))
	}
	fmt.Fprintf(&sb, "ExecStart=%s\n", strings.Join(quoted, " "))
	fmt.Fprintf(&sb, "WorkingDirectory=%s\n", systemdQuote(b.Home))
	if b.Scope == System {
		fmt.Fprintf(&sb, "User=%s\n", systemdQuote(b.User))
	}
	sb.WriteString("StandardOutput=journal\nStandardError=journal\n")
	fmt.Fprintf(&sb, "SyslogIdentifier=%s\n", backupUnit(b.Label))
	return sb.String(), nil
}

// RenderBackupTimer produces the timer that drives the backup service.
func (sd systemd) RenderBackupTimer(b BackupSchedule) (string, error) {
	if err := b.Validate(); err != nil {
		return "", err
	}

	var sb strings.Builder
	sb.WriteString("[Unit]\n")
	sb.WriteString("Description=Outpost scheduled backup timer\n")
	sb.WriteString("\n[Timer]\n")
	fmt.Fprintf(&sb, "OnCalendar=%s\n", b.onCalendar())
	// Run a missed backup once the machine is up again, rather than skipping it.
	sb.WriteString("Persistent=true\n")
	// Spread the load so every timer on the box does not fire at once.
	sb.WriteString("RandomizedDelaySec=60\n")
	fmt.Fprintf(&sb, "Unit=%s.service\n", backupUnit(b.Label))

	sb.WriteString("\n[Install]\n")
	sb.WriteString("WantedBy=timers.target\n")
	return sb.String(), nil
}

// onCalendar renders the cadence as a systemd calendar expression.
func (b BackupSchedule) onCalendar() string {
	switch b.Cadence {
	case Hourly:
		return fmt.Sprintf("*-*-* *:%02d:00", b.Minute)
	case Weekly:
		return fmt.Sprintf("Sun *-*-* %02d:%02d:00", b.Hour, b.Minute)
	default:
		return fmt.Sprintf("*-*-* %02d:%02d:00", b.Hour, b.Minute)
	}
}

// BackupScheduler is the platform-specific half of scheduled backups.
type BackupScheduler interface {
	Manager
	BackupUnitPath(b BackupSchedule) (string, error)
	RenderBackup(b BackupSchedule) (string, error)
}

// SchedulerFor returns the scheduler for a GOOS value.
func SchedulerFor(goos string) (BackupScheduler, error) {
	switch goos {
	case "darwin":
		return launchd{}, nil
	case "linux":
		return systemd{}, nil
	default:
		return nil, fmt.Errorf("no scheduled-backup support for %s: use cron to run `outpost backup`", goos)
	}
}

// WriteBackupUnits writes everything a scheduled backup needs and returns the
// paths written. systemd needs a service and a timer; launchd needs one file.
func WriteBackupUnits(goos string, b BackupSchedule) ([]string, error) {
	sched, err := SchedulerFor(goos)
	if err != nil {
		return nil, err
	}
	timerPath, err := sched.BackupUnitPath(b)
	if err != nil {
		return nil, err
	}
	body, err := sched.RenderBackup(b)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(filepath.Dir(timerPath), 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", filepath.Dir(timerPath), err)
	}

	// On systemd the oneshot service lives beside the timer.
	if sd, ok := sched.(systemd); ok {
		servicePath := strings.TrimSuffix(timerPath, ".timer") + ".service"
		if err := os.WriteFile(servicePath, []byte(body), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", servicePath, err)
		}
		timerBody, err := sd.RenderBackupTimer(b)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(timerPath, []byte(timerBody), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", timerPath, err)
		}
		return []string{servicePath, timerPath}, nil
	}

	if err := os.WriteFile(timerPath, []byte(body), 0o644); err != nil {
		return nil, fmt.Errorf("write %s: %w", timerPath, err)
	}
	return []string{timerPath}, nil
}

// BackupEnableCommands are the commands that activate a scheduled backup.
func BackupEnableCommands(goos, timerPath string, b BackupSchedule) [][]string {
	if goos == "linux" {
		unit := backupUnit(b.Label) + ".timer"
		if b.Scope == System {
			return [][]string{
				{"sudo", "systemctl", "daemon-reload"},
				{"sudo", "systemctl", "enable", "--now", unit},
			}
		}
		return [][]string{
			{"systemctl", "--user", "daemon-reload"},
			{"systemctl", "--user", "enable", "--now", unit},
		}
	}
	if b.Scope == System {
		return [][]string{{"sudo", "launchctl", "load", "-w", timerPath}}
	}
	return [][]string{{"launchctl", "load", "-w", timerPath}}
}

// BackupDisableCommands stop and deregister a scheduled backup.
func BackupDisableCommands(goos, timerPath string, b BackupSchedule) [][]string {
	if goos == "linux" {
		return systemdDisable(b.Scope, backupUnit(b.Label)+".timer")
	}
	if b.Scope == System {
		return [][]string{{"sudo", "launchctl", "unload", "-w", timerPath}}
	}
	return [][]string{{"launchctl", "unload", "-w", timerPath}}
}

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/tsdetect"
)

// A unit file is readable by other local users, and systemd shows its
// environment to any of them, so the key must never be rendered into one.
func TestInstall_PrintLeavesTheAuthKeyOut(t *testing.T) {
	sandboxUnitDir(t)
	home := newHome(t)

	out, _, err := runCLI(t, home, "install", "--print", "--authkey", "tskey-test")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "tskey-test") || strings.Contains(out, "TS_AUTHKEY") {
		t.Errorf("the rendered unit carries the auth key:\n%s", out)
	}
}

// The key given to install goes to config.toml, and the unit written beside
// it carries no copy, whether given as a flag or found in the shell.
func TestInstall_AuthKeyGoesToConfigNotTheUnit(t *testing.T) {
	sandboxUnitDir(t)
	stubDetect(t, tsdetect.Result{})
	stubAnswers(t, nil)
	prev := initProbe
	initProbe = func([]string) bool { return false }
	t.Cleanup(func() { initProbe = prev })

	home := filepath.Join(t.TempDir(), "new-home")
	out, _, err := runCLI(t, home, "install", "--authkey", "tskey-flag")
	if err != nil {
		t.Fatal(err)
	}
	if got := authKeyIn(t, home); got != "tskey-flag" {
		t.Errorf("auth_key = %q; want the key given to install", got)
	}
	if strings.Contains(out, "tskey-flag") {
		t.Errorf("the key was echoed:\n%s", out)
	}
	unit := wroteUnit(t, out)
	if body, err := os.ReadFile(unit); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(body), "tskey-flag") || strings.Contains(string(body), "TS_AUTHKEY") {
		t.Errorf("the unit file carries the auth key:\n%s", body)
	}

	t.Setenv("TS_AUTHKEY", "tskey-ambient")
	home = filepath.Join(t.TempDir(), "new-home")
	out, _, err = runCLI(t, home, "install", "--no-tailscale")
	if err != nil {
		t.Fatal(err)
	}
	if body, err := os.ReadFile(wroteUnit(t, out)); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(body), "tskey-ambient") {
		t.Errorf("TS_AUTHKEY from the shell was copied into the unit file:\n%s", body)
	}
	if got := authKeyIn(t, home); got != "" {
		t.Errorf("TS_AUTHKEY from the shell was saved to config.toml")
	}
	if !strings.Contains(out, "outpost config --authkey") {
		t.Errorf("no pointer to config --authkey for the key in the shell:\n%s", out)
	}
}

var wroteLine = regexp.MustCompile(`(?m)^wrote (\S+) `)

// wroteUnit is the unit file install says it wrote.
func wroteUnit(t *testing.T, out string) string {
	t.Helper()
	m := wroteLine.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("install didn't say where it wrote the unit:\n%s", out)
	}
	return m[1]
}

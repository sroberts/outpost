package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/tsdetect"
)

// sandboxUnitDir keeps install's unit file out of the real LaunchAgents or
// systemd user directory.
func sandboxUnitDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("TS_AUTHKEY", "")
}

// Abandoning the question must not leave a new home looking like an old one,
// which would read as Tailscale on.
func TestInstall_AbandonedQuestionLeavesNewHomeOff(t *testing.T) {
	sandboxUnitDir(t)
	stubDetect(t, tsdetect.Result{})
	stubAnswers(t, ptr("a\nb\nc\n"))
	home := filepath.Join(t.TempDir(), "new-home")

	if _, _, err := runCLI(t, home, "install"); err == nil {
		t.Fatal("install accepted three unrecognised answers")
	}
	if explicit, on := setting(t, home); !explicit || on {
		t.Errorf("after an abandoned question: explicit=%v enabled=%v; want explicit off", explicit, on)
	}
}

// An auth key is only any use with Tailscale on, so a scripted install that
// passes one gets it on rather than a key that's silently ignored.
func TestInstall_AuthKeyMeansOn(t *testing.T) {
	sandboxUnitDir(t)
	stubDetect(t, tsdetect.Result{})
	stubAnswers(t, nil)

	home := filepath.Join(t.TempDir(), "new-home")
	out, _, err := runCLI(t, home, "install", "--authkey", "tskey-test")
	if err != nil {
		t.Fatal(err)
	}
	if explicit, on := setting(t, home); !explicit || !on {
		t.Errorf("--authkey on a new home: explicit=%v enabled=%v; want explicit on\n%s", explicit, on, out)
	}

	// --no-tailscale still wins, and says the key goes unused.
	home = filepath.Join(t.TempDir(), "new-home")
	out, _, err = runCLI(t, home, "install", "--authkey", "tskey-test", "--no-tailscale")
	if err != nil {
		t.Fatal(err)
	}
	if _, on := setting(t, home); on {
		t.Error("--no-tailscale with --authkey turned Tailscale on")
	}
	if !strings.Contains(out, "auth key won't be used") {
		t.Errorf("no note that the key goes unused:\n%s", out)
	}
}

func TestConfigInit_NewHomeIsOff(t *testing.T) {
	home := filepath.Join(t.TempDir(), "new-home")
	if _, _, err := runCLI(t, home, "config", "--init"); err != nil {
		t.Fatal(err)
	}
	if explicit, on := setting(t, home); !explicit || on {
		t.Errorf("config --init on a new home: explicit=%v enabled=%v; want explicit off", explicit, on)
	}

	// An existing home has been running with Tailscale on, and the starter
	// file records that.
	existing := newHome(t)
	if _, _, err := runCLI(t, existing, "config", "--init"); err != nil {
		t.Fatal(err)
	}
	if explicit, on := setting(t, existing); !explicit || !on {
		t.Errorf("config --init on an existing home: explicit=%v enabled=%v; want explicit on", explicit, on)
	}
}

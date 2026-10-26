package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/config"
	"github.com/sroberts/outpost/internal/tsdetect"
)

// stubDetect makes detection report res, and counts calls.
func stubDetect(t *testing.T, res tsdetect.Result) *int {
	t.Helper()
	prev := detectTailscale
	calls := 0
	detectTailscale = func(context.Context) tsdetect.Result { calls++; return res }
	t.Cleanup(func() { detectTailscale = prev })
	return &calls
}

// stubAnswers stands in for the install question's input. A nil answers
// means there is no terminal.
func stubAnswers(t *testing.T, answers *string) {
	t.Helper()
	prevIn, prevTTY := answerIn, answerIsTerminal
	if answers == nil {
		answerIn = strings.NewReader("")
		answerIsTerminal = func() bool { return false }
	} else {
		answerIn = strings.NewReader(*answers)
		answerIsTerminal = func() bool { return true }
	}
	t.Cleanup(func() { answerIn, answerIsTerminal = prevIn, prevTTY })
}

func ptr(s string) *string { return &s }

// setting reads back what config.toml says: whether it sets tailscale.enabled,
// and the effective value.
func setting(t *testing.T, home string) (explicit, enabled bool) {
	t.Helper()
	path := filepath.Join(home, "config.toml")
	explicit, err := config.HasTailscaleSetting(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return explicit, cfg.TailscaleEnabled()
}

func TestFirstRun_NewHomeGetsTailscaleOff(t *testing.T) {
	stubDetect(t, tsdetect.Result{Found: true, Tailnet: "tail1234.ts.net"})
	home := filepath.Join(t.TempDir(), "new-home")

	_, stderr, err := runCLI(t, home, "apply")
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if explicit, on := setting(t, home); !explicit || on {
		t.Errorf("new home: explicit=%v enabled=%v; want an explicit false", explicit, on)
	}
	for _, want := range []string{"Tailscale is off", "tail1234.ts.net", "outpost config --tailscale on"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("first-run notice lacks %q:\n%s", want, stderr)
		}
	}

	// Only the first command says anything.
	_, stderr, err = runCLI(t, home, "apply")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "Tailscale") {
		t.Errorf("second run repeated the first-run notice:\n%s", stderr)
	}
}

func TestFirstRun_NoMentionWhenNotDetected(t *testing.T) {
	stubDetect(t, tsdetect.Result{})
	home := filepath.Join(t.TempDir(), "new-home")
	_, stderr, err := runCLI(t, home, "apply")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr, "config --tailscale on") {
		t.Errorf("suggested turning Tailscale on with none detected:\n%s", stderr)
	}
	if _, on := setting(t, home); on {
		t.Error("Tailscale on for a new home")
	}
}

// A home that already exists keeps the built-in default, and nothing is
// written to it.
func TestFirstRun_ExistingHomeLeftAlone(t *testing.T) {
	calls := stubDetect(t, tsdetect.Result{Found: true})
	home := newHome(t)
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "config.toml")); !os.IsNotExist(err) {
		t.Error("config.toml was written into an existing home")
	}
	if *calls != 0 {
		t.Errorf("detection ran %d times for an existing home", *calls)
	}
}

// chooseApp builds an app over home as install would see it.
func chooseApp(t *testing.T, home string) (*app, *bytes.Buffer) {
	t.Helper()
	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	return &app{home: home, out: &out, err: &out, config: cfg}, &out
}

func TestChooseTailscale_Flags(t *testing.T) {
	stubDetect(t, tsdetect.Result{})
	stubAnswers(t, ptr("n\n"))
	for _, on := range []bool{true, false} {
		home := newHome(t)
		a, _ := chooseApp(t, home)
		if err := a.chooseTailscale(on, !on, false, false); err != nil {
			t.Fatal(err)
		}
		if explicit, got := setting(t, home); !explicit || got != on {
			t.Errorf("flag %v: explicit=%v enabled=%v", on, explicit, got)
		}
	}
}

func TestChooseTailscale_ExplicitSettingIsKept(t *testing.T) {
	calls := stubDetect(t, tsdetect.Result{Found: true})
	stubAnswers(t, ptr("y\n"))
	home := newHome(t)
	off := false
	if err := config.Save(filepath.Join(home, "config.toml"), config.Config{Tailscale: config.Tailscale{Enabled: &off}}); err != nil {
		t.Fatal(err)
	}
	a, out := chooseApp(t, home)
	if err := a.chooseTailscale(false, false, false, true); err != nil {
		t.Fatal(err)
	}
	if _, on := setting(t, home); on {
		t.Error("an explicit off was changed")
	}
	if *calls != 0 || strings.Contains(out.String(), "?") {
		t.Errorf("asked although the home already has a setting:\n%s", out)
	}
}

func TestChooseTailscale_Asks(t *testing.T) {
	cases := []struct {
		name     string
		answers  string
		tsnetDir bool
		want     bool
	}{
		{"yes", "y\n", false, true},
		{"YES", "YES\n", false, true},
		{"no", "n\n", true, false},
		{"default no", "\n", false, false},
		{"default yes with tsnet state", "\n", true, true},
		{"eof takes the default", "", true, true},
		{"re-asks", "maybe\nyes\n", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stubDetect(t, tsdetect.Result{Found: true, Tailnet: "tail1234.ts.net"})
			stubAnswers(t, ptr(c.answers))
			home := newHome(t)
			if c.tsnetDir {
				if err := os.MkdirAll(filepath.Join(home, "tsnet"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			a, out := chooseApp(t, home)
			if err := a.chooseTailscale(false, false, false, false); err != nil {
				t.Fatal(err)
			}
			if explicit, got := setting(t, home); !explicit || got != c.want {
				t.Errorf("explicit=%v enabled=%v; want explicit %v", explicit, got, c.want)
			}
			if !strings.Contains(out.String(), "Tailscale is running here, tailnet tail1234.ts.net") {
				t.Errorf("didn't report what detection found:\n%s", out)
			}
			wantHint := "[y/N]"
			if c.tsnetDir {
				wantHint = "[Y/n]"
			}
			if !strings.Contains(out.String(), wantHint) {
				t.Errorf("prompt lacks %s:\n%s", wantHint, out)
			}
		})
	}
}

func TestChooseTailscale_GivesUpAfterThreeBadAnswers(t *testing.T) {
	stubDetect(t, tsdetect.Result{})
	stubAnswers(t, ptr("a\nb\nc\ny\n"))
	home := newHome(t)
	a, _ := chooseApp(t, home)
	if err := a.chooseTailscale(false, false, false, false); err == nil {
		t.Error("accepted after three unrecognised answers")
	}
	if explicit, _ := setting(t, home); explicit {
		t.Error("wrote a setting without an answer")
	}
}

func TestChooseTailscale_NoTerminal(t *testing.T) {
	stubDetect(t, tsdetect.Result{Found: true})
	stubAnswers(t, nil)

	// A new home is a new install: off.
	fresh := newHome(t)
	a, _ := chooseApp(t, fresh)
	if err := a.chooseTailscale(false, false, true, false); err != nil {
		t.Fatal(err)
	}
	if explicit, on := setting(t, fresh); !explicit || on {
		t.Errorf("new home, no terminal: explicit=%v enabled=%v; want explicit off", explicit, on)
	}

	// An existing home keeps running as it has, and nothing is written.
	existing := newHome(t)
	a, out := chooseApp(t, existing)
	if err := a.chooseTailscale(false, false, false, false); err != nil {
		t.Fatal(err)
	}
	if explicit, on := setting(t, existing); explicit || !on {
		t.Errorf("existing home, no terminal: explicit=%v enabled=%v; want it untouched", explicit, on)
	}
	if !strings.Contains(out.String(), "--no-tailscale") {
		t.Errorf("didn't say how to record a choice:\n%s", out)
	}
}

func TestInstall_TailscaleFlagsConflict(t *testing.T) {
	home := filepath.Join(t.TempDir(), "new-home")
	_, _, err := runCLI(t, home, "install", "--tailscale", "--no-tailscale")
	if err == nil || !strings.Contains(err.Error(), "can't be used together") {
		t.Errorf("both flags: %v", err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Error("a rejected install created the home")
	}
}

func TestInstall_PrintWritesNoSetting(t *testing.T) {
	stubDetect(t, tsdetect.Result{})
	home := filepath.Join(t.TempDir(), "new-home")
	if _, _, err := runCLI(t, home, "install", "--print", "--no-tailscale"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Error("install --print created the home")
	}
}

func TestConfigCmd_Tailscale(t *testing.T) {
	home := newHome(t)
	if err := os.WriteFile(filepath.Join(home, "config.toml"),
		[]byte("[tailscale]\nauth_key = \"tskey-keep\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, home, "config", "--tailscale", "off")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Tailscale: off") {
		t.Errorf("output = %q", out)
	}
	if explicit, on := setting(t, home); !explicit || on {
		t.Errorf("after off: explicit=%v enabled=%v", explicit, on)
	}
	cfg, _ := config.Load(filepath.Join(home, "config.toml"))
	if cfg.Tailscale.AuthKey != "tskey-keep" {
		t.Errorf("auth_key lost: %q", cfg.Tailscale.AuthKey)
	}

	// A manifest written with Tailscale off has no tailscale_name; turning it
	// on should say so before a restart fails on it.
	body := "name = \"alpha\"\nruntime = \"process\"\nbinary = \"/bin/sleep\"\nport = 19555\nstorage = \"" +
		filepath.Join(home, "data", "alpha") + "\"\n"
	if err := os.WriteFile(filepath.Join(home, "services", "alpha.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, home, "config", "--tailscale", "ON")
	if err != nil {
		t.Fatal(err)
	}
	if _, on := setting(t, home); !on {
		t.Error("--tailscale ON didn't turn it on")
	}
	if !strings.Contains(out, "warning") || !strings.Contains(out, "alpha") {
		t.Errorf("no warning about alpha's missing tailscale_name:\n%s", out)
	}

	if _, _, err := runCLI(t, home, "config", "--tailscale", "maybe"); err == nil {
		t.Error("--tailscale maybe was accepted")
	}
	if _, _, err := runCLI(t, home, "config", "--tailscale", "on", "--init"); err == nil {
		t.Error("--tailscale with --init was accepted")
	}
}

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/applog"
)

func writeStamped(t *testing.T, home, service string, lines ...string) {
	t.Helper()
	dir := filepath.Join(home, "logs")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	var b strings.Builder
	for i, l := range lines {
		stream := applog.Stdout
		if strings.HasPrefix(l, "!") {
			stream, l = applog.Stderr, l[1:]
		}
		b.Write(applog.Format(at.Add(time.Duration(i)*time.Second), stream, l))
	}
	if err := os.WriteFile(filepath.Join(dir, service+".log"), []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestCLI_LogsShowsLevelsAndFilters(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))
	writeStamped(t, home, "alpha", "booting", `!{"level":"error","msg":"db locked","table":"users"}`, "level=warn msg=slow ms=812", "!plain stderr")

	out, _, err := runCLI(t, home, "logs", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"booting", "ERRO db locked table=users", "WARN slow ms=812", "err  plain stderr"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	out, _, err = runCLI(t, home, "logs", "alpha", "--level", "warn")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "booting") || !strings.Contains(out, "db locked") || !strings.Contains(out, "slow") {
		t.Errorf("--level warn:\n%s", out)
	}

	out, _, err = runCLI(t, home, "logs", "alpha", "--stream", "err", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if out != "{\"level\":\"error\",\"msg\":\"db locked\",\"table\":\"users\"}\nplain stderr\n" {
		t.Errorf("--stream err --raw:\n%q", out)
	}

	out, _, err = runCLI(t, home, "logs", "alpha", "-n", "1", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var rec applog.Record
	if err := json.Unmarshal([]byte(out), &rec); err != nil || rec.Message != "plain stderr" || rec.Stream != "err" || rec.Service != "alpha" {
		t.Errorf("-n 1 --json = %q (%v)", out, err)
	}

	if _, _, err := runCLI(t, home, "logs", "alpha", "--level", "loud"); err == nil {
		t.Error("an unknown level was accepted")
	}
}

func TestCLI_LogsAllInterleaves(t *testing.T) {
	home := newHome(t)
	writeManifest(t, home, "alpha", freePort(t))
	writeManifest(t, home, "beta", freePort(t))
	writeStamped(t, home, "alpha", "a-first")
	writeStamped(t, home, "beta", "b-first", "b-second")

	out, _, err := runCLI(t, home, "logs", "--all", "--raw")
	if err != nil {
		t.Fatal(err)
	}
	if out != "alpha: a-first\nbeta: b-first\nbeta: b-second\n" {
		t.Errorf("--all:\n%q", out)
	}
	if _, _, err := runCLI(t, home, "logs", "alpha", "--all"); err == nil {
		t.Error("a name and --all together were accepted")
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.Local)
	cases := map[string]time.Time{
		"10m":                  now.Add(-10 * time.Minute),
		"2d":                   now.Add(-48 * time.Hour),
		"2026-11-18T09:00":     time.Date(2026, 11, 18, 9, 0, 0, 0, time.Local),
		"2026-11-18T09:00:00Z": time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC),
	}
	for in, want := range cases {
		got, err := parseSince(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseSince("yesterday", now); err == nil {
		t.Error("parseSince accepted yesterday")
	}
}

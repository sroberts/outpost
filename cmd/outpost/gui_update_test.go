package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

// updatesFor is the updater as `serve` builds it, for a home.
func updatesFor(home string) daemonUpdates {
	return daemonUpdates{&app{home: home, out: io.Discard, err: io.Discard}}
}

// The whole path the portal takes, against a real daemon and a real process:
// what `outpost update` proves, but reached the way the gateway reaches it.
func TestDaemonUpdates_InstallsAndRestarts(t *testing.T) {
	home, c, original := daemonHome(t)
	// fakeservice again, so the new "release" stays healthy; the marker is a
	// comment on the end of the file, which an ELF binary doesn't mind.
	useFakeGitHub(t, fakeGitHub(t, append(append([]byte{}, original...), []byte("new")...), "", true))
	u := updatesFor(home)

	check, err := u.Latest(context.Background(), "svc")
	if err != nil {
		t.Fatal(err)
	}
	if !check.Newer || check.Latest != "v1.2.3" {
		t.Fatalf("check = %+v; want v1.2.3 offered", check)
	}

	before, err := c.Status("svc")
	if err != nil {
		t.Fatal(err)
	}
	out, err := u.Update("svc")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, out)
	}
	for _, want := range []string{"backed up svc", "running v1.2.3 and healthy (was v1.0.0)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "svc.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Release != "v1.2.3" {
		t.Errorf("manifest records %q", svc.Release)
	}
	after, err := c.Status("svc")
	if err != nil {
		t.Fatal(err)
	}
	if after.InstanceID == before.InstanceID || after.Status != "running" {
		t.Errorf("status = %+v; want a new running instance", after)
	}

	check, err = u.Latest(context.Background(), "svc")
	if err != nil {
		t.Fatal(err)
	}
	if check.Newer {
		t.Errorf("after updating, check = %+v; want nothing to offer", check)
	}
}

func TestDaemonUpdates_RollsBackABadRelease(t *testing.T) {
	home, c, original := daemonHome(t)
	path := filepath.Join(home, "services", "svc.toml")
	before := readFile(t, path)
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 1\n"), "", true))

	// The default wait is 30 seconds, and a process that exits at once fails
	// long before it runs out.
	start := time.Now()
	out, err := updatesFor(home).Update("svc")
	if err == nil {
		t.Fatalf("a broken release succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "rolled back to v1.0.0, which is running and healthy") {
		t.Errorf("err = %v (after %s)", err, time.Since(start))
	}
	if !bytes.Equal([]byte(readFile(t, filepath.Join(home, "bin", "svc"))), original) {
		t.Error("the old binary was not put back")
	}
	if got := readFile(t, path); got != before {
		t.Errorf("manifest not restored:\n%s", got)
	}
	if st, err := c.Status("svc"); err != nil || st.Status != "running" {
		t.Errorf("status = %+v, %v", st, err)
	}
}

func TestDaemonUpdates_LatestOffersOnlyWhatUpdateWouldDo(t *testing.T) {
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true)) // latest is v1.2.3
	ctx := context.Background()

	for name, tc := range map[string]struct {
		release string
		newer   bool
		note    string
	}{
		"older":             {"v1.0.0", true, ""},
		"same":              {"v1.2.3", false, ""},
		"same without a v":  {"1.2.3", false, ""},
		"ahead of latest":   {"v2.0.0-rc1", false, "Newer than the latest"},
		"no version to say": {"nightly", true, ""},
	} {
		home := t.TempDir()
		declareInstalled(t, home, "svc", []byte("old"), freePort(t), tc.release)
		check, err := updatesFor(home).Latest(ctx, "svc")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if check.Newer != tc.newer || !strings.Contains(check.Note, tc.note) {
			t.Errorf("%s: check = %+v", name, check)
		}
	}

	// One the update would refuse isn't offered.
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	body := strings.Replace(readFile(t, path), filepath.Join(home, "bin", "svc"), "/usr/bin/true", 1)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	check, err := updatesFor(home).Latest(ctx, "svc")
	if err != nil {
		t.Fatal(err)
	}
	if check.Newer || !strings.Contains(check.Note, "isn't under Outpost's bin directory") {
		t.Errorf("binary elsewhere: check = %+v", check)
	}
}

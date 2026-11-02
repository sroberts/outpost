package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A service with no lock file yet has nothing for update --self to hold, so its
// update has to notice that one is running: the restart would cut it short.
func TestUpdate_RefusedWhileOutpostUpdates(t *testing.T) {
	home := t.TempDir()
	original := []byte("#!/bin/sh\necho old\n")
	declareInstalled(t, home, "svc", original, freePort(t), "v1.0.0")
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\necho new\n"), "", true))

	unlock, err := lockFile(selfUpdateLock(home))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	_, _, err = runCLI(t, home, "update", "svc")
	if err == nil || !strings.Contains(err.Error(), "Outpost itself is being updated") {
		t.Fatalf("err = %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, "bin", "svc"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Error("the binary was replaced")
	}
}

// The lock file outlives the update that made it, and means nothing once free.
func TestUpdate_FinishedSelfUpdateDoesNotHoldItUp(t *testing.T) {
	home := t.TempDir()
	declareInstalled(t, home, "svc", []byte("#!/bin/sh\necho old\n"), freePort(t), "v1.0.0")
	release := []byte("#!/bin/sh\necho new\n")
	useFakeGitHub(t, fakeGitHub(t, release, "", true))

	unlock, err := lockFile(selfUpdateLock(home))
	if err != nil {
		t.Fatal(err)
	}
	unlock()

	if stdout, stderr, err := runCLI(t, home, "update", "svc"); err != nil {
		t.Fatalf("update: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}
	if !bytes.Equal([]byte(readFile(t, filepath.Join(home, "bin", "svc"))), release) {
		t.Error("the binary is not the release's")
	}
}

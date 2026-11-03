package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addsFor(home string) daemonAdds {
	return daemonAdds{&app{home: home, out: io.Discard, err: io.Discard}}
}

// The portal's preview is `add --print`: it says what would be installed and
// writes nothing.
func TestDaemonAdds_PreviewWritesNothing(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 0\n"), helloDescriptor, true))
	out, err := addsFor(home).Preview(context.Background(), "o/r")
	if err != nil {
		t.Fatalf("preview: %v\n%s", err, out)
	}
	if !strings.Contains(out, `name`) || !strings.Contains(out, "would install") {
		t.Errorf("preview doesn't show the manifest and asset:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "services", "hello.toml")); !os.IsNotExist(err) {
		t.Errorf("preview wrote a manifest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "hello")); !os.IsNotExist(err) {
		t.Errorf("preview installed a binary: %v", err)
	}
}

// Add installs and declares the service, and never replaces one that's
// already declared: the portal has no --force.
func TestDaemonAdds_AddInstallsAndNeverOverwrites(t *testing.T) {
	home := t.TempDir()
	useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 0\n"), helloDescriptor, true))
	adds := addsFor(home)
	out, err := adds.Add("o/r")
	if err != nil {
		t.Fatalf("add: %v\n%s", err, out)
	}
	path := filepath.Join(home, "services", "hello.toml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("no manifest written: %v\n%s", err, out)
	}
	if !strings.Contains(out, "wrote "+path) {
		t.Errorf("output doesn't say what was written:\n%s", out)
	}
	before, _ := os.ReadFile(path)

	if out, err := adds.Add("o/r"); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("adding it again: err %v, want already exists\n%s", err, out)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("adding it again changed the manifest")
	}
}

// The gateway only passes owner/name, but nothing that could be read as a flag
// or a service name gets through here either.
func TestDaemonAdds_RefusesWhatIsntARepository(t *testing.T) {
	home := t.TempDir()
	adds := addsFor(home)
	for _, repo := range []string{"--force", "-x/r", "hello", ""} {
		if _, err := adds.Add(repo); err == nil {
			t.Errorf("Add(%q) went ahead", repo)
		}
		if _, err := adds.Preview(context.Background(), repo); err == nil {
			t.Errorf("Preview(%q) went ahead", repo)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(home, "services")); len(entries) != 0 {
		t.Errorf("refused adds wrote %d manifest(s)", len(entries))
	}
}

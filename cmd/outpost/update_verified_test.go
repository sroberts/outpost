package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// markVerified records in a declared service's manifest that its installed
// release was checksum verified, as `add` or an update would have.
func markVerified(t *testing.T, path string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString("verified       = true\n"); err != nil {
		t.Fatal(err)
	}
}

func loadVerified(t *testing.T, path string) bool {
	t.Helper()
	svc, err := manifest.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return svc.Verified
}

func TestRenderManifest_VerifiedRoundTrips(t *testing.T) {
	for _, verified := range []bool{true, false} {
		svc := &manifest.Service{Name: "svc", Runtime: "process", Binary: "/bin/true", Port: 8080,
			Storage: "/tmp/svc", Healthcheck: "/up", RestartPolicy: "on-failure",
			Source: "o/r", Release: "v1", Verified: verified}
		body := renderManifest(svc)
		if !verified && strings.Contains(body, "verified") {
			t.Errorf("an unverified manifest mentions verified:\n%s", body)
		}
		path := filepath.Join(t.TempDir(), "svc.toml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := loadVerified(t, path); got != verified {
			t.Errorf("verified = %t after a round trip; want %t\n%s", got, verified, body)
		}
	}
}

func TestAdd_RecordsWhetherTheReleaseWasVerified(t *testing.T) {
	for _, sums := range []bool{true, false} {
		home := t.TempDir()
		useFakeGitHub(t, fakeGitHub(t, []byte("#!/bin/sh\nexit 0\n"), helloDescriptor, sums))
		if _, stderr, err := runCLI(t, home, "add", "https://github.com/o/r"); err != nil {
			t.Fatalf("add: %v\n%s", err, stderr)
		}
		if got := loadVerified(t, filepath.Join(home, "services", "hello.toml")); got != sums {
			t.Errorf("with checksums %t: verified = %t", sums, got)
		}
	}
}

// A release that stops publishing checksums must not quietly replace one that
// was verified: that is what a tampered release would look like too.
func TestUpdate_RefusesAnUnverifiedReleaseOverAVerifiedOne(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	markVerified(t, path)
	before := readFile(t, path)
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", false))

	_, _, err := runCLI(t, home, "update", "svc")
	if err == nil || !strings.Contains(err.Error(), "--allow-unverified") {
		t.Fatalf("err = %v; want a refusal pointing at --allow-unverified", err)
	}
	if got := readFile(t, filepath.Join(home, "bin", "svc")); got != "old" {
		t.Errorf("binary = %q; want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(home, "bin", "svc.previous")); !os.IsNotExist(err) {
		t.Errorf("svc.previous exists after a refused update (%v)", err)
	}
	if got := readFile(t, path); got != before {
		t.Errorf("manifest changed:\n%s", got)
	}
}

func TestUpdate_AllowUnverifiedRecordsItIsNoLongerVerified(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	markVerified(t, path)
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", false))

	if _, stderr, err := runCLI(t, home, "update", "svc", "--allow-unverified"); err != nil {
		t.Fatalf("update: %v\n%s", err, stderr)
	}
	if got := readFile(t, filepath.Join(home, "bin", "svc")); got != "new" {
		t.Errorf("binary = %q; want the release's", got)
	}
	if loadVerified(t, path) {
		t.Errorf("manifest still says verified:\n%s", readFile(t, path))
	}
}

func TestUpdate_UnverifiedOverUnverifiedProceeds(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", false))

	_, stderr, err := runCLI(t, home, "update", "svc")
	if err != nil {
		t.Fatalf("update: %v\n%s", err, stderr)
	}
	if got := readFile(t, filepath.Join(home, "bin", "svc")); got != "new" {
		t.Errorf("binary = %q; want the release's", got)
	}
	if !strings.Contains(stderr, "installed unverified") {
		t.Errorf("stderr does not warn it is unverified:\n%s", stderr)
	}
	// The test's name is in the manifest's paths, so look for the key itself.
	if body := readFile(t, path); strings.Contains(body, "\nverified") {
		t.Errorf("manifest records verified for an unverified release:\n%s", body)
	}
}

func TestUpdate_VerifiedReleaseIsRecorded(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", true))

	if _, stderr, err := runCLI(t, home, "update", "svc"); err != nil {
		t.Fatalf("update: %v\n%s", err, stderr)
	}
	if !loadVerified(t, path) {
		t.Errorf("manifest does not record verified:\n%s", readFile(t, path))
	}
}

func TestSetVerified(t *testing.T) {
	cases := []struct {
		name, in string
		verified bool
		want     string
	}{
		{"added before a table", "name = \"x\"\n[env]\nA = \"1\"\n", true, "name = \"x\"\nverified       = true\n[env]\nA = \"1\"\n"},
		{"replaced", "verified = true # was\nname = \"x\"\n", false, "verified       = false  # was\nname = \"x\"\n"},
		{"absent stays absent", "name = \"x\"\n", false, "name = \"x\"\n"},
		{"no trailing newline", "name = \"x\"", true, "name = \"x\"\nverified       = true\n"},
	}
	for _, tc := range cases {
		got := string(setVerified([]byte(tc.in), tc.verified))
		if got != tc.want {
			t.Errorf("%s: got %q; want %q", tc.name, got, tc.want)
		}
		if err := checkVerified([]byte(got), tc.verified); err != nil {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}

// The portal runs the same update, and its refusal has to reach the page as
// the error, which is what the gateway shows on a failed update.
func TestDaemonUpdates_RefusesAnUnverifiedReleaseOverAVerifiedOne(t *testing.T) {
	home := t.TempDir()
	path := declareInstalled(t, home, "svc", []byte("old"), freePort(t), "v1.0.0")
	markVerified(t, path)
	useFakeGitHub(t, fakeGitHub(t, []byte("new"), "", false))

	_, err := updatesFor(home).Update("svc")
	if err == nil || !strings.Contains(err.Error(), "checksum verified") {
		t.Fatalf("err = %v; want the refusal", err)
	}
	if got := readFile(t, filepath.Join(home, "bin", "svc")); got != "old" {
		t.Errorf("binary = %q; want it untouched", got)
	}
}

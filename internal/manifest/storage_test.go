package manifest

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_Env(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envsvc.toml")
	body := `
name    = "envsvc"
binary  = "/usr/bin/true"
port    = 9000
storage = "/tmp/envsvc"
tailscale_name = "envsvc"
env = { LOG_LEVEL = "info", MODE = "test" }
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := svc.Env["LOG_LEVEL"]; got != "info" {
		t.Errorf("Env[LOG_LEVEL] = %q; want %q", got, "info")
	}
	if got := svc.Env["MODE"]; got != "test" {
		t.Errorf("Env[MODE] = %q; want %q", got, "test")
	}
}

// Filename minus extension is the service name unless the manifest overrides it.
func TestLoad_NameDefaultsToFilename(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plumbline.toml")
	body := `
binary  = "/usr/bin/true"
port    = 9001
storage = "/tmp/plumbline"
tailscale_name = "plumbline"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if svc.Name != "plumbline" {
		t.Errorf("Name = %q; want %q (derived from filename)", svc.Name, "plumbline")
	}
}

func TestEnsureStorage_CreatesDir0700(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nested", "data")
	svc := &Service{Name: "a", Storage: target}

	if err := svc.EnsureStorage(); err != nil {
		t.Fatalf("EnsureStorage: %v", err)
	}
	fi, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat after EnsureStorage: %v", err)
	}
	if !fi.IsDir() {
		t.Fatalf("%s is not a directory", target)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("perm = %#o; want 0700", perm)
	}
}

func TestEnsureStorage_Idempotent(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "data")
	svc := &Service{Name: "a", Storage: target}
	if err := svc.EnsureStorage(); err != nil {
		t.Fatalf("first EnsureStorage: %v", err)
	}
	if err := svc.EnsureStorage(); err != nil {
		t.Fatalf("second EnsureStorage: %v", err)
	}
}

func TestEnsureStorage_ErrorsWhenPathIsFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "afile")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	svc := &Service{Name: "a", Storage: target}
	if err := svc.EnsureStorage(); err == nil {
		t.Fatal("EnsureStorage on a regular file: want error, got nil")
	}
}

// ExpandStorage must not mutate the manifest it reads from.
func TestExpandStorage_DoesNotMutate(t *testing.T) {
	svc := &Service{Storage: "~/data"}
	first := svc.ExpandStorage()
	if svc.Storage != "~/data" {
		t.Errorf("ExpandStorage mutated Storage to %q", svc.Storage)
	}
	if second := svc.ExpandStorage(); second != first {
		t.Errorf("ExpandStorage not idempotent: %q then %q", first, second)
	}
}

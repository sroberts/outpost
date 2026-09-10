package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

func TestAdd_WritesAValidManifest(t *testing.T) {
	home := newHome(t)

	out, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep", "--args", "300", "--port", "9310")
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if !strings.Contains(out, "wrote") {
		t.Errorf("unexpected output:\n%s", out)
	}

	svc, err := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatalf("generated manifest does not load: %v", err)
	}
	if svc.Name != "demo" || svc.Port != 9310 || svc.Binary != "/bin/sleep" {
		t.Errorf("unexpected manifest: %+v", svc)
	}
	if len(svc.Args) != 1 || svc.Args[0] != "300" {
		t.Errorf("args = %v; want [300]", svc.Args)
	}
	// The generated file must pass the same validation as a hand-written one.
	if errs := manifest.ValidateAll([]*manifest.Service{svc}); len(errs) != 0 {
		t.Errorf("generated manifest fails validation: %v", errs)
	}
	// Storage must exist so the service can write immediately.
	if fi, err := os.Stat(svc.ExpandStorage()); err != nil || !fi.IsDir() {
		t.Errorf("storage not created: %v", err)
	}
}

func TestAdd_AllocatesAFreePortWhenNotGiven(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatalf("add: %v", err)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Port <= 0 {
		t.Errorf("port = %d; want an allocated one", svc.Port)
	}
}

func TestAdd_RefusesToClobberWithoutForce(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/echo"); err == nil {
		t.Fatal("add over an existing manifest: want error, got nil")
	}
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/echo", "--force"); err != nil {
		t.Errorf("--force should overwrite: %v", err)
	}
}

// A new service that collides with an existing one must be caught before the
// file is written, not at the next apply.
func TestAdd_RejectsCollisionsBeforeWriting(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "first", "--binary", "/bin/sleep", "--port", "9320"); err != nil {
		t.Fatalf("runCLI: %v", err)
	}

	_, _, err := runCLI(t, home, "add", "second", "--binary", "/bin/sleep", "--port", "9320")
	if err == nil {
		t.Fatal("port collision was accepted")
	}
	if !strings.Contains(err.Error(), "duplicate port") {
		t.Errorf("error does not name the collision: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(home, "services", "second.toml")); statErr == nil {
		t.Error("the rejected manifest was written anyway")
	}
}

func TestAdd_RequiresBinary(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo"); err == nil {
		t.Fatal("add without --binary: want error, got nil")
	}
}

func TestAdd_RejectsMissingBinary(t *testing.T) {
	home := newHome(t)
	_, _, err := runCLI(t, home, "add", "demo", "--binary", filepath.Join(t.TempDir(), "nope"))
	if err == nil {
		t.Fatal("add with a nonexistent binary: want error, got nil")
	}
}

func TestAdd_PrintDoesNotWrite(t *testing.T) {
	home := newHome(t)
	out, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep", "--print")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "binary") {
		t.Errorf("--print did not render the manifest:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "services", "demo.toml")); err == nil {
		t.Error("--print wrote a file")
	}
}

func TestAdd_EnvAndMCP(t *testing.T) {
	home := newHome(t)
	_, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep",
		"--env", "LOG_LEVEL=debug,MODE=test", "--mcp")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if svc.Env["LOG_LEVEL"] != "debug" || svc.Env["MODE"] != "test" {
		t.Errorf("env = %v", svc.Env)
	}
	if !svc.MCP {
		t.Error("--mcp not recorded")
	}
}

func TestAdd_RejectsMalformedEnv(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep", "--env", "JUSTAKEY"); err == nil {
		t.Fatal("malformed --env was accepted")
	}
}

func TestRemove_DeletesTheManifestButKeepsData(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	svc, _ := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	storage := svc.ExpandStorage()
	if err := os.WriteFile(filepath.Join(storage, "data.txt"), []byte("keep me"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	out, _, err := runCLI(t, home, "remove", "demo")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, "services", "demo.toml")); err == nil {
		t.Error("manifest still present after remove")
	}
	// Data is not destroyed by an ordinary remove.
	if _, err := os.Stat(filepath.Join(storage, "data.txt")); err != nil {
		t.Errorf("remove deleted the service's data: %v", err)
	}
	if !strings.Contains(out, "--purge") {
		t.Errorf("remove does not mention how to delete the data:\n%s", out)
	}
}

func TestRemove_PurgeDeletesData(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatalf("runCLI: %v", err)
	}
	svc, _ := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	storage := svc.ExpandStorage()

	if _, _, err := runCLI(t, home, "remove", "demo", "--purge", "--yes"); err != nil {
		t.Fatalf("remove --purge: %v", err)
	}
	if _, err := os.Stat(storage); !os.IsNotExist(err) {
		t.Error("--purge did not delete the storage directory")
	}
}

func TestRemove_UnknownService(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "remove", "ghost"); err == nil {
		t.Fatal("remove of an unknown service: want error, got nil")
	}
}

// With Tailscale off, a generated manifest has no tailscale_name and still
// validates.
func TestAdd_NoTailscaleName_WhenDisabled(t *testing.T) {
	home := newHome(t)
	writeConfig(t, home, "[tailscale]\nenabled = false\n")

	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatalf("add: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(home, "services", "demo.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "tailscale_name") {
		t.Errorf("a tailnet name was written with Tailscale disabled:\n%s", body)
	}

	// And apply must accept it.
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Errorf("apply rejected a manifest with no tailscale_name: %v", err)
	}
}

func TestAdd_TailscaleNameByDefault(t *testing.T) {
	home := newHome(t)
	if _, _, err := runCLI(t, home, "add", "demo", "--binary", "/bin/sleep"); err != nil {
		t.Fatal(err)
	}
	svc, _ := manifest.Load(filepath.Join(home, "services", "demo.toml"))
	if svc.TailscaleName != "demo" {
		t.Errorf("TailscaleName = %q; want demo", svc.TailscaleName)
	}
}

func writeConfig(t *testing.T, home, body string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

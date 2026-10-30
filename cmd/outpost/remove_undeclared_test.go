package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #32: a manifest deleted by hand shows as removed, and `outpost remove`
// clears it even though the file it would load is gone.
func TestRemove_ClearsAServiceWhoseManifestIsGone(t *testing.T) {
	home := tailnetDaemon(t)
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(home, "services", "alpha.toml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Fatal(err)
	}

	out, _, err := runCLI(t, home, "list")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("list does not show alpha as removed:\n%s", out)
	}
	if _, _, err := runCLI(t, home, "start", "alpha"); err == nil || !strings.Contains(err.Error(), "outpost remove alpha") {
		t.Errorf("start of a removed service: %v", err)
	}

	out, _, err = runCLI(t, home, "remove", "alpha")
	if err != nil {
		t.Fatalf("remove without a manifest: %v", err)
	}
	if !strings.Contains(out, "cleared alpha") {
		t.Errorf("remove output:\n%s", out)
	}
	out, _, _ = runCLI(t, home, "list")
	if strings.Contains(out, "alpha") {
		t.Errorf("alpha is still listed after remove:\n%s", out)
	}
	if _, _, err := runCLI(t, home, "remove", "alpha"); err == nil {
		t.Error("a second remove of a cleared service succeeded")
	}
}

// Removing a declared service with the daemon up leaves no removed row
// behind: it was removed on purpose.
func TestRemove_WithDaemonLeavesNoRow(t *testing.T) {
	home := tailnetDaemon(t)
	if _, _, err := runCLI(t, home, "apply"); err != nil {
		t.Fatal(err)
	}
	out, _, err := runCLI(t, home, "remove", "alpha")
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !strings.Contains(out, "stopped alpha and cleared its record") {
		t.Errorf("remove output:\n%s", out)
	}
	out, _, _ = runCLI(t, home, "list")
	if strings.Contains(out, "alpha") {
		t.Errorf("alpha is still listed after remove:\n%s", out)
	}
}

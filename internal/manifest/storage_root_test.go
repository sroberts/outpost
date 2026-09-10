package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeManifest drops a manifest into <root>/services and returns the root.
func rootWithManifest(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "services")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "alpha.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

const relBody = `
runtime        = "process"
binary         = "/usr/bin/true"
port           = 8080
storage        = "data/alpha"
tailscale_name = "alpha"
`

// A relative storage path must resolve against the Outpost root, not against
// whatever working directory the daemon happens to have been given.
func TestExpandStorage_RelativeResolvesAgainstRoot(t *testing.T) {
	root := rootWithManifest(t, relBody)

	svc, err := Load(filepath.Join(root, "services", "alpha.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	want := filepath.Join(root, "data", "alpha")
	if got := svc.ExpandStorage(); got != want {
		t.Errorf("ExpandStorage = %q; want %q", got, want)
	}
}

// The same manifest must name the same directory regardless of the process's
// working directory — the actual bug, not just the absolute-path shape.
func TestExpandStorage_IndependentOfWorkingDirectory(t *testing.T) {
	root := rootWithManifest(t, relBody)
	path := filepath.Join(root, "services", "alpha.toml")

	svc, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	before := svc.ExpandStorage()

	t.Chdir(t.TempDir())

	again, err := Load(path)
	if err != nil {
		t.Fatalf("Load after chdir: %v", err)
	}
	after := again.ExpandStorage()
	if after != before {
		t.Errorf("storage moved with the working directory: %q then %q", before, after)
	}
	// Equality alone would also hold if both were the same relative string, so
	// require that it actually resolved.
	if !filepath.IsAbs(after) {
		t.Errorf("storage = %q; want an absolute path independent of the cwd", after)
	}
}

func TestExpandStorage_AbsoluteIsUntouched(t *testing.T) {
	svc := &Service{Storage: "/var/lib/alpha"}
	svc.SetRoot("/somewhere/else")
	if got := svc.ExpandStorage(); got != "/var/lib/alpha" {
		t.Errorf("ExpandStorage = %q; want the absolute path unchanged", got)
	}
}

// With no root recorded a relative path stays relative, so a hand-built
// Service behaves as it always did.
func TestExpandStorage_NoRootLeavesRelativeAlone(t *testing.T) {
	svc := &Service{Storage: "data/alpha"}
	if got := svc.ExpandStorage(); got != filepath.Clean("data/alpha") {
		t.Errorf("ExpandStorage = %q; want it left relative", got)
	}
}

func TestValidate_RejectsStorageThatClimbsOutOfTheRoot(t *testing.T) {
	for _, path := range []string{"..", "../escape", "data/../../escape", "../"} {
		svc := &Service{
			Name: "alpha", Runtime: "process", Binary: "/usr/bin/true",
			Port: 8080, Storage: path, TailscaleName: "alpha",
			Healthcheck: "/up", RestartPolicy: "on-failure",
		}
		// Assert on the storage complaint specifically: with several required
		// fields in play it would be easy for this to pass on an unrelated
		// error and prove nothing.
		var found bool
		for _, err := range ValidateAll([]*Service{svc}) {
			if strings.Contains(err.Error(), "climbs above the Outpost root") {
				found = true
			}
		}
		if !found {
			t.Errorf("storage = %q was not refused for climbing out of the root", path)
		}
	}
}

// An absolute path outside the root is the operator being explicit, and must
// still be allowed.
func TestValidate_AllowsAbsoluteStorageOutsideTheRoot(t *testing.T) {
	svc := &Service{
		Name: "alpha", Runtime: "process", Binary: "/usr/bin/true",
		Port: 8080, Storage: "/var/lib/alpha", TailscaleName: "alpha",
		Healthcheck: "/up", RestartPolicy: "on-failure",
	}
	if errs := ValidateAll([]*Service{svc}); len(errs) != 0 {
		t.Errorf("absolute storage outside the root was refused: %v", errs)
	}
}

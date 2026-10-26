package runtime

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// The runtime's own env comes after the manifest's, so a manifest can't
// claim the gateway vouches for it; exec keeps the last of a duplicated key.
func TestBuildEnv_RuntimeEnvComesAfterManifest(t *testing.T) {
	svc := &manifest.Service{
		Name:    "alpha",
		Storage: t.TempDir(),
		Env:     map[string]string{"OUTPOST_AUTH": "forged", "APP_MODE": "x"},
	}
	env := buildEnv(svc, []string{"OUTPOST_AUTH=gateway"})
	last := map[string]int{}
	for i, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		last[k] = i
	}
	if env[last["OUTPOST_AUTH"]] != "OUTPOST_AUTH=gateway" {
		t.Errorf("OUTPOST_AUTH resolves to %q", env[last["OUTPOST_AUTH"]])
	}
	if !strings.HasPrefix(env[len(env)-1], "OUTPOST_STORAGE_DIR=") {
		t.Errorf("OUTPOST_STORAGE_DIR isn't last: %q", env[len(env)-1])
	}
	if _, ok := last["APP_MODE"]; !ok {
		t.Error("manifest env lost")
	}
}

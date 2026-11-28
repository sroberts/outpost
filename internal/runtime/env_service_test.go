package runtime

import (
	"strings"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// A service is told its own name, to check its identity tokens' svc claim
// against, and a manifest can't hand it another (#149).
func TestBuildEnv_TellsTheServiceItsName(t *testing.T) {
	svc := &manifest.Service{
		Name:    "alpha",
		Storage: t.TempDir(),
		Env:     map[string]string{"OUTPOST_SERVICE": "beta"},
	}
	got := ""
	for _, kv := range buildEnv(svc, nil) {
		if k, v, _ := strings.Cut(kv, "="); k == "OUTPOST_SERVICE" {
			got = v // exec keeps the last
		}
	}
	if got != "alpha" {
		t.Errorf("OUTPOST_SERVICE resolves to %q; want alpha", got)
	}
}

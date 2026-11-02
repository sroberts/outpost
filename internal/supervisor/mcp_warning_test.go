package supervisor

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// The daemon logs the warning whenever it loads the manifests, not only
// when someone runs `outpost apply`: a manifest edited by hand is picked up
// by Reload, and one already wrong is read at startup.
func TestApply_LogsMCPWithoutPath(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	var lines []string
	sup, err := New(Options{
		ServicesDir:     filepath.Join(h.dir, "services"),
		Store:           h.store,
		ProcessRuntime:  h.rt,
		Now:             h.clock.Now,
		ManifestOptions: manifest.Options{Gateway: true},
		Logf: func(format string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			lines = append(lines, fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.sup = sup
	h.writeManifest("yarr", "mcp = true")
	// The harness derives the port from the name's length, so not "sous".
	h.writeManifest("recipes", "mcp = true\nmcp_path = \"/mcp\"")
	h.apply()

	mu.Lock()
	defer mu.Unlock()
	var got []string
	for _, l := range lines {
		if strings.Contains(l, "mcp_path") {
			got = append(got, l)
		}
	}
	if len(got) != 1 || !strings.Contains(got[0], "yarr") {
		t.Errorf("want one mcp_path warning, for yarr; got %q", got)
	}
}

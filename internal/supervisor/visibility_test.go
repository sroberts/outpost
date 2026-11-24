package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// A gateway-only service runs, but never gets a tailnet node: not when it
// starts, not on a healthy tick, and with Tailscale on, no tailscale_name is
// asked of it.
func TestVisibility_GatewayOnlyGetsNoNode(t *testing.T) {
	h, tn := tailnetHarness(t)
	h.sup.manifestOpts = manifest.Options{RequireTailscale: true, Gateway: true}
	body := fmt.Sprintf(`
runtime    = "process"
binary     = "/usr/bin/true"
port       = 9301
storage    = %q
visibility = "gateway"
`, filepath.Join(h.dir, "data", "quiet"))
	if err := os.WriteFile(filepath.Join(h.dir, "services", "quiet.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	h.tick()

	if got := h.rt.startCount("quiet"); got != 1 {
		t.Fatalf("start count = %d; want 1", got)
	}
	if attached, _, live := tn.counts("quiet"); attached != 0 || live {
		t.Errorf("gateway-only service: attached=%d live=%v; want no node", attached, live)
	}
	// The default is unchanged: alpha still joins.
	if attached, _, _ := tn.counts("alpha"); attached == 0 {
		t.Error("alpha, visibility unset, never joined the tailnet")
	}

	st := h.statusOf("quiet")
	if st.Visibility != manifest.VisibilityGateway || st.Tailnet != nil {
		t.Errorf("status visibility=%q tailnet=%+v; want gateway and no node", st.Visibility, st.Tailnet)
	}
	if got := h.statusOf("alpha").Visibility; got != manifest.VisibilityTailnet {
		t.Errorf("alpha's visibility = %q; want the default, tailnet", got)
	}
}

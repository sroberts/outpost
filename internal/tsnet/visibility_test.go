package tsnet

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/sroberts/outpost/internal/manifest"
)

// The router refuses a gateway-only service outright, so no caller can give
// it a node, even one that skipped validation.
func TestAttach_RefusesGatewayOnly(t *testing.T) {
	var started atomic.Bool
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		started.Store(true)
		return newFakeNode(t), nil
	})
	s := svc("quiet", 9302)
	s.Visibility = manifest.VisibilityGateway
	if err := r.Attach(context.Background(), s); err == nil {
		t.Fatal("Attach accepted a gateway-only service")
	}
	if got := r.Attached(); len(got) != 0 || started.Load() {
		t.Errorf("attached = %v, started = %v; want no node", got, started.Load())
	}
}

package tsnet

import (
	"context"
	"testing"

	"github.com/sroberts/outpost/internal/tailnet"
)

// A node must read as starting from the moment Attach returns, before the
// attach goroutine has run at all. A node with no state reads as unknown, and
// every renderer shows unknown as the configured name — which is exactly how
// an attached node looks, and the confusion this state exists to remove.
func TestAttach_NodeReadsStartingBeforeTheAttachGoroutineRuns(t *testing.T) {
	clock := newStubClock()
	r := testRouter(t, func(ctx context.Context, cfg nodeConfig) (tailnetNode, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	r.now = clock.now

	if err := r.Attach(context.Background(), svc("fresh", 9310)); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	st, ok := r.NodeStatus("fresh")
	if !ok {
		t.Fatal("no status for a node Attach has just registered")
	}
	if st.State != tailnet.StateStarting {
		t.Errorf("state = %q straight after Attach; want %q", st.State, tailnet.StateStarting)
	}
	if !st.Since.Equal(clock.now()) {
		t.Errorf("since = %v; want the time Attach registered the node, %v", st.Since, clock.now())
	}
}

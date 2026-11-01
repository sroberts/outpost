package gateway

import "testing"

// A routed service reads as zero before anything reaches it, so the sampler's
// baseline comes before the first request rather than after it.
func TestTraffic_UntouchedRouteReadsZero(t *testing.T) {
	// wiki has a port of its own here, and nothing has dialed it.
	h, _ := trafficHarness(t)
	got, ok := h.g.Traffic()["wiki"]
	if !ok {
		t.Fatal("a routed service nothing has reached yet is missing from Traffic")
	}
	if got.In != 0 || got.Out != 0 {
		t.Fatalf("wiki = %+v before any request; want zero", got)
	}
}

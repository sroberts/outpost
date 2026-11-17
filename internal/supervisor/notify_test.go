package supervisor

import (
	"context"
	"testing"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A watcher holding Changed is woken by an event, and by a service's state
// being persisted, and a fresh channel waits for the next change.
func TestChanged_FiresOnEventAndPersist(t *testing.T) {
	h := newHarness(t)

	ch := h.sup.Changed()
	if closed(ch) {
		t.Fatal("Changed is closed before anything changed")
	}
	if err := h.sup.RecordEvent(context.Background(), "gateway", "sign-in", "alice"); err != nil {
		t.Fatal(err)
	}
	if !closed(ch) {
		t.Fatal("an event did not close Changed")
	}

	ch = h.sup.Changed()
	if closed(ch) {
		t.Fatal("the channel after a change is already closed")
	}
	h.writeManifest("alpha", "")
	h.apply()
	h.tick()
	if !closed(ch) {
		t.Fatal("applying and starting a service did not close Changed")
	}

	// persist alone, without an event alongside it.
	ch = h.sup.Changed()
	_, _ = h.sup.submit(context.Background(), func(s *Supervisor) (any, error) {
		s.persist(s.tracked["alpha"])
		return nil, nil
	})
	if !closed(ch) {
		t.Fatal("persist did not close Changed")
	}
}

// Every watcher holding the channel is woken, not just one.
func TestChanged_Broadcasts(t *testing.T) {
	h := newHarness(t)
	a, b := h.sup.Changed(), h.sup.Changed()
	h.sup.changes.notify()
	if !closed(a) || !closed(b) {
		t.Fatal("notify did not wake every watcher")
	}
}

// Notifying with nobody watching must not panic or block.
func TestChanged_NotifyWithoutWatchers(t *testing.T) {
	h := newHarness(t)
	h.sup.changes.notify()
	h.sup.changes.notify()
	if closed(h.sup.Changed()) {
		t.Fatal("a change before anyone watched is delivered to a later watcher")
	}
}

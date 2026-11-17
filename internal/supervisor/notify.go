package supervisor

import (
	"reflect"
	"sync"
)

// changes is a broadcast that a watcher can wait on: each change closes the
// current channel, waking everyone holding it, and puts a fresh one in its
// place. Closing never blocks, so the reconcile loop can signal however many
// watchers there are, or however slow they are, without waiting on any.
type changes struct {
	mu sync.Mutex
	ch chan struct{}
}

func (c *changes) wait() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ch == nil {
		c.ch = make(chan struct{})
	}
	return c.ch
}

func (c *changes) notify() {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Nobody has asked yet, so there is nobody to wake.
	if c.ch == nil {
		return
	}
	close(c.ch)
	c.ch = nil
}

// Changed returns a channel closed at the next change to any service's
// observed state or the activity feed. Safe from any goroutine. A change can
// be signalled without anything a client would see being different, so a
// watcher compares what it reads rather than trusting every wake-up.
func (s *Supervisor) Changed() <-chan struct{} { return s.changes.wait() }

// noticeTailnetLocked signals a change when any service's tailnet node has
// changed state since the last tick: nodes come up, need a login or fail in
// the router's own time, and nothing persisted records it. Reconcile-loop
// goroutine only.
func (s *Supervisor) noticeTailnetLocked() {
	now := make(map[string]TailnetStatus)
	for name, svc := range s.desired {
		if ts := s.tailnetOf(svc); ts != nil {
			now[name] = *ts
		}
	}
	if s.tailnetSeen != nil && !reflect.DeepEqual(now, s.tailnetSeen) {
		s.changes.notify()
	}
	s.tailnetSeen = now
}

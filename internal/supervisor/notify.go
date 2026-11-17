package supervisor

import "sync"

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

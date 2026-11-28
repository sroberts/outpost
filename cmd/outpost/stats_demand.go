package main

import (
	"context"
	"sync"

	"github.com/sroberts/outpost/internal/stats"
)

// sampleRunner is the part of the sampler that runs; a test stands in for it.
type sampleRunner interface {
	Run(ctx context.Context)
	All() map[string]stats.Usage
}

// demandStats runs the sampler only once something wants usage: the gateway's
// catalog at startup, or the first stats request over the socket. A daemon
// nobody looks at, without the gateway, so pays for no ps probes and no
// storage walks.
type demandStats struct {
	ctx context.Context
	s   sampleRunner

	mu      sync.Mutex
	started bool
	closed  bool
	done    chan struct{}
}

func newDemandStats(ctx context.Context, s sampleRunner) *demandStats {
	return &demandStats{ctx: ctx, s: s, done: make(chan struct{})}
}

// start runs the sampler if it isn't running yet. Once stop has been called
// it does nothing, so nothing can outlive the stores the sampler reads.
func (d *demandStats) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.started || d.closed {
		return
	}
	d.started = true
	go func() {
		defer close(d.done)
		d.s.Run(d.ctx)
	}()
}

// All is the stats command's source. The first call starts sampling, so it
// reads empty; the client's next poll has the first sample.
func (d *demandStats) All() map[string]stats.Usage {
	d.start()
	return d.s.All()
}

// stop refuses any later start and waits for a running sampler to finish,
// which it does once ctx is done.
func (d *demandStats) stop() {
	d.mu.Lock()
	d.closed = true
	started := d.started
	d.mu.Unlock()
	if started {
		<-d.done
	}
}

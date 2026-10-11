package supervisor

import (
	"context"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
)

// Healthchecks and the daemon's own liveness (#29).
//
// Probes used to run one after another on the reconcile loop, so N services
// timing out cost N timeouts, and the loop could not keep its interval once
// two of them failed. And a probe that timed out because the daemon itself
// could not be scheduled -- it was stalled, not the service -- counted
// against the service exactly like a real failure, so one stall restarted
// every healthy service at once.

const (
	// heartbeatPeriod is how often the daemon checks that it is being
	// scheduled at all.
	heartbeatPeriod = 250 * time.Millisecond
	// stallThreshold is how late a heartbeat has to be before the daemon
	// counts itself as stalled. A healthcheck timeout is 2s, so a stall long
	// enough to fail one is well past this.
	stallThreshold = time.Second
)

// heartbeat notices the daemon failing to run. It uses the wall clock rather
// than the supervisor's injectable one, because what it measures is real
// scheduling delay.
type heartbeat struct {
	period, threshold time.Duration

	mu       sync.Mutex
	last     time.Time     // most recent beat; zero while not running
	stallEnd time.Time     // when the most recent stall was seen to end
	stallLen time.Duration // how long it was
}

func newHeartbeat(period, threshold time.Duration) *heartbeat {
	return &heartbeat{period: period, threshold: threshold}
}

// run beats until ctx ends.
func (h *heartbeat) run(ctx context.Context) {
	t := time.NewTicker(h.period)
	defer t.Stop()

	h.mu.Lock()
	h.last = time.Now()
	h.mu.Unlock()
	defer func() {
		// A heartbeat that is not running knows nothing; an old beat left
		// behind would read as a stall that never ends.
		h.mu.Lock()
		h.last = time.Time{}
		h.mu.Unlock()
	}()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		h.mu.Lock()
		if late := now.Sub(h.last) - h.period; late > h.threshold {
			h.stallEnd, h.stallLen = now, late
		}
		h.last = now
		h.mu.Unlock()
	}
}

// stalledSince reports whether the daemon stalled at any point from since
// until now, and for how long. With the heartbeat not running it reports
// nothing, so a supervisor driven tick by tick behaves as it always did.
func (h *heartbeat) stalledSince(since time.Time) (time.Duration, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.last.IsZero() {
		return 0, false
	}
	// Still stalled: the heartbeat itself has not run yet to record it.
	if late := time.Since(h.last) - h.period; late > h.threshold {
		return late, true
	}
	if h.stallLen > 0 && !h.stallEnd.Before(since) {
		return h.stallLen, true
	}
	return 0, false
}

// probeResult is one healthcheck, taken for a particular instance.
type probeResult struct {
	handle  runtime.Handle
	ok      bool
	err     error
	started time.Time
}

// probeAll healthchecks every running service concurrently and waits for all
// of them, so a tick costs one probe timeout however many services are
// failing. Only the probes run in parallel: their results are applied on the
// reconcile loop, which stays the only goroutine touching supervisor state.
func (s *Supervisor) probeAll(ctx context.Context) map[string]probeResult {
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = map[string]probeResult{}
	)
	for name, st := range s.tracked {
		if st.handle == nil || exited(st.handle) {
			continue
		}
		h, rt := st.handle, s.runtimeFor(st.svc)
		wg.Add(1)
		go func() {
			defer wg.Done()
			started := time.Now()
			ok, err := rt.Healthy(ctx, h)
			mu.Lock()
			out[name] = probeResult{handle: h, ok: ok, err: err, started: started}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return out
}

// probe returns this tick's healthcheck for st's current instance, taking one
// now if the batch has none -- the instance was started or replaced since.
func (s *Supervisor) probe(ctx context.Context, st *svcState) probeResult {
	if r, ok := s.probes[st.svc.Name]; ok && r.handle == st.handle {
		return r
	}
	started := time.Now()
	ok, err := s.runtimeFor(st.svc).Healthy(ctx, st.handle)
	return probeResult{handle: st.handle, ok: ok, err: err, started: started}
}

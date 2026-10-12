package main

import (
	"fmt"
	"io"
	goruntime "runtime"
	"sync"
	"time"
)

// defaultStartupTimeout bounds daemon bring-up, from the banner to the socket
// accepting connections. Healthy startups take about a second; the slack is
// for orphan reclaim, which gives each leftover process a few seconds to exit.
const defaultStartupTimeout = 2 * time.Minute

// startupWatch turns a daemon that hangs during bring-up into one that exits.
//
// Under launchd a wedged start was invisible (#30): the job read as running,
// nothing past the banner was logged, and no socket ever appeared, so the
// only symptom was a CLI that could not find a daemon. Exiting non-zero lets
// KeepAlive or Restart=always try again, and the log says which stage it
// stopped in, with every goroutine's stack, which is what the next diagnosis
// needs.
type startupWatch struct {
	mu    sync.Mutex
	stage string
	since time.Time

	done chan struct{}
	once sync.Once
}

// watchStartup arms the deadline. A zero timeout disables it. exit is
// os.Exit outside tests.
func watchStartup(timeout time.Duration, log io.Writer, exit func(int)) *startupWatch {
	w := &startupWatch{stage: "starting", since: time.Now(), done: make(chan struct{})}
	if timeout <= 0 {
		return w
	}
	go func() {
		t := time.NewTimer(timeout)
		defer t.Stop()
		select {
		case <-w.done:
			return
		case <-t.C:
		}
		w.mu.Lock()
		stage, since := w.stage, w.since
		w.mu.Unlock()
		fmt.Fprintf(log, "outpost: startup did not finish within %s; stuck %s for %s. Exiting so the service manager restarts the daemon.\n",
			timeout, stage, time.Since(since).Round(time.Second))
		fmt.Fprintf(log, "outpost: goroutines at the deadline:\n%s\n", allStacks())
		exit(1)
	}()
	return w
}

// at records the stage bring-up has reached.
func (w *startupWatch) at(stage string) {
	w.mu.Lock()
	w.stage, w.since = stage, time.Now()
	w.mu.Unlock()
}

// finished disarms the deadline. Safe to call more than once.
func (w *startupWatch) finished() { w.once.Do(func() { close(w.done) }) }

func allStacks() []byte {
	buf := make([]byte, 256<<10)
	for {
		n := goruntime.Stack(buf, true)
		if n < len(buf) {
			return buf[:n]
		}
		buf = make([]byte, 2*len(buf))
	}
}

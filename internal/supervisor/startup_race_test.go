package supervisor

import (
	"context"
	"sync"
	"testing"
	"time"
)

// An API call racing Run's startup must not execute on the caller's goroutine
// while the loop is already ticking. Run sets loopRunning as its first
// statement, so a caller that reads it a moment earlier takes the inline path.
func TestSubmit_RacesRunStartup(t *testing.T) {
	for attempt := 0; attempt < 60; attempt++ {
		h := newHarness(t)
		h.writeManifest("alpha", "")
		h.apply()
		h.sup.Interval = time.Millisecond

		ctx, cancel := context.WithCancel(context.Background())
		var wg sync.WaitGroup

		// Release both goroutines at once so the API calls land exactly as Run
		// is coming up, and again as it is shutting down.
		start := make(chan struct{})

		wg.Add(1)
		go func() { defer wg.Done(); <-start; h.sup.Run(ctx) }()

		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 25; j++ {
					h.sup.List(context.Background())
					h.sup.Services()
					h.sup.Status(context.Background(), "alpha")
				}
			}()
		}
		close(start)

		cancel()
		wg.Wait()
	}
}

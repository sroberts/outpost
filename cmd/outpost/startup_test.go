package main

import (
	"bytes"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is written by the watch goroutine and read by the test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// A bring-up that never finishes must exit non-zero, saying where it stuck,
// rather than sit there looking healthy to launchd (#30).
func TestStartupWatch_ExitsAndSaysWhereItStuck(t *testing.T) {
	var log syncBuffer
	exited := make(chan int, 1)
	w := watchStartup(50*time.Millisecond, &log, func(code int) { exited <- code })
	w.at("reclaiming orphans from a previous run")

	select {
	case code := <-exited:
		if code == 0 {
			t.Error("exit code 0; the service manager only restarts a failed daemon")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a stalled startup did not exit")
	}
	out := log.String()
	for _, want := range []string{"reclaiming orphans from a previous run", "goroutine "} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not mention %q:\n%.500s", want, out)
		}
	}
}

// A startup that finishes in time is left alone.
func TestStartupWatch_FinishedDisarms(t *testing.T) {
	exited := make(chan int, 1)
	w := watchStartup(50*time.Millisecond, &syncBuffer{}, func(code int) { exited <- code })
	w.finished()
	w.finished() // the deferred call in cmdServe runs after the explicit one

	select {
	case code := <-exited:
		t.Fatalf("exited with %d after startup finished", code)
	case <-time.After(200 * time.Millisecond):
	}
}

// --startup-timeout=0 turns the deadline off.
func TestStartupWatch_ZeroDisables(t *testing.T) {
	exited := make(chan int, 1)
	watchStartup(0, &syncBuffer{}, func(code int) { exited <- code })

	select {
	case code := <-exited:
		t.Fatalf("exited with %d with the deadline disabled", code)
	case <-time.After(100 * time.Millisecond):
	}
}

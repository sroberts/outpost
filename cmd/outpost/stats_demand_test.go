package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/stats"
)

type countingSampler struct{ runs atomic.Int32 }

func (c *countingSampler) Run(ctx context.Context) {
	c.runs.Add(1)
	<-ctx.Done()
}

func (c *countingSampler) All() map[string]stats.Usage { return nil }

func TestDemandStats_StartsOnFirstReadOnly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &countingSampler{}
	d := newDemandStats(ctx, c)

	time.Sleep(20 * time.Millisecond)
	if n := c.runs.Load(); n != 0 {
		t.Fatalf("sampler ran %d times before anything asked for usage", n)
	}
	d.All()
	d.All()
	deadline := time.Now().Add(time.Second)
	for c.runs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := c.runs.Load(); n != 1 {
		t.Fatalf("sampler ran %d times after two reads, want 1", n)
	}
	cancel()
	d.stop()
}

func TestDemandStats_NoStartAfterStop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c := &countingSampler{}
	d := newDemandStats(ctx, c)
	d.stop() // never started: must not block
	d.All()
	time.Sleep(20 * time.Millisecond)
	if n := c.runs.Load(); n != 0 {
		t.Fatalf("sampler started after stop (%d runs)", n)
	}
}

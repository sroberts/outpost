package ipc

import (
	"context"
	"encoding/json"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/stats"
)

type fakeStats map[string]stats.Usage

func (f fakeStats) All() map[string]stats.Usage { return f }

func statsDaemon(t *testing.T, src StatsSource) *Client {
	t.Helper()
	sup := newIdleSupervisor(t, t.TempDir())
	sockPath := filepath.Join(shortDir(t), "outpost.sock")
	srv, err := Listen(sockPath, sup, nil)
	if err != nil {
		t.Fatal(err)
	}
	if src != nil {
		srv.UseStats(src)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); _ = srv.Serve(ctx) }()
	t.Cleanup(func() { cancel(); srv.Close(); <-done })
	return NewClient(sockPath)
}

func TestStats_RoundTripsNaN(t *testing.T) {
	nan := math.NaN()
	since := time.Unix(1_800_000_000, 0)
	c := statsDaemon(t, fakeStats{
		"app": {
			Up: true, Measured: true, CPU: nan, RSS: 4096, Since: since,
			CPUSeries: []float64{nan, 0.5}, RecentRSS: []float64{1, nan, 3},
		},
		"other": {CPU: 0.25},
	})

	all, err := c.Stats("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("Stats(\"\") = %d services; want 2", len(all))
	}
	u := all["app"]
	if !u.Up || !u.Measured || u.RSS != 4096 || !math.IsNaN(u.CPU) || !u.Since.Equal(since) {
		t.Errorf("app = %+v", u)
	}
	if len(u.CPUSeries) != 2 || !math.IsNaN(u.CPUSeries[0]) || u.CPUSeries[1] != 0.5 {
		t.Errorf("cpu series = %v", u.CPUSeries)
	}
	if len(u.RecentRSS) != 3 || u.RecentRSS[0] != 1 || !math.IsNaN(u.RecentRSS[1]) || u.RecentRSS[2] != 3 {
		t.Errorf("recent rss = %v", u.RecentRSS)
	}
	if all["other"].CPU != 0.25 {
		t.Errorf("other cpu = %v", all["other"].CPU)
	}

	one, err := c.Stats("other")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := one["other"]; len(one) != 1 || !ok {
		t.Errorf("Stats(other) = %v", one)
	}
	none, err := c.Stats("missing")
	if err != nil || len(none) != 0 {
		t.Errorf("Stats(missing) = %v, %v; want empty", none, err)
	}
}

// NaN goes over the wire as null, which is what lets it be encoded at all.
func TestUsageInfo_NaNIsNull(t *testing.T) {
	b, err := json.Marshal(usageInfo(stats.Usage{CPU: math.NaN(), InSeries: []float64{math.NaN(), 2}}))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"cpu":null`) || !strings.Contains(s, `"in_series":[null,2]`) {
		t.Errorf("wire form = %s", s)
	}
}

func TestStats_Unavailable(t *testing.T) {
	c := statsDaemon(t, nil)
	_, err := c.Stats("")
	if err == nil || err.Error() != "usage stats are not available from this daemon" {
		t.Fatalf("err = %v", err)
	}
}

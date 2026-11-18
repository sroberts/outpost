package stats

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

// The recent series are the raw 30-second slots, oldest first and ending with
// the current one, with a gap wherever the service wasn't up.
func TestUsage_RecentSlots(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.probe.set(map[int]Group{100: {CPUTime: time.Second, RSS: 1000, Leader: true}})
	r.setTraffic(map[string]Bytes{"app": {In: 10, Out: 20}})
	r.s.sampleOnce(context.Background(), t0)

	r.probe.set(map[int]Group{100: {CPUTime: 16 * time.Second, RSS: 2000, Leader: true}})
	r.setTraffic(map[string]Bytes{"app": {In: 110, Out: 220}})
	r.s.sampleOnce(context.Background(), t0.Add(30*time.Second))

	// Down for the next slot, but the gateway still carried traffic for it.
	r.list.set(supervisor.ServiceStatus{Name: "app", Status: "failed"})
	r.setTraffic(map[string]Bytes{"app": {In: 115, Out: 225}})
	r.s.sampleOnce(context.Background(), t0.Add(60*time.Second))

	u := r.usage(t, "app")
	for _, s := range [][]float64{u.RecentCPU, u.RecentRSS, u.RecentIn, u.RecentOut} {
		if len(s) != RecentSlots {
			t.Fatalf("recent series has %d slots; want %d", len(s), RecentSlots)
		}
	}
	last := RecentSlots - 1
	// Slots before the first sample hold nothing.
	for i := range last - 2 {
		if !math.IsNaN(u.RecentCPU[i]) || !math.IsNaN(u.RecentRSS[i]) || !math.IsNaN(u.RecentIn[i]) || !math.IsNaN(u.RecentOut[i]) {
			t.Fatalf("slot %d has data before the first sample", i)
		}
	}

	first, second, down := last-2, last-1, last
	// The first sample has memory and traffic (none yet: no prior reading)
	// but no CPU difference to take.
	if !math.IsNaN(u.RecentCPU[first]) || u.RecentRSS[first] != 1000 || u.RecentIn[first] != 0 {
		t.Errorf("first slot = cpu %v rss %v in %v", u.RecentCPU[first], u.RecentRSS[first], u.RecentIn[first])
	}
	if u.RecentCPU[second] != 0.5 || u.RecentRSS[second] != 2000 || u.RecentIn[second] != 100 || u.RecentOut[second] != 200 {
		t.Errorf("second slot = cpu %v rss %v in %v out %v; want 0.5, 2000, 100, 200",
			u.RecentCPU[second], u.RecentRSS[second], u.RecentIn[second], u.RecentOut[second])
	}
	if !math.IsNaN(u.RecentCPU[down]) || !math.IsNaN(u.RecentRSS[down]) {
		t.Errorf("down slot = cpu %v rss %v; want gaps", u.RecentCPU[down], u.RecentRSS[down])
	}
	if u.RecentIn[down] != 5 || u.RecentOut[down] != 5 {
		t.Errorf("down slot traffic = %v/%v; want 5/5, drawn because some was counted", u.RecentIn[down], u.RecentOut[down])
	}
}

// A down slot with no traffic is a gap in the traffic series too, and slots
// older than half an hour drop out of the window.
func TestUsage_RecentWindowSlides(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.probe.set(map[int]Group{100: {CPUTime: time.Second, RSS: 1000, Leader: true}})
	r.s.sampleOnce(context.Background(), t0)

	r.list.set(supervisor.ServiceStatus{Name: "app", Status: "stopped"})
	later := t0.Add(RecentSlots * sampleEvery)
	r.s.sampleOnce(context.Background(), later)

	u := r.usage(t, "app")
	for i, v := range u.RecentRSS {
		if !math.IsNaN(v) {
			t.Errorf("rss slot %d = %v; the only up sample is outside the window", i, v)
		}
	}
	if v := u.RecentIn[RecentSlots-1]; !math.IsNaN(v) {
		t.Errorf("down slot with no traffic = %v; want a gap", v)
	}
}

func TestSampler_All(t *testing.T) {
	r := newRig(t, running("a", "100"), running("b", "200"))
	r.probe.set(map[int]Group{100: {RSS: 1, Leader: true}, 200: {RSS: 2, Leader: true}})
	if got := r.s.All(); len(got) != 0 {
		t.Fatalf("All before a sample = %v; want empty", got)
	}
	r.s.sampleOnce(context.Background(), t0)

	all := r.s.All()
	if len(all) != 2 || all["a"].RSS != 1 || all["b"].RSS != 2 {
		t.Fatalf("All = %+v", all)
	}
	// The map is the caller's: changing it doesn't change the sampler's.
	delete(all, "a")
	if _, ok := r.s.Usage("a"); !ok {
		t.Error("deleting from All's map reached the sampler")
	}
}

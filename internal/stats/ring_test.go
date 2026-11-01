package stats

import (
	"math"
	"testing"
	"time"
)

// t0 is on a half-hour boundary, so it starts a sample slot, a storage slot
// and a quarter-hour bucket all at once.
var t0 = time.Unix(1_800_000_000, 0)

func TestRing_WrapsAroundOntoTheSameSlot(t *testing.T) {
	r := new(ring)
	r.put(t0, slot{in: 5, up: true})
	later := t0.Add(window)
	r.put(later, slot{in: 7, up: true})

	if r.index(slotStart(t0, sampleEvery)) != r.index(slotStart(later, sampleEvery)) {
		t.Fatal("a day apart should be the same slot")
	}
	if s, ok := r.get(later.Unix(), later); !ok || s.in != 7 {
		t.Errorf("new slot = %+v, %v; want in 7", s, ok)
	}
	if _, ok := r.get(t0.Unix(), later); ok {
		t.Error("the overwritten slot still reads")
	}
}

func TestRing_ExpiresAfterADay(t *testing.T) {
	r := new(ring)
	r.put(t0, slot{in: 5, out: 2, up: true})

	if got := r.summarize(t0.Add(window - sampleEvery)); got.in24 != 5 || got.out24 != 2 {
		t.Errorf("just inside the day: in24 %d out24 %d; want 5, 2", got.in24, got.out24)
	}
	got := r.summarize(t0.Add(window))
	if got.in24 != 0 || got.since != 0 {
		t.Errorf("a day later: in24 %d since %d; want nothing", got.in24, got.since)
	}
	if _, ok := r.get(t0.Unix(), t0.Add(window)); ok {
		t.Error("a day-old slot still reads")
	}
}

func TestRing_TwoSamplesInOneSlotMerge(t *testing.T) {
	r := new(ring)
	r.put(t0, slot{cpu: 0.5, rss: 10, mem: true, in: 3, up: true})
	r.put(t0.Add(10*time.Second), slot{cpu: float32(math.NaN()), rss: 30, mem: true, in: 4, out: 1, up: true})
	s, ok := r.get(t0.Unix(), t0.Add(time.Minute))
	if !ok || s.cpu != 0.5 || s.rss != 30 || s.in != 7 || s.out != 1 {
		t.Errorf("merged = %+v; want cpu kept, rss max, traffic summed", s)
	}
}

func TestRing_Buckets(t *testing.T) {
	r := new(ring)
	bw := bucketWidth
	// Bucket 0: two up slots.
	r.put(t0, slot{cpu: 0.5, rss: 100, mem: true, in: 10, up: true})
	r.put(t0.Add(sampleEvery), slot{cpu: 1.5, rss: 50, mem: true, in: 20, out: 4, up: true})
	// Bucket 1: nothing. Bucket 2: down.
	r.put(t0.Add(2*bw), slot{cpu: float32(math.NaN()), up: false})
	// The current bucket: up, but neither cpu nor memory read.
	now := t0.Add(95*bw + time.Minute)
	r.put(t0.Add(95*bw), slot{cpu: float32(math.NaN()), in: 3, up: true})

	got := r.summarize(now)
	for name, s := range map[string][]float64{"cpu": got.cpu, "rss": got.rss, "in": got.in, "out": got.out} {
		if len(s) != Buckets {
			t.Fatalf("%s has %d buckets", name, len(s))
		}
		for _, b := range []int{1, 2} {
			if !math.IsNaN(s[b]) {
				t.Errorf("%s[%d] = %v; want NaN for no data and for down", name, b, s[b])
			}
		}
	}
	if got.cpu[0] != 1.5 || got.rss[0] != 100 {
		t.Errorf("bucket 0 cpu %v rss %v; want the highest: 1.5, 100", got.cpu[0], got.rss[0])
	}
	if got.in[0] != 30 || got.out[0] != 4 {
		t.Errorf("bucket 0 in %v out %v; want the sum: 30, 4", got.in[0], got.out[0])
	}
	if !math.IsNaN(got.cpu[95]) || !math.IsNaN(got.rss[95]) || got.in[95] != 3 || got.out[95] != 0 {
		t.Errorf("bucket 95: cpu %v rss %v in %v out %v", got.cpu[95], got.rss[95], got.in[95], got.out[95])
	}
	if got.since != t0.Unix() {
		t.Errorf("since = %d; want %d", got.since, t0.Unix())
	}
	if got.in24 != 33 || got.out24 != 4 {
		t.Errorf("in24 %d out24 %d; want 33, 4", got.in24, got.out24)
	}
}

func TestStorageRing_Buckets(t *testing.T) {
	r := new(storageRing)
	r.put(t0, 100, false)
	r.put(t0.Add(storageEvery), 200, false)
	r.put(t0.Add(2*storageEvery), 300, true)
	now := t0.Add(95*bucketWidth + time.Minute)
	got := r.summarize(now)
	if got[0] != 200 || got[1] != 300 || !math.IsNaN(got[2]) {
		t.Errorf("storage buckets = %v %v %v; want 200 300 NaN", got[0], got[1], got[2])
	}
	if got := r.summarize(t0.Add(window + time.Hour)); !math.IsNaN(got[0]) || !math.IsNaN(got[95]) {
		t.Error("storage older than a day still shows")
	}
}

// Traffic that crossed the gateway while the app was down is drawn, so the
// graph agrees with the day's totals; cpu and memory still gap.
func TestRing_TrafficWhileDownIsDrawn(t *testing.T) {
	r := new(ring)
	r.put(t0, slot{in: 50, out: 7, up: false})
	got := r.summarize(t0.Add(95*bucketWidth + time.Minute))
	if got.in[0] != 50 || got.out[0] != 7 || got.in24 != 50 {
		t.Errorf("in[0]=%v out[0]=%v in24=%d; want the down slot's traffic drawn and totalled", got.in[0], got.out[0], got.in24)
	}
	if !math.IsNaN(got.cpu[0]) || !math.IsNaN(got.rss[0]) {
		t.Errorf("cpu[0]=%v rss[0]=%v; want gaps while down", got.cpu[0], got.rss[0])
	}
}

package stats

import (
	"math"
	"time"
)

const (
	// sampleEvery is the width of one slot in the sample ring, and how often
	// the sampler ticks.
	sampleEvery = 30 * time.Second
	// window is how far back history goes; anything older reads as empty.
	window = 24 * time.Hour
	// ringSlots is a day of 30-second slots.
	ringSlots = int(window / sampleEvery)

	// storageEvery is how often storage is walked, and the width of one slot
	// in the storage ring. A walk can be expensive, so it is far rarer than
	// a sample.
	storageEvery = 10 * time.Minute
	storageSlots = int(window / storageEvery)

	// Buckets is how many points each series in a Usage has: a day in
	// quarter hours, few enough to draw as a sparkline.
	Buckets     = 96
	bucketWidth = window / Buckets
)

// slot is one 30-second sample. Slots are reused as the ring wraps, so at
// says which interval a slot currently holds; a slot whose at isn't the one
// being asked for holds nothing for it.
type slot struct {
	at  int64   // unix seconds at the start of the slot
	cpu float32 // cores; NaN when not known
	rss uint64
	in  uint64 // bytes to the service during the interval
	out uint64 // bytes from it
	up  bool
	mem bool // rss is known
}

// ring is a day of samples in fixed slots, indexed by time, so its size never
// grows however long the daemon runs.
type ring [ringSlots]slot

func slotStart(t time.Time, width time.Duration) int64 {
	w := int64(width / time.Second)
	return t.Unix() / w * w
}

func (r *ring) index(at int64) int {
	return int(at/int64(sampleEvery/time.Second)) % ringSlots
}

// put records a sample in the slot for now. Two samples can land in one slot
// when ticks drift; the second is merged in rather than replacing the first,
// so traffic isn't lost and the slot keeps the higher load.
func (r *ring) put(now time.Time, s slot) {
	s.at = slotStart(now, sampleEvery)
	p := &r[r.index(s.at)]
	if p.at != s.at {
		*p = s
		return
	}
	p.in += s.in
	p.out += s.out
	p.up = p.up || s.up
	if s.mem {
		p.rss = max(p.rss, s.rss)
		p.mem = true
	}
	if !isNaN32(s.cpu) && (isNaN32(p.cpu) || s.cpu > p.cpu) {
		p.cpu = s.cpu
	}
}

// get is the slot starting at at, if it holds that interval and is inside the
// window ending at now.
func (r *ring) get(at int64, now time.Time) (slot, bool) {
	if !live(at, now) {
		return slot{}, false
	}
	s := r[r.index(at)]
	return s, s.at == at
}

// live reports whether a slot starting at at is within the day ending at now.
func live(at int64, now time.Time) bool {
	n := now.Unix()
	return at <= n && at > n-int64(window/time.Second)
}

// storageSlot is one walk of a service's storage.
type storageSlot struct {
	at      int64
	bytes   uint64
	partial bool
}

type storageRing [storageSlots]storageSlot

func (r *storageRing) index(at int64) int {
	return int(at/int64(storageEvery/time.Second)) % storageSlots
}

func (r *storageRing) put(now time.Time, bytes uint64, partial bool) {
	at := slotStart(now, storageEvery)
	r[r.index(at)] = storageSlot{at: at, bytes: bytes, partial: partial}
}

func (r *storageRing) get(at int64, now time.Time) (storageSlot, bool) {
	if !live(at, now) {
		return storageSlot{}, false
	}
	s := r[r.index(at)]
	return s, s.at == at
}

// bucketStart is the start of the first of the Buckets quarter hours ending
// with the one now is in. Buckets are aligned to the clock rather than to
// now, so a sparkline doesn't shift under the viewer between samples.
func bucketStart(now time.Time) int64 {
	return slotStart(now, bucketWidth) - int64((Buckets-1)*bucketWidth/time.Second)
}

func nanSeries() []float64 {
	s := make([]float64, Buckets)
	for i := range s {
		s[i] = math.NaN()
	}
	return s
}

// series is what a service's ring says about the last day.
type series struct {
	cpu, rss, in, out []float64
	in24, out24       uint64
	since             int64 // earliest slot held; 0 for none
}

// summarize reduces the ring to Buckets points each: the most a quarter hour
// asked of the machine, for cpu and memory, and everything that moved in it,
// for traffic. A quarter hour in which the service was never up is NaN for
// cpu and memory, so it draws as a gap rather than as an idle service.
func (r *ring) summarize(now time.Time) series {
	out := series{cpu: nanSeries(), rss: nanSeries(), in: nanSeries(), out: nanSeries()}
	step := int64(sampleEvery / time.Second)
	first := bucketStart(now)
	bw := int64(bucketWidth / time.Second)
	for i := range r {
		s := r[i]
		if s.at == 0 || !live(s.at, now) || r.index(s.at) != i {
			continue
		}
		out.in24 += s.in
		out.out24 += s.out
		if out.since == 0 || s.at < out.since {
			out.since = s.at
		}
	}
	for at := first; at <= now.Unix(); at += step {
		s, ok := r.get(at, now)
		if !ok {
			continue
		}
		b := int((at - first) / bw)
		// Traffic is counted at the gateway whether or not the process is up
		// (requests to a crashed app still cross it), so any that was counted
		// is drawn and the graph agrees with the day's totals. A down slot
		// with none stays a gap.
		if s.up || s.in > 0 || s.out > 0 {
			out.in[b] = sumKnown(out.in[b], float64(s.in))
			out.out[b] = sumKnown(out.out[b], float64(s.out))
		}
		if !s.up {
			continue
		}
		if !isNaN32(s.cpu) {
			out.cpu[b] = maxKnown(out.cpu[b], float64(s.cpu))
		}
		if s.mem {
			out.rss[b] = maxKnown(out.rss[b], float64(s.rss))
		}
	}
	return out
}

// summarize reduces the storage ring to the largest reading in each quarter
// hour. Storage exists whether or not the service runs, so there is no gap for
// a stopped one, only for a quarter hour no walk finished in.
func (r *storageRing) summarize(now time.Time) []float64 {
	out := nanSeries()
	first := bucketStart(now)
	bw := int64(bucketWidth / time.Second)
	step := int64(storageEvery / time.Second)
	// From the first storage slot that can fall in the first bucket.
	for at := first / step * step; at <= now.Unix(); at += step {
		s, ok := r.get(at, now)
		if !ok || at < first {
			continue
		}
		b := int((at - first) / bw)
		out[b] = maxKnown(out[b], float64(s.bytes))
	}
	return out
}

func maxKnown(a, b float64) float64 {
	if math.IsNaN(a) || b > a {
		return b
	}
	return a
}

func sumKnown(a, b float64) float64 {
	if math.IsNaN(a) {
		return b
	}
	return a + b
}

func isNaN32(f float32) bool { return math.IsNaN(float64(f)) }

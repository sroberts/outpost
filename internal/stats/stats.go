// Package stats keeps a day of usage history for each service: CPU, memory,
// storage and the traffic the gateway carried for it.
//
// It is a client of the supervisor like any other, reading the service list
// through its public List on a timer, and it answers reads from a snapshot
// of its own, so drawing a page never waits on the reconcile loop and the
// loop never waits on a measurement.
package stats

import (
	"context"
	"errors"
	"io/fs"
	"math"
	goruntime "runtime"
	"strconv"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/supervisor"
)

// listTimeout bounds asking the supervisor for its services. A loop busy with
// a slow start shouldn't stall sampling, so a tick that can't get the list
// samples the last one it had.
const listTimeout = 5 * time.Second

// Lister is what the sampler needs from the supervisor.
type Lister interface {
	List(ctx context.Context) ([]supervisor.ServiceStatus, error)
}

// Bytes is traffic to (In) and from (Out) a service, counted since the
// counter started. The sampler works from the difference between readings,
// and a reading lower than the last is taken as a counter that started over.
type Bytes struct {
	In, Out uint64
	// Source names the counter the reading came from. When it changes (a
	// service moved to a port that another one used before) the two readings
	// count different things, and no difference is taken between them.
	Source int
}

// Usage is what a service is using now and over the last day. It is built
// once per sample and shared between readers: don't modify its slices.
type Usage struct {
	// Up is whether the service's process is running.
	Up bool
	// Measured is whether CPU and RSS were read: false while the service is
	// down, and always on a platform the probe can't read.
	Measured bool
	// CPU is in cores, averaged over the last sample interval; a page shows
	// it as a percent of one core, as top does. NaN until there have been
	// two readings of the same process to take a difference between.
	CPU float64
	RSS uint64

	// Storage is the size of the service's storage directory at StorageAt.
	// It is measured whether or not the service runs. StoragePartial means
	// the walk stopped short, so the size is a lower bound.
	Storage        uint64
	StorageAt      time.Time
	StoragePartial bool

	// In24 and Out24 are the bytes the gateway carried to and from the
	// service over the last day.
	In24, Out24 uint64

	// Each series is Buckets quarter hours, oldest first, ending with the
	// current one. NaN is no data: the service was down, or the daemon
	// wasn't running. CPU, RSS and Storage hold each quarter's highest
	// reading; In and Out its total.
	CPUSeries, RSSSeries, StorageSeries, InSeries, OutSeries []float64

	// Since is the oldest sample the series draw on, so a page can say how
	// much of the day it knows about.
	Since time.Time
}

// Options configure a Sampler.
type Options struct {
	Services Lister
	// Probe reads CPU and memory; nil picks the one for this platform.
	Probe Probe
	// Traffic, when set, returns each service's cumulative gateway traffic.
	Traffic func() map[string]Bytes
	Now     func() time.Time
	Logf    func(format string, v ...any)
}

// Sampler measures every service on a timer and keeps the history.
type Sampler struct {
	list    Lister
	probe   Probe
	traffic func() map[string]Bytes
	now     func() time.Time
	logf    func(format string, v ...any)

	listTimeout time.Duration
	walkLimit   time.Duration
	entryLimit  int

	// mu guards everything the sampler and the storage walker write.
	mu       sync.Mutex
	targets  []target
	services map[string]*service
	lastErr  string

	// snapMu guards only the published snapshots, so a reader waits at most
	// for a map to be swapped, never for one to be built.
	snapMu sync.RWMutex
	snaps  map[string]Usage
}

// target is one service as the last List described it.
type target struct {
	name    string
	pid     int  // 0 when it has no process
	running bool // the supervisor believes it has a live process
	storage string
}

// service is the history of one service. Its rings are fixed-size, so the
// sampler's memory is bounded by the number of services, not the uptime.
type service struct {
	samples *ring
	storage *storageRing

	// The previous reading of the process, for the CPU difference.
	pid     int
	cpuTime time.Duration
	cpuAt   time.Time
	based   bool

	// The previous traffic reading, for the difference.
	traffic     Bytes
	trafficSeen bool

	// The latest reading, for Usage's current values.
	up       bool
	measured bool
	cpu      float64
	rss      uint64

	storagePath    string
	storageBytes   uint64
	storageAt      time.Time
	storagePartial bool
}

// New builds a Sampler. Run starts it.
func New(opts Options) *Sampler {
	s := &Sampler{
		list:        opts.Services,
		probe:       opts.Probe,
		traffic:     opts.Traffic,
		now:         opts.Now,
		logf:        opts.Logf,
		listTimeout: listTimeout,
		walkLimit:   walkLimit,
		entryLimit:  entryLimit,
		services:    map[string]*service{},
		snaps:       map[string]Usage{},
	}
	if s.probe == nil {
		s.probe = ProbeFor(goruntime.GOOS)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.logf == nil {
		s.logf = func(string, ...any) {}
	}
	return s
}

// Usage is the latest snapshot for a service, and false for one the sampler
// hasn't seen.
func (s *Sampler) Usage(name string) (Usage, bool) {
	s.snapMu.RLock()
	defer s.snapMu.RUnlock()
	u, ok := s.snaps[name]
	return u, ok
}

// Run samples every 30 seconds, and walks storage every 10 minutes, until ctx
// ends. It returns only once both have stopped, so the caller can close what
// List reads from after it.
func (s *Sampler) Run(ctx context.Context) {
	s.sampleOnce(ctx, s.now())

	walked := make(chan struct{})
	go func() {
		defer close(walked)
		s.walkLoop(ctx)
	}()
	defer func() { <-walked }()

	t := time.NewTicker(sampleEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sampleOnce(ctx, s.now())
		}
	}
}

// sampleOnce takes one sample of every service and publishes the result.
func (s *Sampler) sampleOnce(ctx context.Context, now time.Time) {
	lctx, cancel := context.WithTimeout(ctx, s.listTimeout)
	list, listErr := s.list.List(lctx)
	cancel()

	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	groups, probeErr := s.probe.Snapshot(pctx)
	cancel()

	var traffic map[string]Bytes
	if s.traffic != nil {
		traffic = s.traffic()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return // on the way down: an error now is the shutdown, not news
	}
	s.report(listErr, probeErr)
	if listErr == nil {
		s.targets = targetsOf(list)
	}

	keep := make(map[string]bool, len(s.targets))
	for _, t := range s.targets {
		keep[t.name] = true
		svc := s.services[t.name]
		if svc == nil {
			svc = &service{samples: new(ring), storage: new(storageRing)}
			s.services[t.name] = svc
		}
		svc.sample(t, groups, probeErr == nil, traffic, now)
	}
	// A service that has left the list is gone for good: its history goes
	// with it rather than lingering for a name that may be reused.
	for name := range s.services {
		if !keep[name] {
			delete(s.services, name)
		}
	}

	snaps := make(map[string]Usage, len(s.services))
	for name, svc := range s.services {
		snaps[name] = svc.usage(now)
	}
	// Swapped while mu is held, so a walker publishing at the same moment
	// can't put back an older map over this one.
	s.snapMu.Lock()
	s.snaps = snaps
	s.snapMu.Unlock()
}

// report logs a failure once, when it starts, rather than every 30 seconds
// while it lasts. Caller holds mu.
func (s *Sampler) report(listErr, probeErr error) {
	if errors.Is(probeErr, ErrUnsupported) {
		probeErr = nil // known at start, and the rest still works
	}
	msg := ""
	if err := errors.Join(listErr, probeErr); err != nil {
		msg = err.Error()
	}
	if msg != "" && msg != s.lastErr {
		s.logf("stats: %s", msg)
	}
	s.lastErr = msg
}

func targetsOf(list []supervisor.ServiceStatus) []target {
	out := make([]target, 0, len(list))
	for _, st := range list {
		// A removed service's row is history the gateway never shows, and
		// nothing supervises its process: no ring is kept for it.
		if st.Status == supervisor.StatusRemoved {
			continue
		}
		t := target{name: st.Name, storage: st.Storage}
		switch runtime.Status(st.Status) {
		case runtime.StatusStarting, runtime.StatusRunning, runtime.StatusUnhealthy:
			t.running = true
		}
		// The runtime starts each service as the leader of its own process
		// group, so its pid is also the group's id.
		if pid, err := strconv.Atoi(st.InstanceID); err == nil && pid > 1 {
			t.pid = pid
		}
		out = append(out, t)
	}
	return out
}

// sample records one interval for the service.
func (svc *service) sample(t target, groups map[int]Group, probed bool, traffic map[string]Bytes, now time.Time) {
	if t.storage != svc.storagePath {
		// Another directory's history says nothing about this one.
		svc.storage = new(storageRing)
		svc.storagePath = t.storage
		svc.storageBytes, svc.storageAt, svc.storagePartial = 0, time.Time{}, false
	}

	s := slot{cpu: float32(math.NaN())}
	up := t.running && t.pid > 0
	var g Group
	if up && probed {
		// The supervisor's record can lag a crash, and a pid can be reused:
		// only a group still led by that pid is the service.
		var ok bool
		g, ok = groups[t.pid]
		up = ok && g.Leader
	}
	s.up = up
	svc.up = up
	svc.measured = up && probed
	svc.cpu, svc.rss = math.NaN(), 0
	if svc.measured {
		s.rss, s.mem = g.RSS, true
		svc.rss = g.RSS
		dt := now.Sub(svc.cpuAt)
		// A new pid is a new process: the difference from the old one's
		// time means nothing, so that interval has no CPU reading. So does
		// one in which the group lost more time to exits than it gained.
		if svc.based && svc.pid == t.pid && dt > 0 && g.CPUTime >= svc.cpuTime {
			svc.cpu = float64(g.CPUTime-svc.cpuTime) / float64(dt)
			s.cpu = float32(svc.cpu)
		}
		svc.pid, svc.cpuTime, svc.cpuAt, svc.based = t.pid, g.CPUTime, now, true
	} else {
		svc.based = false
	}
	if !up {
		svc.cpu = 0
	}

	if b, ok := traffic[t.name]; ok {
		if svc.trafficSeen && svc.traffic.Source == b.Source {
			s.in = delta(svc.traffic.In, b.In)
			s.out = delta(svc.traffic.Out, b.Out)
		}
		svc.traffic, svc.trafficSeen = b, true
	} else {
		// Not routed this tick. Starting again from the next reading means
		// bytes from a counter that has meanwhile passed to another service
		// are never put down to this one.
		svc.trafficSeen = false
	}
	svc.samples.put(now, s)
}

// delta is the growth of a cumulative counter, with a counter that went down
// taken to have started again from zero.
func delta(prev, cur uint64) uint64 {
	if cur < prev {
		return cur
	}
	return cur - prev
}

// usage builds the service's snapshot.
func (svc *service) usage(now time.Time) Usage {
	ser := svc.samples.summarize(now)
	u := Usage{
		Up:             svc.up,
		Measured:       svc.measured,
		CPU:            svc.cpu,
		RSS:            svc.rss,
		Storage:        svc.storageBytes,
		StorageAt:      svc.storageAt,
		StoragePartial: svc.storagePartial,
		In24:           ser.in24,
		Out24:          ser.out24,
		CPUSeries:      ser.cpu,
		RSSSeries:      ser.rss,
		StorageSeries:  svc.storage.summarize(now),
		InSeries:       ser.in,
		OutSeries:      ser.out,
	}
	if ser.since != 0 {
		u.Since = time.Unix(ser.since, 0)
	}
	return u
}

// walkLoop measures storage once at the start and then every 10 minutes.
func (s *Sampler) walkLoop(ctx context.Context) {
	// The first walk waits for the first list. That can fail at startup,
	// while the loop is busy, and a walk with nothing to walk would leave
	// storage blank for ten minutes.
	for !s.listed() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(sampleEvery):
		}
	}
	t := time.NewTicker(storageEvery)
	defer t.Stop()
	for {
		s.walkAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// listed says whether List has answered yet. targetsOf never returns nil, so
// nil targets means it hasn't.
func (s *Sampler) listed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targets != nil
}

// walkAll measures each service's storage in turn. One at a time, because a
// walk is disk-bound and doing several at once would only make them all slow.
func (s *Sampler) walkAll(ctx context.Context) {
	s.mu.Lock()
	targets := append([]target(nil), s.targets...)
	s.mu.Unlock()
	for _, t := range targets {
		if ctx.Err() != nil {
			return
		}
		if t.storage == "" {
			continue
		}
		u, err := measure(ctx, t.storage, s.walkLimit, s.entryLimit, s.now)
		if err != nil {
			// A storage directory that doesn't exist yet belongs to a service
			// that hasn't started: nothing to measure, and nothing to log
			// every ten minutes until it does.
			if ctx.Err() == nil && !errors.Is(err, fs.ErrNotExist) {
				s.logf("stats: %s: storage: %v", t.name, err)
			}
			continue
		}
		s.recordStorage(t, u, s.now())
	}
}

// recordStorage files a walk's result and republishes that service's
// snapshot, so a new size shows without waiting for the next sample.
func (s *Sampler) recordStorage(t target, u usage, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	svc := s.services[t.name]
	if svc == nil || svc.storagePath != t.storage {
		return // removed, or moved, while the walk ran
	}
	svc.storage.put(now, u.bytes, u.partial)
	svc.storageBytes, svc.storageAt, svc.storagePartial = u.bytes, now, u.partial

	// A new Usage replaces the old one whole: readers hold copies of the
	// old, and its slices are never written again.
	snap := svc.usage(now)
	s.snapMu.Lock()
	s.snaps[t.name] = snap
	s.snapMu.Unlock()
}

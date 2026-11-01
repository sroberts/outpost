package stats

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sroberts/outpost/internal/supervisor"
)

type fakeLister struct {
	mu    sync.Mutex
	list  []supervisor.ServiceStatus
	err   error
	block bool // wait for the deadline, as a busy loop would
	calls int
}

func (f *fakeLister) List(ctx context.Context) ([]supervisor.ServiceStatus, error) {
	f.mu.Lock()
	f.calls++
	block, err := f.block, f.err
	list := append([]supervisor.ServiceStatus(nil), f.list...)
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return list, err
}

func (f *fakeLister) set(list ...supervisor.ServiceStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.list = list
}

type fakeProbe struct {
	mu     sync.Mutex
	groups map[int]Group
	err    error
}

func (f *fakeProbe) Snapshot(context.Context) (map[int]Group, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[int]Group{}
	for k, v := range f.groups {
		out[k] = v
	}
	return out, f.err
}

func (f *fakeProbe) set(groups map[int]Group) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.groups = groups
}

func running(name, pid string) supervisor.ServiceStatus {
	return supervisor.ServiceStatus{Name: name, Status: "running", InstanceID: pid}
}

type rig struct {
	s       *Sampler
	list    *fakeLister
	probe   *fakeProbe
	mu      sync.Mutex
	traffic map[string]Bytes
}

func newRig(t *testing.T, list ...supervisor.ServiceStatus) *rig {
	t.Helper()
	r := &rig{list: &fakeLister{list: list}, probe: &fakeProbe{}}
	r.s = New(Options{
		Services: r.list,
		Probe:    r.probe,
		Traffic: func() map[string]Bytes {
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.traffic
		},
		Now:  func() time.Time { return t0 },
		Logf: t.Logf,
	})
	return r
}

func (r *rig) setTraffic(m map[string]Bytes) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.traffic = m
}

func (r *rig) usage(t *testing.T, name string) Usage {
	t.Helper()
	u, ok := r.s.Usage(name)
	if !ok {
		t.Fatalf("no usage for %s", name)
	}
	return u
}

func TestSampler_CPUFromTheDifference(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.probe.set(map[int]Group{100: {CPUTime: time.Second, RSS: 4096, Leader: true}})
	r.s.sampleOnce(context.Background(), t0)

	u := r.usage(t, "app")
	if !u.Up || !u.Measured || u.RSS != 4096 || !math.IsNaN(u.CPU) {
		t.Errorf("first sample = up %v measured %v rss %d cpu %v; want cpu unknown", u.Up, u.Measured, u.RSS, u.CPU)
	}

	r.probe.set(map[int]Group{100: {CPUTime: 16 * time.Second, RSS: 8192, Leader: true}})
	r.s.sampleOnce(context.Background(), t0.Add(30*time.Second))
	u = r.usage(t, "app")
	if u.CPU != 0.5 || u.RSS != 8192 {
		t.Errorf("cpu %v rss %d; want 0.5 cores, 8192", u.CPU, u.RSS)
	}
	if !u.Since.Equal(t0) {
		t.Errorf("since = %v; want %v", u.Since, t0)
	}
}

func TestSampler_NewPIDResetsTheBaseline(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.probe.set(map[int]Group{100: {CPUTime: 50 * time.Second, Leader: true}})
	r.s.sampleOnce(context.Background(), t0)

	// Restarted: a new process with less time than the old one had.
	r.list.set(running("app", "200"))
	r.probe.set(map[int]Group{200: {CPUTime: time.Second, Leader: true}})
	r.s.sampleOnce(context.Background(), t0.Add(30*time.Second))
	if u := r.usage(t, "app"); !u.Up || !math.IsNaN(u.CPU) {
		t.Errorf("after a restart cpu = %v; want unknown for that interval", u.CPU)
	}

	r.probe.set(map[int]Group{200: {CPUTime: 31 * time.Second, Leader: true}})
	r.s.sampleOnce(context.Background(), t0.Add(60*time.Second))
	if u := r.usage(t, "app"); u.CPU != 1 {
		t.Errorf("cpu = %v; want 1 core from the new process's baseline", u.CPU)
	}
}

func TestSampler_ListTimeoutReusesTargets(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.s.listTimeout = 10 * time.Millisecond
	r.probe.set(map[int]Group{100: {CPUTime: time.Second, Leader: true}})
	r.s.sampleOnce(context.Background(), t0)

	r.list.mu.Lock()
	r.list.block = true
	r.list.mu.Unlock()
	r.probe.set(map[int]Group{100: {CPUTime: 4 * time.Second, Leader: true}})
	r.s.sampleOnce(context.Background(), t0.Add(30*time.Second))
	if u := r.usage(t, "app"); !u.Up || u.CPU != 0.1 {
		t.Errorf("with List timing out: up %v cpu %v; want the last targets sampled", u.Up, u.CPU)
	}
}

func TestSampler_PIDNotLeadingItsGroupIsDown(t *testing.T) {
	r := newRig(t, running("app", "100"), running("gone", "300"))
	// 100's group lives on without it; 300 isn't there at all.
	r.probe.set(map[int]Group{100: {CPUTime: time.Second, RSS: 10, Leader: false}})
	r.s.sampleOnce(context.Background(), t0)
	for _, name := range []string{"app", "gone"} {
		if u := r.usage(t, name); u.Up || u.Measured || u.CPU != 0 || u.RSS != 0 {
			t.Errorf("%s = up %v measured %v cpu %v rss %d; want down", name, u.Up, u.Measured, u.CPU, u.RSS)
		}
	}
	if u := r.usage(t, "app"); !math.IsNaN(u.CPUSeries[Buckets-1]) {
		t.Error("a down interval should draw as a gap")
	}
}

func TestSampler_StoppedAndUnsupported(t *testing.T) {
	r := newRig(t, supervisor.ServiceStatus{Name: "app", Status: "stopped"}, running("other", "100"))
	r.probe.err = ErrUnsupported
	r.s.sampleOnce(context.Background(), t0)
	if u := r.usage(t, "app"); u.Up {
		t.Error("a stopped service is up")
	}
	// No probe: up on the supervisor's word, with nothing measured.
	if u := r.usage(t, "other"); !u.Up || u.Measured || !math.IsNaN(u.CPU) {
		t.Errorf("unsupported probe: up %v measured %v cpu %v", u.Up, u.Measured, u.CPU)
	}
}

func TestSampler_RemovedServiceIsDropped(t *testing.T) {
	r := newRig(t, running("app", "100"), running("old", "200"))
	r.s.sampleOnce(context.Background(), t0)
	if _, ok := r.s.Usage("old"); !ok {
		t.Fatal("no usage for old")
	}
	r.list.set(running("app", "100"))
	r.s.sampleOnce(context.Background(), t0.Add(30*time.Second))
	if _, ok := r.s.Usage("old"); ok {
		t.Error("a service gone from the list still has usage")
	}
	r.s.mu.Lock()
	n := len(r.s.services)
	r.s.mu.Unlock()
	if n != 1 {
		t.Errorf("%d services held; want 1", n)
	}
}

func TestSampler_TrafficDeltas(t *testing.T) {
	r := newRig(t, running("app", "100"))
	r.probe.set(map[int]Group{100: {Leader: true}})
	step := func(i int, b Bytes) Usage {
		r.setTraffic(map[string]Bytes{"app": b})
		r.s.sampleOnce(context.Background(), t0.Add(time.Duration(i)*sampleEvery))
		return r.usage(t, "app")
	}
	if u := step(0, Bytes{In: 100, Out: 50}); u.In24 != 0 || u.Out24 != 0 {
		t.Errorf("the first reading is a baseline: in %d out %d", u.In24, u.Out24)
	}
	if u := step(1, Bytes{In: 300, Out: 80}); u.In24 != 200 || u.Out24 != 30 {
		t.Errorf("in %d out %d; want 200, 30", u.In24, u.Out24)
	}
	// The counter started over.
	if u := step(2, Bytes{In: 40, Out: 10}); u.In24 != 240 || u.Out24 != 40 {
		t.Errorf("after a reset in %d out %d; want 240, 40", u.In24, u.Out24)
	}
	// Unrouted for a tick: the next reading is a fresh baseline.
	r.setTraffic(nil)
	r.s.sampleOnce(context.Background(), t0.Add(3*sampleEvery))
	if u := step(4, Bytes{In: 1000, Out: 1000}); u.In24 != 240 || u.Out24 != 40 {
		t.Errorf("after a gap in %d out %d; want 240, 40", u.In24, u.Out24)
	}
	u := r.usage(t, "app")
	if u.InSeries[Buckets-1] != 240 || u.OutSeries[Buckets-1] != 40 {
		t.Errorf("series in %v out %v", u.InSeries[Buckets-1], u.OutSeries[Buckets-1])
	}
}

func TestSampler_StorageWalk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "data"), make([]byte, 64<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	st := running("app", "100")
	st.Storage = dir
	r := newRig(t, st, supervisor.ServiceStatus{Name: "nostore", Status: "stopped"})
	r.s.sampleOnce(context.Background(), t0)
	r.s.walkAll(context.Background())

	u := r.usage(t, "app")
	if u.Storage < 64<<10 || u.StoragePartial || !u.StorageAt.Equal(t0) {
		t.Errorf("storage = %d partial %v at %v", u.Storage, u.StoragePartial, u.StorageAt)
	}
	if u.StorageSeries[Buckets-1] != float64(u.Storage) {
		t.Errorf("storage series ends %v; want %d", u.StorageSeries[Buckets-1], u.Storage)
	}
	// The next sample keeps it.
	r.s.sampleOnce(context.Background(), t0.Add(sampleEvery))
	if got := r.usage(t, "app"); got.Storage != u.Storage || !got.StorageAt.Equal(t0) {
		t.Errorf("after a sample storage = %d at %v; want %d at %v", got.Storage, got.StorageAt, u.Storage, t0)
	}
}

func TestSampler_RunStopsWithItsContext(t *testing.T) {
	r := newRig(t, running("app", "100"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); r.s.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := r.s.Usage("app"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never sampled")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
}

func TestSampler_ListErrorIsLoggedOnce(t *testing.T) {
	var logged []string
	r := newRig(t)
	r.list.err = errors.New("loop busy")
	r.s.logf = func(format string, v ...any) { logged = append(logged, format) }
	for i := range 3 {
		r.s.sampleOnce(context.Background(), t0.Add(time.Duration(i)*sampleEvery))
	}
	if len(logged) != 1 {
		t.Errorf("logged %d times; want once", len(logged))
	}
}

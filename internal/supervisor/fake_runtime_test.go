package supervisor

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/sroberts/outpost/internal/manifest"
	"github.com/sroberts/outpost/internal/runtime"
)

// fakeHandle is an in-memory stand-in for a running service.
type fakeHandle struct {
	name string
	pid  int

	mu        sync.Mutex
	status    runtime.Status
	exitCode  int
	cleanStop bool
	done      chan struct{}
	closed    bool
}

func (h *fakeHandle) ID() string            { return fmt.Sprintf("%d", h.pid) }
func (h *fakeHandle) PID() int              { return h.pid }
func (h *fakeHandle) Done() <-chan struct{} { return h.done }

func (h *fakeHandle) Status() runtime.Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *fakeHandle) ExitCode() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

func (h *fakeHandle) CleanStop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleanStop
}

// exit terminates the fake instance; clean distinguishes a stop from a crash.
func (h *fakeHandle) exit(code int, clean bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.exitCode, h.cleanStop, h.closed = code, clean, true
	if clean {
		h.status = runtime.StatusStopped
	} else {
		h.status = runtime.StatusFailed
	}
	close(h.done)
}

// fakeRuntime records what the supervisor asked for, and lets a test make
// services crash or go unhealthy on demand.
type fakeRuntime struct {
	mu       sync.Mutex
	starts   map[string]int
	stops    map[string]int
	live     map[string]*fakeHandle
	healthy  map[string]bool
	startErr map[string]error
	nextPID  int
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{
		starts:   map[string]int{},
		stops:    map[string]int{},
		live:     map[string]*fakeHandle{},
		healthy:  map[string]bool{},
		startErr: map[string]error{},
		nextPID:  1000,
	}
}

func (r *fakeRuntime) Start(ctx context.Context, svc *manifest.Service) (runtime.Handle, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.startErr[svc.Name]; err != nil {
		return nil, err
	}
	r.nextPID++
	h := &fakeHandle{name: svc.Name, pid: r.nextPID, status: runtime.StatusRunning, exitCode: -1, done: make(chan struct{})}
	r.starts[svc.Name]++
	r.live[svc.Name] = h
	r.healthy[svc.Name] = true
	return h, nil
}

func (r *fakeRuntime) Stop(ctx context.Context, handle runtime.Handle) error {
	h, ok := handle.(*fakeHandle)
	if !ok {
		return fmt.Errorf("not a fake handle")
	}
	r.mu.Lock()
	r.stops[h.name]++
	r.mu.Unlock()
	h.exit(0, true)
	return nil
}

func (r *fakeRuntime) Healthy(ctx context.Context, handle runtime.Handle) (bool, error) {
	h, ok := handle.(*fakeHandle)
	if !ok {
		return false, fmt.Errorf("not a fake handle")
	}
	select {
	case <-h.done:
		return false, fmt.Errorf("%s: exited", h.name)
	default:
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.healthy[h.name] {
		return false, fmt.Errorf("%s: unhealthy", h.name)
	}
	return true, nil
}

func (r *fakeRuntime) Logs(handle runtime.Handle) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("fake logs\n")), nil
}

// --- test controls ---

func (r *fakeRuntime) startCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.starts[name]
}

func (r *fakeRuntime) stopCount(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stops[name]
}

func (r *fakeRuntime) handle(name string) *fakeHandle {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live[name]
}

func (r *fakeRuntime) setHealthy(name string, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.healthy[name] = ok
}

func (r *fakeRuntime) failStart(name string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startErr[name] = err
}

// crash makes the currently-running instance exit non-zero.
func (r *fakeRuntime) crash(name string, code int) {
	if h := r.handle(name); h != nil {
		h.exit(code, false)
	}
}

// exitClean makes the instance exit zero without the supervisor asking.
func (r *fakeRuntime) exitClean(name string) {
	if h := r.handle(name); h != nil {
		h.exit(0, false) // not user-initiated: a crash-shaped exit with code 0
	}
}

var _ runtime.Runtime = (*fakeRuntime)(nil)

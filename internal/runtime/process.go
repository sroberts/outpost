package runtime

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/manifest"
)

const (
	defaultStopGrace     = 10 * time.Second
	defaultHealthTimeout = 2 * time.Second
)

// ProcessRuntime runs services as native child processes (§6).
type ProcessRuntime struct {
	// LogDir holds <service>.log files; defaults to ~/.outpost/logs.
	LogDir string
	// StopGrace is how long a service gets to exit after SIGTERM before it is
	// sent SIGKILL.
	StopGrace time.Duration
	// HealthTimeout bounds a single healthcheck request.
	HealthTimeout time.Duration
	// Env is KEY=value pairs every service gets, after its manifest's own
	// env so a manifest can't override them.
	Env []string

	clientOnce sync.Once
	client     *http.Client
}

// processHandle tracks one running child process.
type processHandle struct {
	svc     *manifest.Service
	cmd     *exec.Cmd
	logPath string
	logFile *rotatingWriter

	done chan struct{}

	mu        sync.Mutex
	status    Status
	exitCode  int
	exitErr   error
	cleanStop bool
}

func (h *processHandle) ID() string { return fmt.Sprintf("%d", h.PID()) }

func (h *processHandle) PID() int {
	if h.cmd == nil || h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

func (h *processHandle) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.status
}

func (h *processHandle) setStatus(s Status) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.status = s
}

func (h *processHandle) Done() <-chan struct{} { return h.done }

func (h *processHandle) ExitCode() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode
}

func (h *processHandle) ExitErr() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitErr
}

func (h *processHandle) CleanStop() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cleanStop
}

// markStopping records that an exit from here on is user-initiated.
func (h *processHandle) markStopping() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.cleanStop = true
	h.status = StatusStopped
}

func (r *ProcessRuntime) logDir() string {
	if r.LogDir != "" {
		return r.LogDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "outpost", "logs")
	}
	return filepath.Join(home, ".outpost", "logs")
}

func (r *ProcessRuntime) httpClient() *http.Client {
	r.clientOnce.Do(func() {
		timeout := r.HealthTimeout
		if timeout <= 0 {
			timeout = defaultHealthTimeout
		}
		r.client = &http.Client{Timeout: timeout}
	})
	return r.client
}

// binaryPath makes a relative binary path such as bin/svc absolute against the
// daemon's working directory, where it was always resolved, since exec would
// otherwise resolve it against cmd.Dir. A bare name is left for PATH.
func binaryPath(binary string) string {
	if filepath.IsAbs(binary) || !strings.ContainsRune(binary, filepath.Separator) {
		return binary
	}
	if abs, err := filepath.Abs(binary); err == nil {
		return abs
	}
	return binary
}

// Start launches the service and begins reaping it in the background.
func (r *ProcessRuntime) Start(ctx context.Context, svc *manifest.Service) (Handle, error) {
	if svc.Binary == "" {
		return nil, fmt.Errorf("%s: binary is empty", svc.Name)
	}
	if err := svc.EnsureStorage(); err != nil {
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}
	// Fail before forking rather than reporting a start that immediately dies.
	if _, err := exec.LookPath(svc.Binary); err != nil {
		return nil, fmt.Errorf("%s: binary %s: %w", svc.Name, svc.Binary, err)
	}

	logPath := filepath.Join(r.logDir(), svc.Name+".log")
	lw, err := newRotatingWriter(logPath, defaultMaxLogBytes, defaultLogBackups)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", svc.Name, err)
	}

	// Deliberately not exec.CommandContext: the supervisor owns the service
	// lifecycle through Stop, and a cancelled reconcile context must not take
	// running services down with it.
	cmd := exec.Command(binaryPath(svc.Binary), svc.Args...)
	cmd.Args[0] = svc.Binary // what the service sees as argv[0] is unchanged
	// The service's own directory rather than the daemon's, which the unit
	// files set to the Outpost home: an app writing ./config.toml must not
	// overwrite Outpost's.
	cmd.Dir = svc.ExpandStorage()
	cmd.Stdout = lw
	cmd.Stderr = lw
	cmd.Env = buildEnv(svc, r.Env)
	// Own process group, so Stop can signal the service and anything it spawns.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	h := &processHandle{
		svc:      svc,
		cmd:      cmd,
		logPath:  logPath,
		logFile:  lw,
		done:     make(chan struct{}),
		status:   StatusStarting,
		exitCode: -1,
	}

	if err := cmd.Start(); err != nil {
		lw.Close()
		return nil, fmt.Errorf("%s: start %s: %w", svc.Name, svc.Binary, err)
	}
	h.setStatus(StatusRunning)

	go h.reap()
	return h, nil
}

// reap waits for the child and records how it ended.
func (h *processHandle) reap() {
	err := h.cmd.Wait()

	h.mu.Lock()
	h.exitErr = err
	if h.cmd.ProcessState != nil {
		h.exitCode = h.cmd.ProcessState.ExitCode()
	}
	if h.cleanStop {
		h.status = StatusStopped
	} else {
		h.status = StatusFailed
	}
	h.mu.Unlock()

	h.logFile.Close()
	close(h.done)
}

// buildEnv layers the manifest's env, then the runtime's extra env and
// OUTPOST_STORAGE_DIR, over the daemon's own environment (§2).
func buildEnv(svc *manifest.Service, extra []string) []string {
	env := append([]string{}, os.Environ()...)
	for k, v := range svc.Env {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}
	env = append(env, extra...)
	// Appended last so a service can never be handed the wrong storage path.
	env = append(env, "OUTPOST_STORAGE_DIR="+svc.ExpandStorage())
	return env
}

// Stop asks the service to exit, escalating to SIGKILL after StopGrace.
func (r *ProcessRuntime) Stop(ctx context.Context, handle Handle) error {
	h, ok := handle.(*processHandle)
	if !ok {
		return fmt.Errorf("stop: handle is %T, not a process handle", handle)
	}

	select {
	case <-h.done:
		return nil // already exited
	default:
	}

	h.markStopping()

	grace := r.StopGrace
	if grace <= 0 {
		grace = defaultStopGrace
	}

	// Negative PID targets the whole process group.
	if err := syscall.Kill(-h.PID(), syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return fmt.Errorf("%s: SIGTERM: %w", h.svc.Name, err)
	}

	// Wait out the full grace period. A cancelled caller context must not
	// shorten it: SIGTERM-then-immediate-SIGKILL is exactly the ungraceful stop
	// StopGrace exists to prevent, and the caller going away is no reason to
	// deny the service its chance to flush state.
	select {
	case <-h.done:
		return nil
	case <-time.After(grace):
	}

	if err := syscall.Kill(-h.PID(), syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		return fmt.Errorf("%s: SIGKILL: %w", h.svc.Name, err)
	}
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		return fmt.Errorf("%s: still running after SIGKILL", h.svc.Name)
	}
	return nil
}

// Healthy probes the service's healthcheck on loopback. A non-2xx response or a
// transport error is a failed check, and the error explains which (§2).
func (r *ProcessRuntime) Healthy(ctx context.Context, handle Handle) (bool, error) {
	h, ok := handle.(*processHandle)
	if !ok {
		return false, fmt.Errorf("healthy: handle is %T, not a process handle", handle)
	}

	select {
	case <-h.done:
		return false, fmt.Errorf("%s: process has exited", h.svc.Name)
	default:
	}

	url := fmt.Sprintf("http://127.0.0.1:%d%s", h.svc.Port, h.svc.Healthcheck)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return false, fmt.Errorf("%s: healthcheck %s: %w", h.svc.Name, url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return false, fmt.Errorf("%s: healthcheck %s returned %s", h.svc.Name, url, resp.Status)
	}
	return true, nil
}

// Logs opens the service's current log file for reading.
func (r *ProcessRuntime) Logs(handle Handle) (io.ReadCloser, error) {
	h, ok := handle.(*processHandle)
	if !ok {
		return nil, fmt.Errorf("logs: handle is %T, not a process handle", handle)
	}
	f, err := os.Open(h.logPath)
	if err != nil {
		return nil, fmt.Errorf("%s: open log: %w", h.svc.Name, err)
	}
	return f, nil
}

// LogPath reports where a service's logs are written, for CLI use.
func (r *ProcessRuntime) LogPath(service string) string {
	return filepath.Join(r.logDir(), service+".log")
}

var _ Runtime = (*ProcessRuntime)(nil)

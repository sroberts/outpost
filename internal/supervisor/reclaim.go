package supervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sroberts/outpost/internal/state"
)

// reclaimGrace is how long an orphan gets to exit after SIGTERM.
const reclaimGrace = 5 * time.Second

// ReclaimOrphans stops services left running by a previous daemon that did not
// shut down cleanly, and reports which ones it stopped.
//
// A daemon killed with SIGKILL — an OOM kill, a power cut, `kill -9` — never
// runs its shutdown path, so its children survive, reparented to init, still
// holding their ports. A replacement daemon then starts a second instance that
// cannot bind, and restart-loops it forever while the orphan serves traffic
// unsupervised. Call this before the reconcile loop starts.
//
// Orphans are stopped rather than adopted: a process that is not our child
// cannot be waited on, so the supervisor could not observe its exit.
func (s *Supervisor) ReclaimOrphans(ctx context.Context) ([]string, error) {
	records, err := s.store.ListServices()
	if err != nil {
		return nil, fmt.Errorf("read state for orphan reclaim: %w", err)
	}

	var reclaimed []string
	for _, rec := range records {
		if rec.InstanceID == "" {
			continue
		}
		pid, err := strconv.Atoi(rec.InstanceID)
		if err != nil || pid <= 1 {
			continue // a container id, or nothing usable
		}

		svc, declared := s.desired[rec.Name]
		if !declared {
			// No manifest means nothing to compare the process against, and
			// killing on a bare PID match is not safe enough.
			continue
		}
		if !s.isOurProcess(pid, svc.Binary) {
			continue
		}

		s.logf("%s: stopping orphan from a previous run (pid %d)", rec.Name, pid)
		if err := stopPID(pid, reclaimGrace); err != nil {
			s.recordError(rec.Name, fmt.Sprintf("reclaim pid %d: %v", pid, err))
			continue
		}

		s.event(rec.Name, state.EventStopped,
			fmt.Sprintf("reclaimed orphan from a previous run (pid %d)", pid))
		// Clear the stale instance so status stops reporting a dead PID.
		rec.InstanceID = ""
		rec.Status = string(statusStopped)
		if err := s.store.UpsertService(rec); err != nil {
			s.logf("outpost: clear stale instance for %s: %v", rec.Name, err)
		}
		reclaimed = append(reclaimed, rec.Name)
	}
	return reclaimed, nil
}

// isOurProcess reports whether pid is alive and running the given binary.
//
// PIDs are recycled, so a bare liveness check is not enough: killing a stranger
// that inherited a recorded PID would be far worse than leaving an orphan.
func (s *Supervisor) isOurProcess(pid int, binary string) bool {
	if !pidAlive(pid) {
		return false
	}
	cmdline, err := processCommandLine(pid)
	if err != nil {
		return false
	}
	return binary != "" && strings.Contains(cmdline, binary)
}

func pidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 probes for existence without delivering anything.
	return proc.Signal(syscall.Signal(0)) == nil
}

// processCommandLine returns the full argv of a process.
//
// It shells out to ps, which exists on both macOS and Linux with the same flags
// and needs no per-platform build tags — the same trade the container runtime
// makes for the engine CLI (§9).
func processCommandLine(pid int) (string, error) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return "", fmt.Errorf("inspect pid %d: %w", pid, err)
	}
	cmdline := strings.TrimSpace(string(out))
	if cmdline == "" {
		return "", fmt.Errorf("no such process: %d", pid)
	}
	return cmdline, nil
}

// stopPID asks a process group to exit, escalating to SIGKILL after grace.
func stopPID(pid int, grace time.Duration) error {
	// Services are started in their own process group, so signal the group to
	// catch anything the service spawned. ESRCH here means no such *group* —
	// the process may still exist without leading one — so fall through to
	// signalling it directly rather than assuming it is gone.
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			if err == syscall.ESRCH {
				return nil // genuinely gone
			}
			return fmt.Errorf("SIGTERM: %w", err)
		}
	}

	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Best effort: the process may already have gone between the liveness
	// check and here, and either way there is nothing left to try.
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("pid %d still running after SIGKILL", pid)
}

// Package runtime starts, stops and observes the things Outpost supervises.
// A Runtime knows how to turn a manifest.Service into a running Handle; it does
// not decide whether something should be running — that is the supervisor's job.
package runtime

import (
	"context"
	"io"

	"github.com/sroberts/outpost/internal/manifest"
)

// Status is a service's observed lifecycle state (§6).
type Status string

const (
	StatusStarting  Status = "starting"
	StatusRunning   Status = "running"
	StatusUnhealthy Status = "unhealthy"
	StatusStopped   Status = "stopped"
	StatusFailed    Status = "failed"
)

// Handle is a reference to one started service instance. Implementations are
// safe for concurrent use.
type Handle interface {
	// ID identifies the running instance: its PID.
	ID() string
	PID() int
	Status() Status

	// Done is closed once the instance has exited.
	Done() <-chan struct{}

	// ExitCode is meaningful only after Done is closed; it is -1 while running
	// or if the instance was killed by a signal.
	ExitCode() int

	// CleanStop reports whether the exit was user-initiated via Stop rather
	// than a crash. The supervisor keys its restart decision off this (§6.3).
	CleanStop() bool
}

// Runtime is the boundary the supervisor drives. ProcessRuntime implements it
// with os/exec, and is the only implementation: Outpost supervises native
// processes, not containers.
type Runtime interface {
	Start(ctx context.Context, svc *manifest.Service) (Handle, error)
	Stop(ctx context.Context, h Handle) error
	Healthy(ctx context.Context, h Handle) (bool, error)
	Logs(h Handle) (io.ReadCloser, error)
}

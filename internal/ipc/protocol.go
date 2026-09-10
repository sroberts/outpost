// Package ipc carries CLI requests to the running Outpost daemon over a local
// Unix socket (§12), so the CLI, the TUI and future integrations all go through
// internal/supervisor's public API instead of duplicating its logic.
//
// The wire format is one JSON request per connection and one JSON response
// back. That is enough for the control commands; log following stays a direct
// file read on the client side, since streaming it would need a framed
// protocol for no practical gain on a single-machine socket.
package ipc

import (
	"encoding/json"
	"time"

	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/supervisor"
)

// Request commands.
const (
	CmdList    = "list"
	CmdStatus  = "status"
	CmdStart   = "start"
	CmdStop    = "stop"
	CmdRestart = "restart"
	CmdApply   = "apply"
	CmdEvents  = "events"
	CmdPing    = "ping"
	CmdBackup  = "backup"
	CmdRestore = "restore"
	CmdBackups = "backups"
)

// Timeouts. Most commands are near-instant queries against the supervisor's
// in-memory state, so a short bound is the right way to notice a wedged
// daemon. Backup and restore are different in kind: they copy a service's
// entire storage directory, and how long that takes is a property of how much
// data the operator has, not of whether anything is wrong.
const (
	// RequestTimeout bounds an ordinary command.
	RequestTimeout = 30 * time.Second
	// TransferTimeout bounds one that moves a service's data. Generous on
	// purpose: cutting a working backup off partway through and reporting a
	// failure is far worse than waiting.
	TransferTimeout = 2 * time.Hour
)

// Timeout returns how long the given command is allowed to take.
func Timeout(command string) time.Duration {
	switch command {
	case CmdBackup, CmdRestore:
		return TransferTimeout
	default:
		return RequestTimeout
	}
}

// Request is what the CLI sends.
type Request struct {
	Command string `json:"command"`
	Service string `json:"service,omitempty"`
	Limit   int    `json:"limit,omitempty"`
	// Timestamp selects a backup for restore; empty means the newest.
	Timestamp string `json:"timestamp,omitempty"`
}

// Response is what the daemon sends back. Error is set iff OK is false.
type Response struct {
	OK       bool                       `json:"ok"`
	Error    string                     `json:"error,omitempty"`
	Services []supervisor.ServiceStatus `json:"services,omitempty"`
	Status   *supervisor.ServiceStatus  `json:"status,omitempty"`
	Events   []supervisor.EventRecord   `json:"events,omitempty"`
	Archives []backup.Archive           `json:"archives,omitempty"`
	Message  string                     `json:"message,omitempty"`
}

// errorResponse builds a failure reply.
func errorResponse(err error) Response {
	return Response{OK: false, Error: err.Error()}
}

// decodeRequest is a small helper shared by the server and its tests.
func decodeRequest(b []byte) (Request, error) {
	var req Request
	err := json.Unmarshal(b, &req)
	return req, err
}

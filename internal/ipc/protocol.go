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

	"github.com/sroberts/outpost/internal/auth"
	"github.com/sroberts/outpost/internal/backup"
	"github.com/sroberts/outpost/internal/gateway"
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

	CmdUserAdd     = "user.add"
	CmdUserPasswd  = "user.passwd"
	CmdUserRemove  = "user.rm"
	CmdUserList    = "user.list"
	CmdUserGroups  = "user.groups"
	CmdUserDisable = "user.disable"
	CmdUserEnable  = "user.enable"
	CmdUserLogout  = "user.logout"
	CmdUserAdmin   = "user.admin"
	CmdAuthRotate  = "auth.rotate-key"

	CmdGatewayStatus = "gateway.status"
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
	// User carries the user.* commands' arguments. A pointer, so every other
	// request stays byte-identical on the wire.
	User *UserRequest `json:"user,omitempty"`
}

// UserRequest is the argument to a user.* command. Password is in clear: it
// crosses the 0600 socket, the same boundary every other command trusts, and
// the server never logs a request.
type UserRequest struct {
	Username     string   `json:"username"`
	DisplayName  string   `json:"display_name,omitempty"`
	Email        string   `json:"email,omitempty"`
	Admin        bool     `json:"admin,omitempty"`
	Password     string   `json:"password,omitempty"`
	AddGroups    []string `json:"add_groups,omitempty"`
	RemoveGroups []string `json:"remove_groups,omitempty"`
}

// UserInfo is a user as the CLI sees one. It has no hash fields, so nothing
// secret can leave the daemon by this route.
type UserInfo struct {
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name,omitempty"`
	Email       string    `json:"email,omitempty"`
	Admin       bool      `json:"admin,omitempty"`
	Disabled    bool      `json:"disabled,omitempty"`
	Groups      []string  `json:"groups,omitempty"`
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
	LastLoginAt time.Time `json:"last_login_at"`
	Sessions    int       `json:"sessions"`
}

func userInfo(u auth.User) UserInfo {
	return UserInfo{
		Username: u.Username, DisplayName: u.DisplayName, Email: u.Email,
		Admin: u.Admin, Disabled: u.Disabled, Groups: u.Groups, HasPassword: u.HasPassword,
		CreatedAt: u.CreatedAt, LastLoginAt: u.LastLoginAt, Sessions: u.Sessions,
	}
}

// Response is what the daemon sends back. Error is set iff OK is false.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Version identifies the daemon's build. Only ping sets it, which is
	// enough to catch the case that matters: a binary upgraded on disk while
	// the daemon kept running the old one.
	Version string `json:"version,omitempty"`
	// Config is the fingerprint of the config.toml the daemon started with,
	// also set only by ping, so a client can tell when the file has changed
	// since and the daemon needs a restart to apply it.
	Config string `json:"config,omitempty"`
	// PID and Started identify this run of the daemon, also set only by ping,
	// so a client waiting out a restart can tell the new daemon from the old
	// one. Started covers the PID being reused.
	PID      int                        `json:"pid,omitempty"`
	Started  time.Time                  `json:"started,omitzero"`
	Services []supervisor.ServiceStatus `json:"services,omitempty"`
	Status   *supervisor.ServiceStatus  `json:"status,omitempty"`
	Events   []supervisor.EventRecord   `json:"events,omitempty"`
	Archives []backup.Archive           `json:"archives,omitempty"`
	Message  string                     `json:"message,omitempty"`
	Users    []UserInfo                 `json:"users,omitempty"`
	Gateway  *gateway.Status            `json:"gateway,omitempty"`
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

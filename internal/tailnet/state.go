// Package tailnet carries what Outpost has observed about a service's
// Tailscale presence.
//
// It is a leaf package on purpose. internal/tsnet produces this state and
// internal/supervisor, cmd/outpost and internal/tui consume it, but the
// supervisor deliberately links no Tailscale code at all — it drives the
// tailnet through an interface so its tests need no credentials and no
// tailscale.com dependency. Putting the vocabulary here keeps that true:
// nothing in this file imports anything outside the standard library.
package tailnet

import (
	"strings"
	"time"
)

// State is what the router observed, as distinct from what the manifest asked
// for. Conflating the two is the bug this package exists to end: a node stuck
// awaiting authorisation reported its configured name exactly as a node
// genuinely on the tailnet did.
type State string

const (
	// StateUnknown means nobody reported. An older daemon that predates these
	// fields, or a CLI reading manifests with no daemon running. Renderers
	// fall back to the configured name, which is what they always showed.
	StateUnknown State = ""
	// StateDisabled means no tailnet identity was asked for.
	StateDisabled State = "disabled"
	// StateOffline means one was asked for, but there is no node right now —
	// usually because the service is stopped.
	StateOffline State = "offline"
	// StateStarting means an attach is in flight and has not resolved.
	StateStarting State = "starting"
	// StateNeedsLogin means the node is on no tailnet until a human authorises
	// it at LoginURL.
	StateNeedsLogin State = "needs-login"
	// StateAttached means the node is up and proxying.
	StateAttached State = "attached"
	// StateFailed means the last attach attempt returned an error. The router
	// keeps retrying; Detail is why the most recent try did not work.
	StateFailed State = "failed"
)

// NodeStatus is one service's observed tailnet presence.
type NodeStatus struct {
	State State `json:"state,omitempty"`
	// Hostname is the name the node registers under.
	Hostname string `json:"hostname,omitempty"`
	// Addr is the 100.x.y.z tailnet address, once the node has one.
	Addr string `json:"addr,omitempty"`
	// LoginURL is set only in StateNeedsLogin.
	LoginURL string `json:"login_url,omitempty"`
	// Detail explains StateFailed.
	Detail string `json:"detail,omitempty"`
	// Since is when the state above began.
	Since time.Time `json:"since,omitzero"`
}

// Label is the state in words, for an operator rather than a switch statement.
func (s State) Label() string {
	switch s {
	case StateAttached:
		return "attached"
	case StateNeedsLogin:
		return "needs login"
	case StateStarting:
		return "connecting"
	case StateFailed:
		return "failed"
	case StateOffline:
		return "offline"
	case StateDisabled:
		return "disabled"
	}
	return ""
}

// Column is the cell for `outpost list`'s TAILSCALE column.
//
// The configured name appears only when the node is genuinely on the tailnet.
// Every other state says what is actually true instead, which is the whole
// point: the name used to be printed either way.
func Column(n NodeStatus, configuredName string) string {
	switch n.State {
	case StateUnknown:
		// Nobody reported, so say exactly what this always said.
		return orDash(configuredName)
	case StateAttached:
		return orDash(shortName(n, configuredName))
	case StateDisabled, StateOffline:
		return "-"
	}
	return n.State.Label()
}

// Line is the value after "tailscale:" in `outpost status` and the TUI.
func Line(n NodeStatus, configuredName string) string {
	shown := name(n, configuredName)
	switch n.State {
	case StateUnknown:
		return configuredName
	case StateAttached:
		if n.Addr != "" {
			return shown + " (attached, " + n.Addr + ")"
		}
		return shown + " (attached)"
	case StateDisabled:
		return "-"
	case StateOffline:
		return shown + " (no node; the service is not running)"
	case StateFailed:
		if n.Detail != "" {
			return shown + " — failed: " + n.Detail
		}
		return shown + " — failed"
	}
	return shown + " — " + n.State.Label()
}

// shortName is the attached name for a narrow column.
//
// Both facts matter, and they are usually the same fact: a node configured
// "yarr" registers as "yarr.tailcad888.ts.net", so the column shows "yarr" and
// the tailnet suffix stays in `outpost status`, where there is room for it and
// for the address. But when the node registered under a genuinely different
// name — the tailnet had a collision and appended a suffix, say — that is a
// discrepancy the operator needs to see, and the registered name is shown
// instead. Hiding it would be the same mistake this whole change is undoing:
// reporting what was asked for as though it were what happened.
func shortName(n NodeStatus, configuredName string) string {
	if n.Hostname == "" {
		return configuredName
	}
	label, _, _ := strings.Cut(n.Hostname, ".")
	if label == configuredName {
		return configuredName
	}
	return n.Hostname
}

// name prefers what the node actually registered as, falling back to what was
// configured.
func name(n NodeStatus, configuredName string) string {
	if n.Hostname != "" {
		return n.Hostname
	}
	return configuredName
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

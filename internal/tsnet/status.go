package tsnet

import "github.com/sroberts/outpost/internal/tailnet"

// noteLoginURL records that a node is waiting for a human to authorise it, and
// reports whether this sighting is worth a log line.
//
// The state is recorded every time, because a status reader wants the current
// URL whenever it asks. The logging is throttled to loginReminderInterval per
// URL, because tsnet offers the same one every five seconds.
func (r *Router) noteLoginURL(service, url string) bool {
	r.mu.Lock()
	n, ok := r.nodes[service]
	r.mu.Unlock()
	if !ok {
		// Detached while tsnet was still printing. There is nothing to record
		// and nothing anyone can act on.
		return false
	}
	return r.noteLoginURLFor(n, url)
}

// noteLoginURLFor is noteLoginURL for a node already in hand, and records
// nothing unless that node is still the router's node for the service.
//
// A backend outlives its node: shutdownNode gives up waiting after
// shutdownGrace, and a tsnet server left printing can hand over a login URL
// after its node has been replaced by a fresh one. Without this check that URL
// would land on the successor, flipping an attached node to needs-login,
// dropping it out of Attached and publishing a spent link. serve guards the
// same way for the same reason.
func (r *Router) noteLoginURLFor(n *node, url string) bool {
	r.mu.Lock()
	current, ok := r.nodes[n.name]
	r.mu.Unlock()
	if !ok || current != n {
		return false
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	now := r.now()
	if n.state != tailnet.StateNeedsLogin {
		// Since is when this state began, not when it was last reported.
		n.since = now
	}
	fresh := n.loginURL != url
	n.state, n.loginURL, n.detail = tailnet.StateNeedsLogin, url, ""

	if !fresh && now.Sub(n.loginLogged) < loginReminderInterval {
		return false
	}
	n.loginLogged = now
	return true
}

// NodeStatus is what the router has observed about one service's node, and
// false if it has no node at all.
//
// It takes r.mu to find the node and then n.mu to copy it out, the order the
// attach path uses. It does no I/O and waits on nothing: the supervisor calls
// it from the reconcile loop, which must not stall.
func (r *Router) NodeStatus(service string) (tailnet.NodeStatus, bool) {
	r.mu.Lock()
	n, ok := r.nodes[service]
	r.mu.Unlock()
	if !ok {
		return tailnet.NodeStatus{}, false
	}

	n.mu.Lock()
	defer n.mu.Unlock()

	// What the node registered as beats what was configured, once it has one.
	hostname := n.hostname
	if n.dnsName != "" {
		hostname = n.dnsName
	}
	return tailnet.NodeStatus{
		State:    n.state,
		Hostname: hostname,
		Addr:     n.addr,
		LoginURL: n.loginURL,
		Detail:   n.detail,
		Since:    n.since,
	}, true
}

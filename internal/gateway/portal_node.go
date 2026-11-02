package gateway

import "strings"

// PortalNode is how the portal's own tailnet node is doing, when it has one
// ([gateway] tailscale_name).
type PortalNode struct {
	// State is "starting", "needs-login" or "on".
	State string `json:"state"`
	// Host is the DNS name Tailscale gave the node, once it is on.
	Host string `json:"host,omitempty"`
	// LoginURL is where to authorise the node, while it needs a login.
	LoginURL string `json:"login_url,omitempty"`
	// Error is why the last attempt to bring it up failed.
	Error string `json:"error,omitempty"`
	// Renamed is set when Host isn't public_url's host: Tailscale gave the
	// node another name, usually because the one asked for was taken, and
	// public_url names an address nothing answers.
	Renamed bool `json:"renamed,omitempty"`
}

// portalNode reports the portal's node, or nil if it has none.
func (g *Gateway) portalNode() *PortalNode {
	if g.nodeStatus == nil {
		return nil
	}
	n, ok := g.nodeStatus()
	if !ok {
		return nil
	}
	host := strings.TrimSuffix(strings.ToLower(n.Host), ".")
	n.Renamed = host != "" && host != g.portalHost
	return &n
}

// PortalHost is public_url's host name, lower case: the name the portal's
// tailnet node has to be given for public_url to reach it.
func (g *Gateway) PortalHost() string { return g.portalHost }

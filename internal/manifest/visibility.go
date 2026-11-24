package manifest

import (
	"fmt"
	"strings"
)

// Visibility says how far an app reaches: on the tailnet at its own node, or
// only through the gateway. Both are behind the gateway's login.
const (
	// VisibilityTailnet is the default: the app has a tailnet node of its own,
	// which hands every request to the gateway.
	VisibilityTailnet = "tailnet"
	// VisibilityGateway gives the app no node: the gateway serves it at its
	// gateway address, and at /mcp/<name> on the portal when it has an
	// mcp_path.
	VisibilityGateway = "gateway"
)

// GatewayOnly reports whether the service is reached only through the
// gateway, with no tailnet node of its own.
func (s *Service) GatewayOnly() bool { return s.Visibility == VisibilityGateway }

// VisibilityOrDefault is the service's visibility, with unset read as the
// default.
func (s *Service) VisibilityOrDefault() string {
	if s.Visibility == "" {
		return VisibilityTailnet
	}
	return s.Visibility
}

// validateVisibility checks visibility, and what a gateway-only service can't
// also ask for. Funnel, mcp_funnel and tcp say so in their own checks.
func (s *Service) validateVisibility() []error {
	switch s.Visibility {
	case "", VisibilityTailnet:
		return nil
	case VisibilityGateway:
	default:
		return []error{fmt.Errorf("visibility must be %q or %q, got %q", VisibilityTailnet, VisibilityGateway, s.Visibility)}
	}
	// Refused rather than ignored: a name that does nothing reads as a node
	// that exists, and everything that keys off tailscale_name would have to
	// remember not to believe it.
	if s.TailscaleName != "" {
		return []error{fmt.Errorf("visibility = %q gives the app no tailnet node, so tailscale_name %q would do nothing; remove it, or use visibility = %q",
			VisibilityGateway, s.TailscaleName, VisibilityTailnet)}
	}
	return nil
}

// noNodeError is why a field that lives on the service's own node can't be
// used with visibility = "gateway".
func noNodeError(field string) error {
	return fmt.Errorf("%s is served on the service's tailnet node, and visibility = %q gives it none; use visibility = %q with a tailscale_name",
		field, VisibilityGateway, VisibilityTailnet)
}

// visibilityWarnings points out a gateway-only service nothing but this
// machine, or nothing with a browser, can reach.
func visibilityWarnings(s *Service, opts Options) []error {
	if !s.GatewayOnly() {
		return nil
	}
	if !opts.Gateway {
		return []error{fmt.Errorf("%s: visibility = %q but the gateway is off, so only this machine reaches it, on loopback", s.Name, VisibilityGateway)}
	}
	host := s.Hostname
	if host == "" && opts.AppDomain != "" {
		host = s.GatewayAddress() + "." + opts.AppDomain
	}
	// MagicDNS names nodes, <node>.<tailnet>.ts.net, and nothing under them,
	// so with the portal's node as the domain the app's address resolves
	// nowhere.
	if strings.HasSuffix(host, ".ts.net") && strings.Count(host, ".") != 3 {
		mcp := ""
		if s.MCPPath != "" {
			mcp = fmt.Sprintf("; MCP clients still reach it at /mcp/%s on the portal", s.Name)
		}
		return []error{fmt.Errorf("%s: visibility = %q serves it at %s, which MagicDNS doesn't resolve, so browsers can't open it%s; set [gateway] domain to one with wildcard DNS, or use visibility = %q",
			s.Name, VisibilityGateway, host, mcp, VisibilityTailnet)}
	}
	return nil
}

package manifest

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TCPForward is one entry of a service's tcp table: connections to Tailnet
// on the service's node are carried to Local on 127.0.0.1.
type TCPForward struct {
	Tailnet int
	Local   int
}

// String is the form the node process takes it in, "22=23231".
func (f TCPForward) String() string { return fmt.Sprintf("%d=%d", f.Tailnet, f.Local) }

// ParseTCPForward reads the form String writes.
func ParseTCPForward(v string) (TCPForward, error) {
	tn, local, ok := strings.Cut(v, "=")
	if !ok {
		return TCPForward{}, fmt.Errorf("tcp forward %q is not <tailnet port>=<local port>", v)
	}
	t, err := parsePort(tn)
	if err != nil {
		return TCPForward{}, fmt.Errorf("tcp forward %q: %w", v, err)
	}
	l, err := parsePort(local)
	if err != nil {
		return TCPForward{}, fmt.Errorf("tcp forward %q: %w", v, err)
	}
	return TCPForward{Tailnet: t, Local: l}, nil
}

// TCPForwards is the tcp table in tailnet-port order, skipping any entry
// validation would reject.
func (s *Service) TCPForwards() []TCPForward {
	out := make([]TCPForward, 0, len(s.TCP))
	for k, local := range s.TCP {
		tn, err := parsePort(k)
		if err != nil || local < 1 || local > 65535 {
			continue
		}
		out = append(out, TCPForward{Tailnet: tn, Local: local})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Tailnet < out[j].Tailnet })
	return out
}

func parsePort(v string) (int, error) {
	p, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || p < 1 || p > 65535 {
		return 0, fmt.Errorf("%q is not a port (1-65535)", v)
	}
	return p, nil
}

// validateTCP checks one service's tcp table.
func (s *Service) validateTCP(opts Options) []error {
	if len(s.TCP) == 0 {
		return nil
	}
	var errs []error
	// A forward lives on the service's own tailnet node; with no node there
	// is nothing to listen on, and silently ignoring the table would leave
	// the operator wondering why port 22 never answers.
	if !opts.RequireTailscale || s.TailscaleName == "" {
		errs = append(errs, fmt.Errorf("tcp forwards are served on the service's tailnet node, so they need Tailscale and a tailscale_name"))
	}
	keys := make([]string, 0, len(s.TCP))
	for k := range s.TCP {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		local := s.TCP[k]
		tn, err := parsePort(k)
		if err != nil {
			errs = append(errs, fmt.Errorf("tcp: tailnet port %w", err))
			continue
		}
		// The node already serves HTTP on these.
		if tn == 80 || tn == 443 {
			errs = append(errs, fmt.Errorf("tcp: tailnet port %d is the node's own HTTP(S) port; pick another", tn))
		}
		if local < 1 || local > 65535 {
			errs = append(errs, fmt.Errorf("tcp: %d = %d: %d is not a port (1-65535)", tn, local, local))
			continue
		}
		// Forwarding raw TCP to the HTTP port would hand the app requests
		// that never passed the gateway, with no identity headers and no
		// login: a way round everything §3 promises the service.
		if local == s.Port {
			errs = append(errs, fmt.Errorf("tcp: %d = %d forwards to the service's HTTP port, which would bypass the gateway; HTTP is already served on the node", tn, local))
		}
	}
	return errs
}

// tcpPortClashes reports svc's tcp forwards that point at another service's
// port, HTTP or forwarded: the connection would reach the wrong program.
func tcpPortClashes(svc *Service, services []*Service) []error {
	var errs []error
	for _, f := range svc.TCPForwards() {
		for _, other := range services {
			if other == svc {
				continue
			}
			if other.Port == f.Local {
				errs = append(errs, fmt.Errorf("%s: tcp forward to port %d is %s's HTTP port", svc.Name, f.Local, other.Name))
				continue
			}
			for _, of := range other.TCPForwards() {
				if of.Local == f.Local {
					errs = append(errs, fmt.Errorf("%s: tcp forward to port %d is also one of %s's", svc.Name, f.Local, other.Name))
					break
				}
			}
		}
	}
	return errs
}

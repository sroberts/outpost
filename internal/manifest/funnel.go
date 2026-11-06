package manifest

import "fmt"

// WithExposure is s with funnel and public_paths set as the portal set them,
// which win over the manifest's, and the errors that setting has by the
// manifest's own rules. s is not changed.
func (s *Service) WithExposure(funnel bool, publicPaths []string, opts Options) (*Service, []error) {
	out := *s
	out.Funnel = funnel
	out.PublicPaths = append([]string(nil), publicPaths...)
	var errs []error
	for _, p := range out.PublicPaths {
		if err := ValidPublicPath(p); err != nil {
			errs = append(errs, err)
		}
	}
	errs = append(errs, out.validateFunnel(opts)...)
	return &out, errs
}

// validateFunnel checks funnel = true, which answers the service's
// public_paths, and only those, on the public internet through Tailscale
// Funnel. Each rule closes a way it could expose more than that, or nothing.
func (s *Service) validateFunnel(opts Options) []error {
	if !s.Funnel {
		return nil
	}
	var errs []error
	// Funnel is a listener on the service's own node.
	if !opts.RequireTailscale || s.TailscaleName == "" {
		errs = append(errs, fmt.Errorf("funnel is served on the service's tailnet node, so it needs Tailscale and a tailscale_name"))
	}
	// Without the gateway a node proxies straight to the service, so the
	// internet would reach every path, not only the public ones.
	if !opts.Gateway {
		errs = append(errs, fmt.Errorf("funnel needs the gateway on ([gateway] enabled = true in config.toml); without it the whole app would be on the internet, not only its public_paths"))
	}
	if len(s.PublicPaths) == 0 {
		errs = append(errs, fmt.Errorf("funnel = true answers only public_paths from the internet, and there are none; add the webhook's path, such as public_paths = [\"/webhooks/github\"]"))
	}
	return errs
}

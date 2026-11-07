package gateway

import (
	"context"
	"strings"
)

// Exposure is what the gateway needs to set how an app is reached without a
// login. A setting made here wins over the manifest's funnel and
// public_paths; Reset goes back to the manifest's. Each is checked by the
// manifest's own rules and applied before it returns.
type Exposure interface {
	Set(ctx context.Context, service string, funnel bool, publicPaths []string, by string) error
	Reset(ctx context.Context, service, by string) error
}

// publicRow is what the internet reaches of one app, for its Manage menu.
type publicRow struct {
	Paths    string // one per line, for the form
	PathList []string
	Funnel   bool
	// CanFunnel is whether the app has a tailnet node for Funnel to be on.
	CanFunnel bool
	// State is how Funnel is doing, in words; Live is whether the internet
	// reaches it now, and URLs where.
	State string
	Live  bool
	URLs  []string
	// FromPortal says the portal set it, over what the manifest says, in
	// Manifest.
	FromPortal bool
	Manifest   string
	// Health is its state: up when the internet reaches it, down when Funnel
	// is on and failing, starting on the way, off when only the tailnet can;
	// HealthWords says it for a reader.
	Health, HealthWords string
}

// publicRowFor is what the internet reaches of rt, and how.
func publicRowFor(rt route) publicRow {
	row := publicRow{
		PathList: rt.PublicPaths, Funnel: rt.Funnel,
		CanFunnel:  rt.TailscaleName != "",
		FromPortal: rt.FromPortal,
		Paths:      strings.Join(rt.PublicPaths, "\n"),
	}
	row.State, row.Live = funnelWords(rt)
	switch {
	case !rt.Funnel:
		row.Health, row.HealthWords = "off", "tailnet only"
	case row.Live:
		row.Health, row.HealthWords = "up", "on the internet"
	case rt.FunnelState == "unavailable":
		row.Health, row.HealthWords = "down", "Funnel isn't working"
	case rt.Status != "running":
		// Not on its way: it waits on the app, which may stay stopped.
		row.Health, row.HealthWords = "off", "app not running"
	default:
		row.Health, row.HealthWords = "starting", "Funnel starting"
	}
	if row.Live {
		for _, p := range rt.PublicPaths {
			row.URLs = append(row.URLs, "https://"+rt.Host+p)
		}
	}
	if rt.FromPortal {
		row.Manifest = manifestWords(rt)
	}
	return row
}

// funnelWords says how an app's Funnel is doing, and whether the internet
// reaches it now.
func funnelWords(rt route) (string, bool) {
	switch {
	case !rt.Funnel:
		if len(rt.PublicPaths) == 0 {
			return "Nothing skips the login.", false
		}
		return "These skip the login on the tailnet. The internet can't reach them.", false
	case rt.FunnelState == "on" && rt.Host != "":
		return "On: the internet reaches these paths, and nothing else of this app.", true
	case rt.FunnelState == "unavailable":
		return "Unavailable: " + strings.TrimSuffix(rt.FunnelError, ".") + ". Only the tailnet reaches it; Outpost keeps trying.", false
	case rt.Status != "running":
		return "Waiting for the app to run.", false
	default:
		return "Starting.", false
	}
}

func manifestWords(rt route) string {
	funnel := "off"
	if rt.ManifestFunnel {
		funnel = "on"
	}
	paths := "no public paths"
	if len(rt.ManifestPublicPaths) > 0 {
		paths = "public paths " + strings.Join(rt.ManifestPublicPaths, ", ")
	}
	return "Set here, over the manifest, which has Funnel " + funnel + " and " + paths + "."
}

// splitPaths reads the form's paths: one per line, blank lines and repeats
// dropped. The manifest's rules check each.
func splitPaths(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

func describeExposure(funnel bool, paths []string) string {
	var b strings.Builder
	b.WriteString("set public paths ")
	if len(paths) == 0 {
		b.WriteString("to none")
	} else {
		b.WriteString(strings.Join(paths, ", "))
	}
	if funnel {
		b.WriteString(", with Funnel on")
	} else {
		b.WriteString(", with Funnel off")
	}
	return b.String()
}

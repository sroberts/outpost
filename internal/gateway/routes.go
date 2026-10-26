package gateway

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/sroberts/outpost/internal/config"
)

// routeRefresh is how often the route table is rebuilt from the supervisor.
// Requests read the table and never wait on the reconcile loop, so a new
// service is reachable within this long of being applied.
const routeRefresh = 2 * time.Second

// route is one service the gateway can send traffic to.
type route struct {
	Name   string
	Port   int
	Status string
}

type routeTable struct {
	mu     sync.RWMutex
	byName map[string]route
	sorted []route
	// skipped names services that can't be routed, so each is logged once.
	skipped map[string]bool
}

func (t *routeTable) get(name string) (route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	r, ok := t.byName[name]
	return r, ok
}

func (t *routeTable) all() []route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return append([]route(nil), t.sorted...)
}

// refreshRoutes rebuilds the table through the supervisor's public API. On
// an error the old table stays: a slow tick shouldn't take every app down.
func (g *Gateway) refreshRoutes(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	list, err := g.services.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			g.logf("gateway: refresh routes: %v", err)
		}
		return
	}

	byName := make(map[string]route, len(list))
	var sorted []route
	g.routes.mu.Lock()
	defer g.routes.mu.Unlock()
	if g.routes.skipped == nil {
		g.routes.skipped = map[string]bool{}
	}
	for _, s := range list {
		// Port 0 is a row the state store remembers for a service that is
		// no longer declared.
		if s.Port == 0 {
			continue
		}
		if !config.ValidLabel(s.Name) {
			if !g.routes.skipped[s.Name] {
				g.routes.skipped[s.Name] = true
				g.logf("gateway: %q isn't a valid DNS label (a-z, 0-9, '-', at most 63), so it has no address on the gateway; rename the service to route it", s.Name)
			}
			continue
		}
		r := route{Name: s.Name, Port: s.Port, Status: s.Status}
		byName[s.Name] = r
		sorted = append(sorted, r)
	}
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	g.routes.byName = byName
	g.routes.sorted = sorted
}

func (g *Gateway) keepRoutesFresh(ctx context.Context) {
	t := time.NewTicker(routeRefresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.refreshRoutes(ctx)
		}
	}
}

// Status is what `outpost gateway status` shows.
type Status struct {
	Listen    string        `json:"listen"`
	PublicURL string        `json:"public_url"`
	Domain    string        `json:"domain"`
	TLS       bool          `json:"tls"`
	Routes    []RouteStatus `json:"routes"`
}

// RouteStatus is one routed service.
type RouteStatus struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Port   int    `json:"port"`
	Status string `json:"status"`
}

// Status reports the gateway's addresses and the services it routes.
func (g *Gateway) Status() Status {
	st := Status{
		Listen:    g.Addr(),
		PublicURL: g.portalOrigin,
		Domain:    g.domain,
		TLS:       g.cfg.TLSCert != "",
		Routes:    []RouteStatus{},
	}
	for _, rt := range g.routes.all() {
		st.Routes = append(st.Routes, RouteStatus{
			Name:   rt.Name,
			URL:    g.appOrigin(rt.Name),
			Port:   rt.Port,
			Status: rt.Status,
		})
	}
	return st
}

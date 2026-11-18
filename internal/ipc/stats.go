package ipc

import (
	"errors"
	"math"
	"time"

	"github.com/sroberts/outpost/internal/stats"
)

// StatsSource is what the stats command reads usage from.
type StatsSource interface {
	All() map[string]stats.Usage
}

// UseStats enables the stats command. Call it before Serve.
func (s *Server) UseStats(st StatsSource) { s.stats = st }

var errNoStats = errors.New("usage stats are not available from this daemon")

// UsageInfo is stats.Usage on the wire. JSON can't carry NaN, which Usage
// uses for "no data", so every float here is a pointer and null stands for
// NaN.
type UsageInfo struct {
	Up             bool      `json:"up"`
	Measured       bool      `json:"measured"`
	CPU            *float64  `json:"cpu"`
	RSS            uint64    `json:"rss"`
	Storage        uint64    `json:"storage"`
	StorageAt      time.Time `json:"storage_at,omitzero"`
	StoragePartial bool      `json:"storage_partial,omitempty"`
	In24           uint64    `json:"in_24h"`
	Out24          uint64    `json:"out_24h"`

	CPUSeries     []*float64 `json:"cpu_series"`
	RSSSeries     []*float64 `json:"rss_series"`
	StorageSeries []*float64 `json:"storage_series"`
	InSeries      []*float64 `json:"in_series"`
	OutSeries     []*float64 `json:"out_series"`

	RecentCPU []*float64 `json:"recent_cpu"`
	RecentRSS []*float64 `json:"recent_rss"`
	RecentIn  []*float64 `json:"recent_in"`
	RecentOut []*float64 `json:"recent_out"`

	Since time.Time `json:"since,omitzero"`
}

func usageInfo(u stats.Usage) UsageInfo {
	return UsageInfo{
		Up: u.Up, Measured: u.Measured, CPU: toWire(u.CPU), RSS: u.RSS,
		Storage: u.Storage, StorageAt: u.StorageAt, StoragePartial: u.StoragePartial,
		In24: u.In24, Out24: u.Out24,
		CPUSeries: seriesToWire(u.CPUSeries), RSSSeries: seriesToWire(u.RSSSeries),
		StorageSeries: seriesToWire(u.StorageSeries),
		InSeries:      seriesToWire(u.InSeries), OutSeries: seriesToWire(u.OutSeries),
		RecentCPU: seriesToWire(u.RecentCPU), RecentRSS: seriesToWire(u.RecentRSS),
		RecentIn: seriesToWire(u.RecentIn), RecentOut: seriesToWire(u.RecentOut),
		Since: u.Since,
	}
}

// Usage is the reading back in the form the stats package uses, with NaN
// for no data.
func (u UsageInfo) Usage() stats.Usage {
	return stats.Usage{
		Up: u.Up, Measured: u.Measured, CPU: fromWire(u.CPU), RSS: u.RSS,
		Storage: u.Storage, StorageAt: u.StorageAt, StoragePartial: u.StoragePartial,
		In24: u.In24, Out24: u.Out24,
		CPUSeries: seriesFromWire(u.CPUSeries), RSSSeries: seriesFromWire(u.RSSSeries),
		StorageSeries: seriesFromWire(u.StorageSeries),
		InSeries:      seriesFromWire(u.InSeries), OutSeries: seriesFromWire(u.OutSeries),
		RecentCPU: seriesFromWire(u.RecentCPU), RecentRSS: seriesFromWire(u.RecentRSS),
		RecentIn: seriesFromWire(u.RecentIn), RecentOut: seriesFromWire(u.RecentOut),
		Since: u.Since,
	}
}

func toWire(f float64) *float64 {
	// Infinity can't be encoded either, and means no more than NaN would.
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

func fromWire(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func seriesToWire(s []float64) []*float64 {
	if s == nil {
		return nil
	}
	out := make([]*float64, len(s))
	for i, f := range s {
		out[i] = toWire(f)
	}
	return out
}

func seriesFromWire(s []*float64) []float64 {
	if s == nil {
		return nil
	}
	out := make([]float64, len(s))
	for i, p := range s {
		out[i] = fromWire(p)
	}
	return out
}

// dispatchStats serves the stats command from the sampler's snapshot, which
// never waits on the reconcile loop.
func (s *Server) dispatchStats(req Request) Response {
	if s.stats == nil {
		return errorResponse(errNoStats)
	}
	all := s.stats.All()
	out := make(map[string]UsageInfo, len(all))
	for name, u := range all {
		if req.Service != "" && name != req.Service {
			continue
		}
		out[name] = usageInfo(u)
	}
	return Response{OK: true, Usage: out}
}

// Stats returns each service's usage, or only the named one's when service
// isn't "". A service the sampler hasn't seen yet is simply absent.
func (c *Client) Stats(service string) (map[string]stats.Usage, error) {
	resp, err := c.do(Request{Command: CmdStats, Service: service})
	if err != nil {
		return nil, err
	}
	out := make(map[string]stats.Usage, len(resp.Usage))
	for name, u := range resp.Usage {
		out[name] = u.Usage()
	}
	return out, nil
}

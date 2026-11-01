package gateway

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/sroberts/outpost/internal/stats"
)

// A sparkline's viewBox. The width is one unit a quarter hour, so a day of
// buckets spans it exactly and an app with less history leaves the left end
// empty: every app's graph then reads against the same clock.
const (
	sparkW = stats.Buckets
	sparkH = 36
)

// The lines are drawn above the baseline rather than on it, so an idle app's
// flat line still reads as a line and not as the axis.
const (
	plotH = sparkH - 3
	baseY = sparkH - 0.5 // a 1px rule along the foot, inside the viewBox
)

// Scale floors, so an idle app draws a flat line near the bottom rather than
// its noise stretched to fill the graph.
const (
	cpuFloor     = 0.05 // cores: 5% of one
	networkFloor = 100e3
)

// usageView is an app's usage as the catalog draws it, every string and path
// worked out here so the template only places them.
type usageView struct {
	Title string      // the app's name, so each disclosure is announced as its own
	Line  []usagePart // the figures, which are also the disclosure's label
	Tiles []usageTile // the day's graphs
	When  string      // what the graphs span: the whole day, or since when
}

// usagePart is one figure in the summary line. Pending is a CPU reading not
// taken yet, shown as a dash with words for a screen reader.
type usagePart struct {
	Text    string
	Pending bool
	Live    bool // only true of a running process
}

// usageTile is one graph. Values are its text equivalent, a line each so a
// figure never breaks from its unit in a narrow tile; network has Keys
// instead, naming each line in words beside a swatch of it.
type usageTile struct {
	Name   string
	Base   string      // the baseline, from the first sample to now
	Gap    string      // the part of the day not measured yet, drawn broken
	Paths  []sparkLine // the lines themselves
	Dots   []sparkLine // a dot on each line's latest point, so one sample shows
	Values []string
	Keys   []sparkLine
	Note   string // what the figures don't count
}

// sparkLine is one path in a graph, and for network, its key.
type sparkLine struct {
	Class string
	D     string
	Text  string
}

// dropLive leaves out the figures that only mean something while it runs.
func (v *usageView) dropLive() {
	v.Line = slices.DeleteFunc(v.Line, func(p usagePart) bool { return p.Live })
}

// usageOf builds the view of a service's usage, or nil when there's nothing
// to show: no sampler, or one that hasn't seen the service yet.
func (g *Gateway) usageOf(name, title string) *usageView {
	if g.stats == nil {
		return nil
	}
	u, ok := g.stats.Usage(name)
	if !ok {
		return nil
	}
	v := &usageView{Title: title}

	// Figures that aren't known are left out rather than shown as zero; a
	// stopped app still has its storage.
	if u.Measured {
		if finite(u.CPU) {
			v.Line = append(v.Line, usagePart{Text: cpuPercent(u.CPU) + " CPU", Live: true})
		} else {
			v.Line = append(v.Line, usagePart{Pending: true, Live: true})
		}
		v.Line = append(v.Line, usagePart{Text: humanBytes(u.RSS) + " memory", Live: true})
	}
	if !u.StorageAt.IsZero() {
		v.Line = append(v.Line, usagePart{Text: storageText(u) + " stored"})
	}

	// Every graph's unmeasured stretch ends where the first of them starts,
	// so a stopped stretch after that reads as a gap in the line, not as
	// history the daemon doesn't have, and the tiles agree with each other.
	storage := storageNow(u, g.now())
	start := firstFinite(u.CPUSeries, u.RSSSeries, u.InSeries, u.OutSeries, storage)

	if peak, ok := highest(u.CPUSeries); ok {
		values := []string{"peak " + cpuPercent(peak)}
		// Up but not measured is a probe that failed this tick, not a stopped
		// app, so it says nothing rather than "not running".
		switch {
		case !u.Up:
			values = append(values, "not running")
		case u.Measured && finite(u.CPU):
			values = append(values, "now "+cpuPercent(u.CPU))
		} // a reading not taken yet is left out: a dash read aloud says nothing
		v.Tiles = append(v.Tiles, graph(start, usageTile{Name: "CPU", Values: values},
			line("", u.CPUSeries, cpuFloor)))
	}
	if peak, ok := highest(u.RSSSeries); ok {
		values := []string{"peak " + humanBytes(uint64(peak))}
		switch {
		case !u.Up:
			values = append(values, "not running")
		case u.Measured:
			values = append(values, "now "+humanBytes(u.RSS))
		}
		v.Tiles = append(v.Tiles, graph(start, usageTile{Name: "Memory", Values: values},
			line("", u.RSSSeries, 0)))
	}
	inPeak, inOK := highest(u.InSeries)
	outPeak, outOK := highest(u.OutSeries)
	if inOK || outOK {
		// One scale for both, so the taller line is the one that carried more.
		top := max(inPeak, outPeak, networkFloor)
		v.Tiles = append(v.Tiles, graph(start, usageTile{
			Name: "Network",
			Note: "Only traffic through Outpost",
			Keys: []sparkLine{
				{Class: "key in", Text: humanBytes(u.In24) + " in"},
				{Class: "key out", Text: humanBytes(u.Out24) + " out"},
			},
		}, line("in", u.InSeries, top), line("out", u.OutSeries, top)))
	}
	if _, ok := highest(u.StorageSeries); ok && !u.StorageAt.IsZero() {
		v.Tiles = append(v.Tiles, graph(start, usageTile{Name: "Storage", Values: []string{"now " + storageText(u)}},
			line("", storage, 0)))
	}

	v.When = sinceText(u.Since, g.now())
	if v.When == "" {
		v.When = "Last 24 hours"
	}
	if len(v.Line) == 0 && len(v.Tiles) == 0 {
		return nil
	}
	return v
}

// series is one line to draw: its class modifier, values and scale floor.
type series struct {
	class  string
	values []float64
	floor  float64
}

func line(class string, values []float64, floor float64) series {
	return series{class, values, floor}
}

// graph draws a tile's lines, the baseline beneath them and the dots that
// end them. The baseline is broken over the part of the day before the first
// sample, in the line's own broken vocabulary, so a graph started an hour ago
// reads as not yet measured rather than as idle or empty.
func graph(first int, t usageTile, lines ...series) usageTile {
	x := 0.0
	if first > 0 && len(lines[0].values) > 1 {
		x = float64(sparkW) * float64(first) / float64(len(lines[0].values)-1)
		t.Gap = "M0," + coord(baseY) + "H" + coord(x)
	}
	t.Base = "M" + coord(x) + "," + coord(baseY) + "H" + coord(sparkW)

	for _, l := range lines {
		class := strings.TrimSpace("spark " + l.class)
		d := sparkPathMin(l.values, sparkW, plotH, l.floor)
		t.Paths = append(t.Paths, sparkLine{Class: class, D: d})
		if end := endOf(d); end != "" {
			t.Dots = append(t.Dots, sparkLine{Class: strings.TrimSpace("dot " + l.class), D: "M" + end + "h0"})
		}
	}
	return t
}

// firstFinite is the earliest bucket any of the series has a reading in, or
// -1 for none.
func firstFinite(all ...[]float64) int {
	first := -1
	for _, vs := range all {
		for i, v := range vs {
			if finite(v) {
				if first < 0 || i < first {
					first = i
				}
				break
			}
		}
	}
	return first
}

// endOf is the last point a path reaches, "x,y", or "" for an empty one.
func endOf(d string) string {
	i := strings.LastIndexAny(d, "ML")
	if i < 0 {
		return ""
	}
	return d[i+1:]
}

// storageNow is the storage series with the quarter hour now in filled from
// the latest walk. Walks are ten minutes apart, so the current quarter often
// has none yet, and without it the graph stops short of every other one.
// It's a copy: snapshots are shared and never written.
func storageNow(u stats.Usage, now time.Time) []float64 {
	s := u.StorageSeries
	if len(s) == 0 || finite(s[len(s)-1]) || now.Sub(u.StorageAt) > 15*time.Minute {
		return s
	}
	s = append([]float64(nil), s...)
	s[len(s)-1] = float64(u.Storage)
	return s
}

// storageText is the storage size, said as a lower bound when the walk that
// measured it stopped short.
func storageText(u stats.Usage) string {
	if u.StoragePartial {
		return "at least " + humanBytes(u.Storage)
	}
	return humanBytes(u.Storage)
}

// sinceText says when the history starts, or nothing when it covers the day:
// a graph that's empty on the left otherwise reads as an app that was idle.
func sinceText(since, now time.Time) string {
	if since.IsZero() || !since.After(now.Add(-24*time.Hour+15*time.Minute)) {
		return ""
	}
	since = since.In(now.Location())
	if y, m, d := since.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return "Since " + since.Format("15:04")
	}
	return "Since yesterday " + since.Format("15:04")
}

// highest is the largest known value in a series, and false for a series
// with nothing in it.
func highest(series []float64) (float64, bool) {
	top, ok := 0.0, false
	for _, v := range series {
		if finite(v) && (!ok || v > top) {
			top, ok = v, true
		}
	}
	return top, ok
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

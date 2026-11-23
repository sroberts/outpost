package tui

import (
	"fmt"
	"math"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/sroberts/outpost/internal/stats"
)

// StatsClient is the daemon's usage history: CPU, memory, storage and the
// traffic the gateway carried, per service. *ipc.Client satisfies it. A
// client without it gets no graphs and no overview.
type StatsClient interface {
	Stats(service string) (map[string]stats.Usage, error)
}

func (m Model) statsClient() (StatsClient, bool) {
	x, ok := m.client.(StatsClient)
	return x, ok
}

type usageMsg struct {
	usage map[string]stats.Usage
	err   error
}

// fetchUsage reads every service's usage in one request. The daemon samples
// every 30 seconds, so reading it on the 2-second tick costs a request but
// never shows anything stale for long.
func (m Model) fetchUsage() tea.Cmd {
	x, ok := m.statsClient()
	if !ok {
		return nil
	}
	return func() tea.Msg {
		u, err := x.Stats("")
		return usageMsg{usage: u, err: err}
	}
}

// usageShown reports whether the view on screen draws usage. A read is a
// day of series for every service, so the tick doesn't make one for a view
// that shows none; opening one that does reads it then.
func (m Model) usageShown() bool {
	return m.view == viewServices || m.view == viewOverview
}

// usageOf is the selected range's series for a service, and whether there is
// any usage for it at all.
func (m Model) usageOf(name string) (stats.Usage, bool) {
	u, ok := m.usage[name]
	return u, ok
}

// series picks the long (24 h, quarter hours) or the short (30 min, raw
// samples) form of a series.
func (m Model) series(short, long []float64) []float64 {
	if m.graphLong {
		return long
	}
	return short
}

// rangeLabel names the range the graphs cover.
func (m Model) rangeLabel() string {
	if m.graphLong {
		return "last 24 h"
	}
	return "last 30 min"
}

// sparkRunes are eight heights of bar, one cell each.
var sparkRunes = []rune("▁▂▃▄▅▆▇█")

// resample fits vals into width points, each the largest of the values it
// covers, so a short spike is never averaged away. A point covering only
// unknowns is NaN.
func resample(vals []float64, width int) []float64 {
	if width <= 0 {
		return nil
	}
	out := make([]float64, width)
	n := len(vals)
	for i := range out {
		out[i] = math.NaN()
		if n == 0 {
			continue
		}
		lo := i * n / width
		hi := (i + 1) * n / width
		if hi <= lo {
			hi = lo + 1
		}
		for _, v := range vals[lo:min(hi, n)] {
			if !math.IsNaN(v) && (math.IsNaN(out[i]) || v > out[i]) {
				out[i] = v
			}
		}
	}
	return out
}

// ceiling is the largest known value, or 0 for none.
func ceiling(vals []float64) float64 {
	c := 0.0
	for _, v := range vals {
		if !math.IsNaN(v) && v > c {
			c = v
		}
	}
	return c
}

// sparkline draws vals as a one-row bar graph width cells wide, scaled to
// ceil (to their own largest value when ceil is 0). Unknowns are blank, so a
// stopped service reads as a gap rather than as idle.
func sparkline(vals []float64, width int, ceil float64) string {
	pts := resample(vals, width)
	if ceil <= 0 {
		ceil = ceiling(pts)
	}
	var b strings.Builder
	for _, v := range pts {
		switch {
		case math.IsNaN(v):
			b.WriteRune(' ')
		case ceil <= 0 || v <= 0:
			b.WriteRune(sparkRunes[0])
		default:
			i := int(math.Round(v / ceil * float64(len(sparkRunes)-1)))
			b.WriteRune(sparkRunes[max(0, min(i, len(sparkRunes)-1))])
		}
	}
	return b.String()
}

// fmtCPU shows cores as a percent of one, as top does.
func fmtCPU(cores float64) string {
	if math.IsNaN(cores) {
		return "-"
	}
	p := cores * 100
	if p > 0 && p < 1 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

// fmtBytes is a byte count in binary units.
func fmtBytes(n float64) string {
	if math.IsNaN(n) {
		return "-"
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[i])
	}
	if n < 10 {
		return fmt.Sprintf("%.1f %s", n, units[i])
	}
	return fmt.Sprintf("%.0f %s", n, units[i])
}

// usageCells are the table's CPU and MEM cells for a service: the latest
// reading and, given room, a sparkline of the recent CPU. Blank for a
// service with nothing measured.
func (m Model) usageCells(name string, spark int) (cpu, mem string) {
	u, ok := m.usageOf(name)
	if !ok || !u.Measured {
		return "-", "-"
	}
	cpu = fmtCPU(u.CPU)
	if spark > 0 {
		cpu = pad(cpu, 5) + " " + sparkline(u.RecentCPU, spark, 0)
	}
	return cpu, fmtBytes(float64(u.RSS))
}

// graphLines draws the selected service's usage for the detail pane in
// width cells. tall asks for charts several rows high rather than
// sparklines, when the pane has the room.
func (m Model) graphLines(name string, width int, tall int) []string {
	u, ok := m.usageOf(name)
	if !ok {
		return nil
	}
	label := func(k, v string) string {
		return labelStyle.Render(pad(k, 6)) + valueStyle.Render(pad(v, 10))
	}
	graphW := max(width-18, 8)
	var out []string
	out = append(out, sectionStyle.Render("usage")+" "+labelStyle.Render("· "+m.rangeLabel()+" · g toggles"))

	cpuS := m.series(u.RecentCPU, u.CPUSeries)
	rssS := m.series(u.RecentRSS, u.RSSSeries)
	cpuNow, memNow := "-", "-"
	if u.Measured {
		cpuNow, memNow = fmtCPU(u.CPU), fmtBytes(float64(u.RSS))
	}
	if !u.Up {
		cpuNow = "down"
	}
	draw := func(k, now string, vals []float64, peak string) {
		if tall > 1 {
			rows := barChart(vals, graphW, tall, graphStyle)
			for i, r := range rows {
				head := strings.Repeat(" ", 16)
				switch i {
				case 0:
					head = label(k, now)
				case 1:
					// The label column, not the value's, takes "peak": with
					// it in front, a value such as 12.3 MiB ran past the
					// 16 cells every row's head is given and pushed this
					// row of the chart out of line.
					head = labelStyle.Render(pad("peak", 6) + pad(peak, 10))
				}
				out = append(out, head+"  "+r)
			}
			return
		}
		out = append(out, label(k, now)+"  "+graphStyle.Render(sparkline(vals, graphW, 0)))
	}
	draw("cpu", cpuNow, cpuS, fmtCPU(ceiling(cpuS)))
	draw("mem", memNow, rssS, fmtBytes(ceiling(rssS)))

	inS := m.series(u.RecentIn, u.InSeries)
	outS := m.series(u.RecentOut, u.OutSeries)
	if ceiling(inS) > 0 || ceiling(outS) > 0 || u.In24 > 0 || u.Out24 > 0 {
		out = append(out, label("in", fmtBytes(float64(u.In24))+"/d")+"  "+graphStyle.Render(sparkline(inS, graphW, 0)))
		out = append(out, label("out", fmtBytes(float64(u.Out24))+"/d")+"  "+graphStyle.Render(sparkline(outS, graphW, 0)))
	}
	if !u.StorageAt.IsZero() {
		size := fmtBytes(float64(u.Storage))
		if u.StoragePartial {
			size = "≥" + size
		}
		// Storage is walked every ten minutes, so only the day has a shape.
		out = append(out, label("disk", size)+"  "+graphStyle.Render(sparkline(u.StorageSeries, graphW, 0)))
	}
	return out
}

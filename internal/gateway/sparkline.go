package gateway

import (
	"math"
	"strconv"
	"strings"

	"github.com/sroberts/outpost/internal/size"
)

// sparkPath draws series as an SVG path for a "0 0 w h" viewBox, scaled so
// its highest point touches the top. NaN is a gap: the line stops and starts
// again after it, so a stopped app doesn't read as an idle one.
func sparkPath(series []float64, w, h int) string {
	return sparkPathMin(series, w, h, 0)
}

// sparkPathMin is sparkPath with a floor on the scale: a series whose highest
// point is below floor is drawn against floor instead, so the noise of an
// idle app stays a flat line near the bottom instead of filling the chart.
// The caller picks the floor, since only it knows the units.
func sparkPathMin(series []float64, w, h int, floor float64) string {
	if len(series) == 0 || w <= 0 || h <= 0 {
		return ""
	}
	top := floor
	for _, v := range series {
		if finite(v) && v > top {
			top = v
		}
	}
	// A pixel of margin top and bottom, so the stroke isn't clipped.
	span := float64(h) - 2
	y := func(v float64) float64 {
		if top <= 0 || v <= 0 {
			return float64(h) - 1
		}
		return float64(h) - 1 - span*min(v/top, 1)
	}
	step := 0.0
	if len(series) > 1 {
		step = float64(w) / float64(len(series)-1)
	}
	var b strings.Builder
	known := func(i int) bool {
		return i >= 0 && i < len(series) && finite(series[i])
	}
	for i, v := range series {
		if !known(i) {
			continue
		}
		x := step * float64(i)
		if known(i - 1) {
			b.WriteString("L")
			point(&b, x, y(v))
			continue
		}
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		b.WriteString("M")
		if !known(i + 1) {
			// A point on its own has no line to draw, so it gets a short
			// level stroke either side of where it falls; a lone sample is
			// the whole width.
			half := step / 2
			if len(series) == 1 {
				x, half = float64(w)/2, float64(w)/2
			}
			point(&b, max(x-half, 0), y(v))
			b.WriteString("L")
			point(&b, min(x+half, float64(w)), y(v))
			continue
		}
		point(&b, x, y(v))
	}
	return b.String()
}

// point writes "x,y" to a tenth of a pixel, which is all a sparkline can
// show, and keeps the path short.
func point(b *strings.Builder, x, y float64) {
	b.WriteString(coord(x))
	b.WriteString(",")
	b.WriteString(coord(y))
}

func coord(v float64) string {
	v = math.Round(v*10) / 10
	if v <= 0 {
		return "0" // never "-0"
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// humanBytes is size.Bytes, named for the templates' helpers beside it.
func humanBytes(n uint64) string { return size.Bytes(n) }

// cpuPercent is a CPU reading in cores as a percent of one core, as top shows
// it, so a busy app on four cores can read 400%. An unknown reading is a dash.
func cpuPercent(cores float64) string {
	if !finite(cores) || cores < 0 {
		return "–"
	}
	p := cores * 100
	if p < 9.95 {
		return strconv.FormatFloat(p, 'f', 1, 64) + "%"
	}
	return strconv.FormatFloat(p, 'f', 0, 64) + "%"
}

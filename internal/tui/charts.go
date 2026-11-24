package tui

import (
	"math"
	"strings"

	"github.com/NimbleMarkets/ntcharts/canvas/runes"
	"github.com/NimbleMarkets/ntcharts/linechart/streamlinechart"
	ntspark "github.com/NimbleMarkets/ntcharts/sparkline"
	"github.com/charmbracelet/lipgloss"
)

// The charts taller than a row are drawn by ntcharts. The one-row
// sparklines in the table stay hand-drawn (graphs.go): they sit inside a
// table cell, where a canvas is more machinery than a row of runes needs.

// barChart draws vals as columns h rows tall and w wide, scaled to their
// largest value. The series is resampled first, so a short spike survives
// being squeezed into fewer columns. An unknown reads as zero here: a
// chart this size shows the shape, and the table says when a service is
// down.
func barChart(vals []float64, w, h int, style lipgloss.Style) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	pts := resample(vals, w)
	sl := ntspark.New(w, h, ntspark.WithStyle(style))
	if c := ceiling(pts); c > 0 {
		sl.SetMax(c)
	}
	for _, v := range pts {
		if math.IsNaN(v) {
			v = 0
		}
		sl.Push(v)
	}
	sl.Draw()
	return strings.Split(sl.View(), "\n")
}

// series is one line on a lineChart.
type chartSeries struct {
	name  string
	vals  []float64
	style lipgloss.Style
}

// lineChart draws one or more series as lines on shared axes, w by h, the
// y axis labelled by label. Every series is fitted to the plot's width.
func lineChart(series []chartSeries, w, h int, label func(float64) string) []string {
	if w <= 0 || h <= 0 {
		return nil
	}
	top := 0.0
	for _, s := range series {
		top = max(top, ceiling(s.vals))
	}
	if top <= 0 {
		top = 1
	}
	axis := lipgloss.NewStyle().Foreground(colSubtle)
	lc := streamlinechart.New(w, h,
		streamlinechart.WithYRange(0, top),
		streamlinechart.WithAxesStyles(axis, labelStyle))
	// Labels are placed by byte length, so they must be ASCII; fmtCPU and
	// fmtBytes are.
	lc.YLabelFormatter = func(_ int, v float64) string { return label(v) }
	// The axis is as wide as its widest label, so the plot is sized again
	// now the labels are the real ones; SetYRange rescales the buffers to it.
	lc.UpdateGraphSizes()
	lc.SetYRange(0, top)
	lc.SetViewYRange(0, top)
	plotW := max(lc.Width()-lc.Origin().X, 1)
	names := make([]string, 0, len(series))
	for _, s := range series {
		lc.SetDataSetStyles(s.name, runes.ArcLineStyle, s.style)
		for _, v := range resample(s.vals, plotW) {
			if math.IsNaN(v) {
				v = 0
			}
			lc.PushDataSet(s.name, v)
		}
		names = append(names, s.name)
	}
	lc.DrawDataSets(names)
	return strings.Split(lc.View(), "\n")
}

// sumSeries adds series point by point, aligned at their ends (the latest
// sample), for a total across services. A point every series leaves
// unknown stays unknown.
func sumSeries(all [][]float64) []float64 {
	n := 0
	for _, s := range all {
		n = max(n, len(s))
	}
	out := make([]float64, n)
	for i := range out {
		out[i] = math.NaN()
	}
	for _, s := range all {
		off := n - len(s)
		for i, v := range s {
			if math.IsNaN(v) {
				continue
			}
			if math.IsNaN(out[off+i]) {
				out[off+i] = 0
			}
			out[off+i] += v
		}
	}
	return out
}

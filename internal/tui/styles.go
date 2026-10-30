package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/sroberts/outpost/internal/runtime"
	"github.com/sroberts/outpost/internal/supervisor"
)

// Colours adapt to light and dark terminals rather than assuming dark.
var (
	colDim    = lipgloss.AdaptiveColor{Light: "244", Dark: "240"}
	colText   = lipgloss.AdaptiveColor{Light: "236", Dark: "252"}
	colAccent = lipgloss.AdaptiveColor{Light: "27", Dark: "39"}
	colOK     = lipgloss.AdaptiveColor{Light: "28", Dark: "42"}
	colWarn   = lipgloss.AdaptiveColor{Light: "130", Dark: "214"}
	colErr    = lipgloss.AdaptiveColor{Light: "160", Dark: "203"}
	colSubtle = lipgloss.AdaptiveColor{Light: "250", Dark: "238"}
)

var (
	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(colAccent)
	countStyle = lipgloss.NewStyle().Foreground(colDim)

	headerStyle = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	rowStyle    = lipgloss.NewStyle().Foreground(colText)
	selectedRow = lipgloss.NewStyle().Bold(true).Foreground(colAccent)

	sectionStyle = lipgloss.NewStyle().Bold(true).Foreground(colDim)
	labelStyle   = lipgloss.NewStyle().Foreground(colDim)
	valueStyle   = lipgloss.NewStyle().Foreground(colText)

	errorStyle  = lipgloss.NewStyle().Foreground(colErr)
	noticeStyle = lipgloss.NewStyle().Foreground(colAccent)
	ruleStyle   = lipgloss.NewStyle().Foreground(colSubtle)
	logStyle    = lipgloss.NewStyle().Foreground(colDim)
)

// statusStyle colours a status word by what it means, so a glance is enough.
func statusStyle(status string) lipgloss.Style {
	switch runtime.Status(status) {
	case runtime.StatusRunning:
		return lipgloss.NewStyle().Foreground(colOK)
	case runtime.StatusStarting:
		return lipgloss.NewStyle().Foreground(colWarn)
	case runtime.StatusUnhealthy:
		return lipgloss.NewStyle().Foreground(colWarn)
	case runtime.StatusFailed:
		return lipgloss.NewStyle().Foreground(colErr)
	case runtime.StatusStopped:
		return lipgloss.NewStyle().Foreground(colDim)
	default:
		return lipgloss.NewStyle().Foreground(colDim)
	}
}

// statusMark is a glyph carrying the same meaning as the colour, so the
// dashboard still reads correctly without colour.
func statusMark(status string) string {
	switch runtime.Status(status) {
	case runtime.StatusRunning:
		return "●"
	case runtime.StatusStarting:
		return "◐"
	case runtime.StatusUnhealthy:
		return "▲"
	case runtime.StatusFailed:
		return "✖"
	case runtime.StatusStopped:
		return "○"
	case supervisor.StatusRemoved:
		return "✕"
	default:
		return "·"
	}
}

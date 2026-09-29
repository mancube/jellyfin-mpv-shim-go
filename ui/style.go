package ui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Look & feel for both TUI screens. Kept in one place so the status screen
// and the wizard read as the same app.

var (
	colTitle = lipgloss.Color("205") // magenta-ish accent
	colOK    = lipgloss.Color("42")  // connected / playing
	colWarn  = lipgloss.Color("214") // paused
	colBad   = lipgloss.Color("203") // errors / offline
	colDim   = lipgloss.Color("241") // hints, secondary values
	colText  = lipgloss.Color("252")

	styTitle = lipgloss.NewStyle().Bold(true).Foreground(colTitle)
	styOK    = lipgloss.NewStyle().Foreground(colOK).Bold(true)
	styWarn  = lipgloss.NewStyle().Foreground(colWarn).Bold(true)
	styBad   = lipgloss.NewStyle().Foreground(colBad).Bold(true)
	styDim   = lipgloss.NewStyle().Foreground(colDim)
	styText  = lipgloss.NewStyle().Foreground(colText)
	styKey   = lipgloss.NewStyle().Foreground(colTitle).Bold(true)
	styBarOn = lipgloss.NewStyle().Foreground(colOK)
	styBar   = lipgloss.NewStyle().Foreground(colDim)
)

var panelStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colDim).
	Padding(0, 1)

// panel draws a titled box sized to its content.
func panel(title string, body ...string) string {
	return panelW(title, 0, body...)
}

// panelW is panel with a minimum inner width, so a form does not jump around
// while the user types.
func panelW(title string, width int, body ...string) string {
	lines := append([]string{styTitle.Render(title)}, body...)
	style := panelStyle
	if width > 0 {
		style = style.Width(width)
	}
	return style.Render(strings.Join(lines, "\n"))
}

// kv is a "key   value" row with aligned values.
func kv(k, v string) string {
	return styDim.Render(pad(k, 11)) + styText.Render(v)
}

func pad(s string, n int) string {
	if w := lipgloss.Width(s); w < n {
		return s + strings.Repeat(" ", n-w)
	}
	return s
}

// bar renders a playback progress bar of the given width in columns.
func bar(frac float64, width int) string {
	frac = min(max(frac, 0), 1)
	filled := int(frac*float64(width) + 0.5)
	return styBarOn.Render(strings.Repeat("█", filled)) +
		styBar.Render(strings.Repeat("░", width-filled))
}

// hints renders the key-hint line from (key, description) pairs.
func hints(pairs ...[2]string) string {
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, styKey.Render(p[0])+" "+styDim.Render(p[1]))
	}
	return "  " + strings.Join(parts, styDim.Render("   "))
}

// clip shortens s to n columns, keeping the end (paths/URLs matter most).
func clip(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	r := []rune(s)
	if n <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(n-1):])
}

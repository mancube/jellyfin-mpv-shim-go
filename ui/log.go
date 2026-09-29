// Package ui is the setup + status surface: a Bubble Tea TUI (accounts,
// Quick Connect, live playback status) and a desktop systray. No webview, no
// window toolkit.
package ui

import (
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// LogRing is a bounded log buffer: the shim's logger writes into it (as an
// io.Writer) and the TUI/tray read the tail. One lock, one ring — no event
// plumbing needed.
type LogRing struct {
	mu    sync.Mutex
	lines []string
	limit int
	pos   int // write cursor
	n     int // lines written
}

func NewLogRing(limit int) *LogRing {
	if limit < 1 {
		limit = 1
	}
	return &LogRing{limit: limit, lines: make([]string, limit)}
}

// Write implements io.Writer for log.Logger.
func (l *LogRing) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines[l.pos] = strings.TrimRight(string(p), "\n")
	l.pos = (l.pos + 1) % l.limit
	l.n++
	return len(p), nil
}

// Tail returns up to n most recent lines, oldest first.
func (l *LogRing) Tail(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) == 0 {
		return nil
	}
	count := l.n
	if count > l.limit {
		count = l.limit
	}
	if n > 0 && n < count {
		count = n
	}
	out := make([]string, 0, count)
	start := (l.pos - count + l.limit) % l.limit
	for i := 0; i < count; i++ {
		out = append(out, l.lines[(start+i)%l.limit])
	}
	return out
}

// tickMsg drives the periodic refresh of the status screen.
type tickMsg time.Time

func tick() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(t time.Time) tea.Msg { return tickMsg(t) })
}

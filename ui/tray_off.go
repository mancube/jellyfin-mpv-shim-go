//go:build nosystray

package ui

// RunTray is the no-tray build (linux: go build -tags nosystray): the TUI
// remains the full surface.
func RunTray(s *Session) bool { return false }

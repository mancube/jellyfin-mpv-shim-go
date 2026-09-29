//go:build !nosystray

package ui

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"time"

	"github.com/energye/systray"
)

// RunTray starts the desktop tray: a status line, the current playback, and
// Quit. It never blocks the caller (systray owns its own goroutine) and it
// returns false when the platform has no tray host, so startup can log a
// hint and carry on. ponytail: one icon, two menu items — add per-account
// actions when the tray is actually used.
func RunTray(s *Session) bool {
	ready := make(chan struct{})
	go systray.Run(func() {
		icon := trayIcon()
		if icon != nil {
			systray.SetIcon(icon)
		}
		systray.SetTitle("mpv-shim")
		systray.SetTooltip("mpv-shim")
		status := systray.AddMenuItem("Status: starting…", "")
		status.Disable()
		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop mpv-shim")
		quit.Click(func() {
			if s.Quit != nil {
				s.Quit()
			}
		})
		close(ready)
		go refreshTray(s, status)
	}, func() {})

	select {
	case <-ready:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func refreshTray(s *Session, status *systray.MenuItem) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		conn := "offline"
		if s.WS.Connected() {
			conn = "online"
		}
		st := s.Player.Status()
		text := "mpv-shim — " + conn
		if st.Playing {
			state := "playing"
			if st.Paused {
				state = "paused"
			}
			text = fmt.Sprintf("mpv-shim — %s (%s)", st.Title, state)
		}
		systray.SetTooltip(text)
		status.SetTitle("Status: " + text)
	}
}

// trayIcon renders a 22x22 Jellyfin-purple square in memory: a tray icon file
// would be one more binary asset to ship.
func trayIcon() []byte {
	const size = 22
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	accent := color.RGBA{0x00, 0xa4, 0xdc, 0xff}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			edge := x == 0 || y == 0 || x == size-1 || y == size-1
			corner := (x+y < 2) || (x+(size-1-y) < 2) || ((size-1-x)+y < 2) || ((size-1-x)+(size-1-y) < 2)
			if edge || corner {
				img.Set(x, y, color.RGBA{})
				continue
			}
			img.Set(x, y, accent)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

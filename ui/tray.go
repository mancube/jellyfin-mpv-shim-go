//go:build !nosystray

package ui

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/energye/systray"
)

// RunTray starts the desktop tray. It never blocks the caller (systray owns
// its own goroutine) and returns false when the platform has no tray host, so
// startup can log a hint and carry on. The items mirror upstream's pystray
// menu (gui_mgr.py): configure servers, console, player menu, config folder,
// quit.
func RunTray(s *Session) bool {
	ready := make(chan struct{})
	go systray.Run(func() {
		if icon := trayIcon(); icon != nil {
			systray.SetIcon(icon)
		}
		systray.SetTitle("mpv-shim")
		systray.SetTooltip("mpv-shim — starting…")

		status := systray.AddMenuItem("Status: starting…", "")
		status.Disable()
		nowPlaying := systray.AddMenuItem("Now playing: —", "")
		nowPlaying.Disable()
		systray.AddSeparator()

		accounts := systray.AddMenuItem("Configure Servers…", "Add or remove an account")
		accounts.Click(func() { s.RequestAccounts() })

		osd := systray.AddMenuItem("Player Menu (OSD)", "Open the in-player audio/subtitle menu")
		osd.Click(func() {
			if s.OpenOSD != nil {
				s.OpenOSD()
			}
		})

		config := systray.AddMenuItem("Open Config Folder", s.ConfigDir)
		config.Click(func() { openInFileManager(s.ConfigDir) })

		if s.LogPath != "" {
			logItem := systray.AddMenuItem("Open Log File", s.LogPath)
			logItem.Click(func() {
				if _, err := os.Stat(s.LogPath); err != nil {
					s.Logf("log file %s: %v", s.LogPath, err)
					return
				}
				openInFileManager(s.LogPath)
			})
		}

		systray.AddSeparator()
		quit := systray.AddMenuItem("Quit", "Stop mpv-shim")
		quit.Click(func() {
			if s.Quit != nil {
				s.Quit()
			}
		})

		close(ready)
		go refreshTray(s, status, nowPlaying)
	}, func() {})

	select {
	case <-ready:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func refreshTray(s *Session, status, nowPlaying *systray.MenuItem) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for range t.C {
		conn := "offline"
		if s.WS.Connected() {
			conn = "online"
		}
		text := "mpv-shim — " + conn
		status.SetTitle("Status: " + text)
		systray.SetTooltip(text)

		st := s.Player.Status()
		if st.Playing {
			state := "playing"
			if st.Paused {
				state = "paused"
			}
			nowPlaying.SetTitle(fmt.Sprintf("Now playing: %s (%s)", st.Title, state))
			systray.SetTooltip(fmt.Sprintf("mpv-shim — %s (%s)", st.Title, state))
		} else {
			nowPlaying.SetTitle("Now playing: —")
		}
	}
}

// openInFileManager opens a file or folder with the platform's default
// handler (upstream's open_config_brs).
func openInFileManager(path string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "explorer", []string{path}
	default:
		cmd = "xdg-open"
	}
	if err := exec.Command(cmd, append(args, path)...).Start(); err != nil {
		// No desktop environment (or headless): tell the user where it is.
		fmt.Printf("open %s: %v\n", path, err)
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
			corner := (x+y < 2) || (x+(size-1-y) < 2) || ((size-1-x)+y < 2) || ((size-1-x)+(size-1-y) < 2)
			if x == 0 || y == 0 || x == size-1 || y == size-1 || corner {
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

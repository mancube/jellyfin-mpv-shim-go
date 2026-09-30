//go:build !nosystray

package ui

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/energye/systray"

	"mpv-shim/jfin"
)

// RunTray starts the desktop tray. It never blocks the caller (systray owns
// its own goroutine) and returns false when the platform has no tray host, so
// startup can log a hint and carry on. The items mirror upstream's pystray
// menu (gui_mgr.py): configure servers, console, player menu, config folder,
// quit.
func RunTray(s *Session) bool {
	ready := make(chan struct{})
	go systray.Run(func() {
		if icon := trayIcon(jfin.StateOffline); icon != nil {
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
		accounts.Click(func() {
			// Headless (no TUI): a window of its own, like "Show Console".
			if s.OpenSetup != nil {
				s.OpenSetup()
				return
			}
			s.RequestAccounts()
		})

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
		if s.UpdateNote != nil {
			if note := s.UpdateNote(); note != "" {
				update := systray.AddMenuItem(note, "Open the release page")
				update.Click(s.OpenUpdatePage)
				systray.AddSeparator()
			}
		}

		showConsole := systray.AddMenuItem("Show Console", "Open a status window for this instance")
		showConsole.Click(func() {
			if s.OpenConsole != nil {
				s.OpenConsole()
			}
		})

		connection := systray.AddMenuItem("Disconnect", "Drop the connection, keep playing")
		connection.Click(func() {
			// One item, two actions: the label follows the state.
			if s.Connected != nil && s.Connected() {
				if s.Disconnect != nil {
					s.Disconnect()
				}
				return
			}
			if s.Reconnect != nil {
				s.Reconnect()
			}
		})
		quit := systray.AddMenuItem("Quit", "Stop mpv-shim and close the window")
		quit.Click(func() {
			// The whole app: session, mpv and the TUI.
			if s.Quit != nil {
				s.Quit()
			}
			if s.QuitUI != nil {
				s.QuitUI()
			}
		})

		close(ready)
		go refreshTray(s, status, nowPlaying, connection)
	}, func() {})

	select {
	case <-ready:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

// connLabel is the human name of a connection state.
func connLabel(state int32) string {
	switch state {
	case jfin.StateConnected:
		return "online"
	case jfin.StateReconnecting:
		return "reconnecting"
	default:
		return "offline"
	}
}

func refreshTray(s *Session, status, nowPlaying, connection *systray.MenuItem) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	lastState := int32(-1) // force the first swap
	for range t.C {
		state := s.WS.State()
		if state != lastState {
			lastState = state
			setTrayIcon(state)
		}
		// The connection item mirrors the state: Disconnect while online,
		// Reconnect while offline.
		online := state == jfin.StateConnected
		if online {
			connection.SetTitle("Disconnect")
			connection.SetTooltip("Drop the connection, keep playing")
		} else {
			connection.SetTitle("Reconnect")
			connection.SetTooltip("Connect to " + s.Account.Server + " again")
		}

		text := "mpv-shim — " + connLabel(state)
		if s.UpdateNote != nil {
			if note := s.UpdateNote(); note != "" {
				text = note
			}
		}
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

// setTrayIcon swaps the tray icon (the status dot changes with the socket).
func setTrayIcon(state int32) {
	if icon := trayIcon(state); icon != nil {
		systray.SetIcon(icon)
	}
}

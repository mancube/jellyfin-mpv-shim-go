package player

// Remote control operations: the port of upstream player.py's toggle_pause,
// seek, set_volume, set_mute, play_next/prev, toggle_fullscreen, and the
// key/menu dispatch (menu_action, kb_seek). Every op reports progress
// afterwards so the web UI's remote panel stays in sync (upstream's
// timeline_handle()).

// Key bindings we claim at startup (upstream's kb_* defaults). The command is
// an mpv script-message; Player.handleClientMessage routes it. A single
// handler keeps the menu and the remote (GeneralCommand) on one code path.
// Key names are mpv's own (see `mpv --input-keylist`: ESC, ENTER, <, >).
var keyBindings = map[string]string{
	"c":     "menu",
	"ESC":   "back",
	"ENTER": "ok",
	"up":    "up",
	"down":  "down",
	"left":  "left",
	"right": "right",
	"SPACE": "pause",
	"f":     "fullscreen",
	"q":     "stop",
	"<":     "prev",
	">":     "next",
}

// BindKeys claims the shim's keys. Called after mpv spawns.
func (p *Player) BindKeys() {
	for key, action := range keyBindings {
		p.mpv.Keybind(key, "script-message shim-menu "+action)
	}
}

// handleClientMessage is the mpv client-message hook: our key bindings and
// (later) lua scripts speak through it.
func (p *Player) handleClientMessage(args []string) {
	if len(args) < 2 || args[0] != "shim-menu" {
		return
	}
	p.Key(args[1])
}

// Key is one key/remote action, shared by the mpv key bindings and the
// remote's navigation commands. Port of upstream's menu_action +
// kb_seek: with the menu closed, arrows seek, space pauses, etc.
func (p *Player) Key(action string) {
	switch action {
	case "menu", "home":
		p.MenuAction("home")
	case "back", "ok", "up", "down", "left", "right":
		p.MenuAction(action)
	case "pause":
		if p.menu.Shown() {
			p.MenuAction("ok")
		} else {
			p.TogglePause()
		}
	case "fullscreen":
		p.ToggleFullscreen()
	case "stop":
		p.Stop()
	case "next":
		p.Next()
	case "prev":
		p.Prev()
	default:
		p.log.Printf("key: unknown action %q", action)
	}
}

// MenuAction routes a navigation action to the OSD menu when it is open, and
// to the keyboard-seek fallbacks when it is not (upstream menu_action).
func (p *Player) MenuAction(action string) {
	if p.menu.Shown() {
		p.menu.Action(action)
		return
	}
	switch action {
	case "home":
		p.menu.Show()
	case "up":
		p.seekRelative(60)
	case "down":
		p.seekRelative(-60)
	case "left":
		p.seekRelative(-5)
	case "right":
		p.seekRelative(5)
	}
}

func (p *Player) seekRelative(delta float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pos := p.lastPos
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			pos = f
		}
	}
	p.seekLocked(pos+delta, false)
}

func (p *Player) isPausedLocked() bool {
	if x, err := p.mpv.GetProperty("pause"); err == nil {
		b, _ := x.(bool)
		return b
	}
	return p.lastPause
}

// SetPaused pauses/unpauses and reports the new state. pause_ignore mirrors
// upstream: our own pause change must not be treated as a remote one.
func (p *Player) SetPaused(paused bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil || p.aborted() {
		return
	}
	p.pauseIgnore = paused
	p.mpv.SetProperty("pause", paused)
	p.lastPause = paused
	p.sendProgressLocked()
}

func (p *Player) TogglePause() {
	p.SetPaused(!p.isPausedLocked())
}

// Seek moves playback to pos (absolute) or by pos (relative) and reports.
// Port of upstream seek().
func (p *Player) Seek(pos float64, absolute bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seekLocked(pos, absolute)
}

func (p *Player) seekLocked(pos float64, absolute bool) {
	if p.media == nil || p.aborted() {
		return
	}
	flags := "relative"
	if absolute {
		flags = "absolute+exact"
	}
	p.lastSeek = pos
	if err := p.mpv.Command("seek", pos, flags); err != nil {
		p.log.Printf("seek: %v", err)
		return
	}
	p.lastPos = pos
	p.sendProgressLocked()
}

// SetVolume sets the volume 0-100. Upstream only writes when the value
// changed: the server spams SetVolume.
func (p *Player) SetVolume(pct int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok && int(f) == pct {
			return
		}
	}
	p.mpv.SetProperty("volume", pct)
	p.sendProgressLocked()
}

// GetVolume returns the current volume (percent).
func (p *Player) GetVolume() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok {
			return int(f)
		}
	}
	return 0
}

// SetMute mutes/unmutes and reports.
func (p *Player) SetMute(mute bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.mpv.SetProperty("mute", mute)
	p.lastMute = mute
	p.sendProgressLocked()
}

// ToggleFullscreen flips the mpv fullscreen property.
func (p *Player) ToggleFullscreen() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil {
		return
	}
	cur, _ := p.mpv.GetProperty("fullscreen")
	on, _ := cur.(bool)
	p.mpv.SetProperty("fullscreen", !on)
}

// Next plays the next item in the queue (upstream play_next).
func (p *Player) Next() {
	p.menu.Hide()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jumpLocked(1)
}

// Prev plays the previous item in the queue (upstream play_prev).
func (p *Player) Prev() {
	p.menu.Hide()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.jumpLocked(-1)
}

func (p *Player) jumpLocked(delta int) {
	m := p.media
	if m == nil {
		return
	}
	target := m.Seq + delta
	if target < 0 || target >= len(m.Queue) {
		return
	}
	next, err := m.At(p.ctx, target)
	if err != nil || next == nil {
		p.log.Printf("jump: %v", err)
		return
	}
	p.sendStopped(true)
	if err := p.playLocked(next, 0); err != nil {
		p.log.Printf("play %d: %v", target, err)
	}
}

// sendProgressLocked posts a progress report after a remote/UI change
// (upstream timeline_handle + send_timeline). No-op without media.
func (p *Player) sendProgressLocked() { p.reportLocked() }

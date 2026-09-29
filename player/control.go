package player

// Remote control operations: the port of upstream player.py's toggle_pause,
// seek, set_volume, set_mute, play_next/prev, toggle_fullscreen, and the
// key/menu dispatch (menu_action, kb_seek). Every op reports progress
// afterwards so the web UI's remote panel stays in sync (upstream's
// timeline_handle()).

import (
	"strconv"

	"mpv-shim/jfin"
)

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
	"w":     "watched",
	"u":     "unwatched",
	"s":     "screenshot",
}

// BindKeys claims the shim's keys. Called after mpv spawns.
func (p *Player) BindKeys() {
	for key, action := range keyBindings {
		p.mpv.Keybind(key, "script-message shim-menu "+action)
	}
}

// handleClientMessage is the mpv client-message hook: our key bindings and
// the mouse script speak through it.
func (p *Player) handleClientMessage(args []string) {
	if len(args) < 2 || args[0] != "shim-menu" {
		return
	}
	switch args[1] {
	case "select":
		// mouse hover: highlight a row (upstream menu.mouse_select)
		if n, err := strconv.Atoi(args[2]); err == nil {
			p.menu.mouseSelect(n)
		}
	case "click":
		p.menu.mouseClick()
	default:
		p.Key(args[1])
	}
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
	case "media-next":
		p.mediaKeyNext()
	case "media-prev":
		p.mediaKeyPrev()
	case "stop":
		p.Stop()
	case "next":
		p.Next()
	case "prev":
		p.Prev()
	case "watched":
		p.WatchedSkip()
	case "unwatched":
		p.UnwatchedQuit()
	case "screenshot":
		p.Screenshot()
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
	// Upstream kb_seek: the arrow keys are seek steps, configurable.
	p.mu.Lock()
	o := p.opt
	p.mu.Unlock()
	seek := func(delta float64, vertical bool) {
		if vertical {
			p.seekBy(delta, o.SeekVExact)
			return
		}
		if o.UseWebSeek {
			// Honour the remote's own skip lengths when the server sent them.
			back, fwd, ok := p.webSeekLengths()
			if ok {
				if action == "left" {
					delta = back
				} else if action == "right" {
					delta = fwd
				}
			}
		}
		p.seekBy(delta, o.SeekHExact)
	}
	switch action {
	case "home":
		p.menu.Show()
	case "back":
		// Upstream: ESC outside the menu leaves fullscreen.
		p.setFullscreen(false)
	case "up":
		seek(o.SeekUp, true)
	case "down":
		seek(o.SeekDown, true)
	case "left":
		seek(o.SeekLeft, false)
	case "right":
		seek(o.SeekRight, false)
	}
}

// seekBy seeks by delta seconds, keyframe-exact when asked (upstream
// seek_h_exact/seek_v_exact).
func (p *Player) seekBy(delta float64, exact bool) {
	if exact {
		p.seekExact(delta)
		return
	}
	p.seekRelative(delta)
}

// seekExact is the keyframe-accurate variant of seekRelative.
func (p *Player) seekExact(delta float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil || p.aborted() {
		return
	}
	p.touchLocked()
	if err := p.mpv.Command("seek", delta, "relative+exact"); err != nil {
		p.log.Printf("exact seek %+v: %v", delta, err)
	}
	p.sendProgressLocked()
}

// seekRelative seeks by delta seconds (mpv keybindings: arrows, jump keys).
// mpv's `relative` is relative to the *current* position, so the amount itself
// is what we pass — passing pos+delta made every seek jump forward.
func (p *Player) seekRelative(delta float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil || p.aborted() {
		return
	}
	p.touchLocked()
	if err := p.mpv.Command("seek", delta, "relative"); err != nil {
		p.log.Printf("seek %+v: %v", delta, err)
		return
	}
	// mpv applies the seek asynchronously; re-read once it settled.
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			p.lastPos = f
		}
	}
	p.sendProgressLocked()
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
	p.touchLocked()
	if p.PauseReport {
		p.sendProgressLocked()
	}
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
	p.touchLocked()
	if err := p.mpv.Command("seek", pos, flags); err != nil {
		p.log.Printf("seek: %v", err)
		return
	}
	if absolute {
		p.lastPos = pos
	} else {
		// A relative seek moves by `pos` from wherever mpv is; ask it.
		if x, err := p.mpv.GetProperty("time-pos"); err == nil {
			if f, ok := x.(float64); ok {
				p.lastPos = f
			}
		}
	}
	p.sendProgressLocked()
}

// SetVolume sets the volume 0-100 (clamped). Upstream only writes when the
// value changed: the server spams SetVolume.
//
// Setting a volume above zero is an explicit "I want to hear this", so it
// also unmutes: otherwise the slider appears dead (mpv keeps the mute flag and
// the remote shows "muted" no matter what the volume is).
func (p *Player) SetVolume(pct int) {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	changed := false
	if pct > 0 { // volume intent un-mutes
		if x, err := p.mpv.GetProperty("mute"); err == nil {
			if m, ok := x.(bool); ok && m {
				p.mpv.SetProperty("mute", false)
				p.lastMute = false
				changed = true
			}
		}
	}
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok && int(f) == pct {
			if changed { // we only unmuted
				p.touchLocked()
				p.sendProgressLocked()
			}
			return // unchanged: no report (the server spams SetVolume)
		}
	}
	p.mpv.SetProperty("volume", pct)
	p.touchLocked()
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
	p.touchLocked()
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

func (p *Player) setFullscreen(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil {
		return
	}
	p.mpv.SetProperty("fullscreen", on)
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

// mediaKeyNext/mediaKeyPrev implement the media keys: seek, or skip episodes
// when media_key_seek is off (upstream handle_media_next/prev).
func (p *Player) mediaKeyNext() {
	p.mu.Lock()
	seek := p.opt.MediaKeySeek
	isIntro := p.isInIntroLocked()
	p.mu.Unlock()
	switch {
	case isIntro:
		p.skipIntro()
	case seek:
		p.MenuAction("right")
	default:
		p.Next()
	}
}

func (p *Player) mediaKeyPrev() {
	p.mu.Lock()
	seek := p.opt.MediaKeySeek
	p.mu.Unlock()
	if seek {
		p.MenuAction("left")
		return
	}
	p.Prev()
}

func (p *Player) skipIntro() {
	p.mu.Lock()
	pos := p.lastPos
	var end float64 = -1
	if p.media != nil {
		for i := range p.media.Video.Intros {
			in := &p.media.Video.Intros[i]
			if !in.HasTriggered && in.Type != "Outro" && pos >= in.Start && pos <= in.End {
				end = in.End
				in.HasTriggered = true
				break
			}
		}
	}
	aborted := p.aborted()
	p.mu.Unlock()
	if end < 0 || aborted {
		return
	}
	p.Seek(end, true)
	p.mpv.ShowText("Skipped Intro", 3000, 1)
}

// isInIntroLocked reports whether playback is inside an unskipped intro.
func (p *Player) isInIntroLocked() bool {
	if p.media == nil {
		return false
	}
	for i := range p.media.Video.Intros {
		in := &p.media.Video.Intros[i]
		if !in.HasTriggered && in.Type != "Outro" && p.lastPos >= in.Start && p.lastPos <= in.End {
			return true
		}
	}
	return false
}

// StepVolume changes the volume by delta (remote VolumeUp/VolumeDown).
// It no-ops when the current volume cannot be read: guessing 0 would mute.
func (p *Player) StepVolume(delta int) {
	p.mu.Lock()
	x, err := p.mpv.GetProperty("volume")
	p.mu.Unlock()
	f, ok := x.(float64)
	if err != nil || !ok {
		return
	}
	p.SetVolume(int(f) + delta)
}

// ToggleMute flips mute (remote ToggleMute).
func (p *Player) ToggleMute() {
	p.mu.Lock()
	x, err := p.mpv.GetProperty("mute")
	p.mu.Unlock()
	if err != nil {
		return
	}
	if m, ok := x.(bool); ok {
		p.SetMute(!m)
	}
}

// Screenshot writes a frame to ScreenshotDir (remote TakeScreenshot).
func (p *Player) Screenshot() {
	p.mu.Lock()
	dir := p.ScreenshotDir
	p.mu.Unlock()
	if dir == "" {
		p.log.Printf("screenshot: no directory configured")
		return
	}
	if err := p.mpv.Screenshot(dir); err != nil {
		p.log.Printf("screenshot: %v", err)
		return
	}
	p.mpv.ShowText("Screenshot saved", 2000, 1)
}

// WatchedSkip marks the current item watched and plays the next one
// (upstream watched_skip, key `w`).
func (p *Player) WatchedSkip() {
	p.mu.Lock()
	if p.media != nil {
		v := p.media.Video
		if err := v.M.C.SetPlayed(p.ctx, v.ID, true); err != nil {
			p.log.Printf("set watched: %v", err)
		}
	}
	p.mu.Unlock()
	p.Next()
}

// UnwatchedQuit stops playback and marks the item unwatched (upstream
// unwatched_quit, key `u`).
func (p *Player) UnwatchedQuit() {
	p.mu.Lock()
	var v *jfin.Video
	if p.media != nil {
		v = p.media.Video
	}
	ctx := p.ctx
	p.mu.Unlock()
	p.Stop()
	if v != nil {
		if err := v.M.C.SetPlayed(ctx, v.ID, false); err != nil {
			p.log.Printf("set unwatched: %v", err)
		}
	}
}

// sendProgressLocked posts a progress report after a remote/UI change
// (upstream timeline_handle + send_timeline). No-op without media.
func (p *Player) sendProgressLocked() { p.reportLocked() }

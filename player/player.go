package player

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"mpv-shim/jfin"
)

// Player is the playback state machine: it owns the current Media, drives
// mpv through the Mpv interface, and reports sessions to Jellyfin. Port of
// the core of upstream player.py (PlayerManager), minus menu/syncplay/
// trickplay (M3–M4).
type Player struct {
	mpv Mpv
	log *log.Logger

	mu    sync.Mutex
	ctx   context.Context
	media *jfin.Media
	url   string
	start time.Time
	// Timeline state (upstream names).
	shouldSendTimeline bool
	watchedMarked      bool
	warnedTranscode    bool
	lastPos            float64
	lastPause          bool
	lastMute           bool
	lastSeek           float64
	doNotHandlePause   bool
	pauseIgnore        bool
	fileErr            bool
	stopping           bool
	events             chan mpvEvent
	menu               *menu
	// last reported pause/mute/volume, to spot remote/UI-visible changes
	repPause  bool
	repMute   bool
	repVolume float64
	lastTick  time.Time
	// idle-stop: when > 0, playback is stopped after this long without
	// activity (upstream stop_idle + idle_cmd_delay).
	idleStop     time.Duration
	lastActivity time.Time
	// ScreenshotDir is where TakeScreenshot writes frames.
	ScreenshotDir string
	// PauseReport mirrors the pause_report setting: report immediately when
	// the remote pauses/unpauses.
	PauseReport bool
}

// mpvEvent is one queued IPC event (the hook runs in the reader goroutine).
type mpvEvent struct {
	name string
	args []string
}

func New(mpv Mpv, lg *log.Logger) *Player {
	if lg == nil {
		lg = log.Default()
	}
	p := &Player{mpv: mpv, log: lg, PauseReport: true} // pause_report defaults on
	p.menu = newMenu(p)
	return p
}

// Start sets the app lifetime ctx and launches the tick + exit watchers.
func (p *Player) Start(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.events = make(chan mpvEvent, 16)
	p.mu.Unlock()
	p.mpv.SetEventHook(p.handleEvent)
	go p.tickLoop(ctx)
	go p.exitWatch(ctx)
	go p.eventLoop()
	// Claim our keys as soon as mpv is up (retried on every spawn, since a
	// crash respawn starts a fresh mpv with no bindings).
	go p.bindKeysWhenAlive(ctx)
}

// Status is a snapshot for the TUI/systray. Player is busy, so Status never
// blocks: it answers from the last known state when mpv is unreachable.
type Status struct {
	Title    string
	Position float64
	Duration float64
	Paused   bool
	Volume   float64
	Mute     bool
	Playing  bool
}

// Status returns a snapshot of the current playback.
func (p *Player) Status() Status {
	p.mu.Lock()
	defer p.mu.Unlock()
	s := Status{Volume: 100}
	if p.media == nil {
		return s
	}
	s.Title = p.media.Video.ProperTitle()
	s.Duration = p.media.Video.GetDuration()
	s.Playing = p.mpv.Alive() && !p.aborted()
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			s.Position = f
		}
	}
	if x, err := p.mpv.GetProperty("pause"); err == nil {
		s.Paused, _ = x.(bool)
	}
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok {
			s.Volume = f
		}
	}
	if x, err := p.mpv.GetProperty("mute"); err == nil {
		s.Mute, _ = x.(bool)
	}
	return s
}

// SetIdleStop enables the idle stop: after d without playback (or without
// any activity) the player stops, as upstream's stop_idle does. 0 disables.
func (p *Player) SetIdleStop(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idleStop = d
	p.lastActivity = time.Now()
}

// touchLocked resets the idle timer (any user/remote activity counts).
func (p *Player) touchLocked() { p.lastActivity = time.Now() }

// idleCheckLocked implements the idle stop. Called from Tick.
func (p *Player) idleCheckLocked() {
	if p.idleStop <= 0 || p.stopping || p.media == nil {
		return // nothing loaded: there is no playback to stop
	}
	if !p.aborted() && !p.lastPause {
		p.touchLocked() // playing: not idle
		return
	}
	if time.Since(p.lastActivity) < p.idleStop {
		return
	}
	p.log.Printf("idle for %s — stopping playback", p.idleStop)
	p.touchLocked()
	p.stopLocked()
}

func (p *Player) HasVideo() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media != nil
}

// bindKeysWhenAlive waits for the first mpv spawn and claims the shim's keys.
// Key bindings do not survive a crash respawn, so re-claim on every new
// process (incarnation changes when EnsureRunning actually starts mpv).
func (p *Player) bindKeysWhenAlive(ctx context.Context) {
	bound := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		if !p.mpv.Alive() {
			continue
		}
		if id := p.mpv.Incarnation(); id == bound {
			continue
		}
		p.BindKeys()
		bound = p.mpv.Incarnation()
	}
}

// Play loads the media's video into mpv and reports session start.
// Port of upstream play + _play_media.
func (p *Player) Play(m *jfin.Media, offset float64) error {
	p.menu.Hide() // upstream play() hides the menu before loading
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.playLocked(m, offset)
}

func (p *Player) playLocked(m *jfin.Media, offset float64) error {
	v := m.Video
	if v == nil {
		return errors.New("player: media has no video")
	}
	p.shouldSendTimeline = false
	p.start = time.Now()
	url, err := v.PlaybackURL(p.ctx)
	if err != nil {
		return fmt.Errorf("playback url: %w", err)
	}
	if url == "" {
		return errors.New("no playable URL")
	}
	p.pauseIgnore = true
	p.doNotHandlePause = true
	p.touchLocked()
	if err := p.mpv.EnsureRunning(p.ctx); err != nil {
		p.doNotHandlePause = false
		return err
	}
	p.url = url
	p.mpv.SetProperty("keep-open", m.HasNext())
	if err := p.mpv.LoadFile(p.ctx, url); err != nil {
		p.doNotHandlePause = false
		return err
	}
	if !p.waitForDuration(30 * time.Second) {
		p.doNotHandlePause = false
		p.stopLocked()
		return errors.New("timeout waiting for media")
	}
	p.mpv.SetProperty("force-media-title", v.ProperTitle())
	p.media = m
	p.lastPos = 0
	p.lastPause = false
	p.lastMute = false
	p.watchedMarked = false
	p.configureStreams()
	if offset > 0 {
		p.lastSeek = offset
		p.lastPos = offset
		p.mpv.SetProperty("time-pos", offset)
	}
	p.mpv.SetProperty("pause", false)
	p.sendStart()
	p.shouldSendTimeline = true
	p.doNotHandlePause = false
	if !m.IsLocal && v.IsTranscode && !p.warnedTranscode {
		p.warnedTranscode = true
		p.mpv.ShowText("Your remote video is transcoding! Adjust remote_kbps if this is not needed.", 5000, 1)
	}
	return nil
}

// Stop tears down playback: stop report, mpv stop, transcode teardown.
// Port of upstream stop().
func (p *Player) Stop() {
	p.menu.Hide()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopLocked()
}

func (p *Player) stopLocked() {
	if p.media == nil {
		return
	}
	if p.aborted() {
		return
	}
	p.shouldSendTimeline = false
	opts := p.timelineOptions(false)
	p.mpv.SetProperty("pause", false)
	v := p.media.Video
	p.media = nil
	p.url = ""
	_ = p.mpv.Stop()
	v.TerminateTranscode(p.ctx)
	if err := v.M.C.SessionStopped(p.ctx, opts); err != nil {
		p.log.Printf("session stopped: %v", err)
	}
}

// InsertQueue adds ids to the queue (PlayNext after current, PlayLast at the
// end). Port of upstream insert_items.
func (p *Player) InsertQueue(ids []string, atEnd bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media != nil {
		p.media.Insert(ids, atEnd)
	}
}

// SetStreams switches audio (aid) and/or subtitle (sid) by Jellyfin stream
// index; nil = unchanged. Restarts playback when required (transcode audio,
// Encode-method subtitle). Port of upstream set_streams.
func (p *Player) SetStreams(aid, sid *int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil {
		return
	}
	if p.media.Video.SetStreams(aid, sid) {
		p.restartLocked()
	} else {
		p.configureStreams()
	}
}

func (p *Player) restartLocked() {
	pos := p.lastPos
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			pos = f
		}
	}
	// play() re-fetches PlaybackInfo (new transcode session, new
	// PlaySessionId) and re-reports session start.
	if err := p.playLocked(p.media, pos); err != nil {
		p.log.Printf("restart: %v", err)
	}
}

// configureStreams applies aid/sid to mpv. Port of upstream configure_streams.
func (p *Player) configureStreams() {
	v := p.media.Video
	if v.Aid != nil && !v.IsTranscode {
		if i, ok := v.AudioSeq[*v.Aid]; ok {
			p.mpv.SetProperty("audio", i) // upstream sets `audio` (mpv track id)
		}
	}
	if v.Sid == nil || *v.Sid == -1 {
		p.mpv.SetProperty("sub", "no")
	} else if i, ok := v.SubtitleSeq[*v.Sid]; ok {
		p.mpv.SetProperty("sub", i)
	} else if u, ok := v.SubtitleURL[*v.Sid]; ok {
		if err := p.mpv.SubAdd(u); err != nil {
			p.log.Printf("external subtitle: %v", err)
		}
	}
}

// Shutdown is the app-exit path: stop report, then kill mpv.
func (p *Player) Shutdown() {
	p.mu.Lock()
	p.stopping = true
	p.mu.Unlock()
	p.Stop()
	p.mpv.Kill()
}

// handleEvent is the mpv IPC event hook. It only enqueues — all work happens
// in eventLoop, because the hook runs in the IPC reader goroutine, which
// must never block (Proc commands wait for its responses). Port of the
// upstream actionThread hop.
func (p *Player) handleEvent(name string, data json.RawMessage) {
	// mpv v0.41 reports load failures as end-file {"reason":"error",
	// "file_error":...}; older mpv sends a separate file-error event.
	var failed bool
	switch name {
	case "client-message":
		var args []string
		if json.Unmarshal(data, &args) != nil {
			return
		}
		select {
		case p.events <- mpvEvent{name: name, args: args}:
		default:
		}
		return
	case "file-error":
		failed = true
	case "end-file":
		var e struct {
			Reason    string `json:"reason"`
			FileError string `json:"file_error"`
		}
		if json.Unmarshal(data, &e) == nil && (e.Reason == "error" || e.FileError != "") {
			failed = true
		}
	default:
		return
	}
	if failed {
		p.mu.Lock()
		p.fileErr = true
		p.mu.Unlock()
	}
	if failed {
		name = "file-error"
	}
	select {
	case p.events <- mpvEvent{name: name}:
	default: // queue full: drop (end-file at worst retries on next event)
	}
}

func (p *Player) eventLoop() {
	for ev := range p.events {
		if ev.name == "client-message" {
			p.handleClientMessage(ev.args)
			continue
		}
		if ev.name == "file-error" {
			p.mu.Lock()
			p.fileErr = false
			p.log.Printf("mpv failed to load media")
			p.stopLocked()
			p.mu.Unlock()
			continue
		}
		p.menu.Hide() // the queue advanced; the old menu is stale
		p.mu.Lock()
		p.handleEndFileLocked()
		p.mu.Unlock()
	}
}

// handleEndFileLocked is the end-file path: mark watched, advance the queue
// (auto-play), or close out the session. Port of upstream finished_callback.
func (p *Player) handleEndFileLocked() {
	m := p.media
	if m == nil {
		return
	}
	v := m.Video
	if !p.watchedMarked {
		p.watchedMarked = true
		if err := v.M.C.SetPlayed(p.ctx, v.ID, true); err != nil {
			p.log.Printf("set watched: %v", err)
		}
	}
	if m.HasNext() {
		p.sendStopped(true)
		next, err := m.Next(p.ctx)
		if err != nil || next == nil {
			p.log.Printf("queue advance: %v", err)
			p.media = nil
			p.shouldSendTimeline = false
			p.url = ""
			return
		}
		if err := p.playLocked(next, 0); err != nil {
			p.log.Printf("play next: %v", err)
			p.media = nil
			p.shouldSendTimeline = false
			p.url = ""
		}
	} else {
		p.sendStopped(true)
		p.media = nil
		p.shouldSendTimeline = false
		p.url = ""
	}
}

// exitWatch reacts to mpv process death: graceful → stop report; crash →
// respawn and resume (PLAN M2 DoD).
func (p *Player) exitWatch(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.mpv.Exit():
		}
		p.handleExit()
	}
}

func (p *Player) handleExit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopping || p.media == nil {
		return
	}
	if p.mpv.Graceful() {
		v := p.media.Video
		opts := p.timelineOptions(false)
		p.shouldSendTimeline = false
		p.media = nil
		p.url = ""
		v.TerminateTranscode(p.ctx)
		if err := v.M.C.SessionStopped(p.ctx, opts); err != nil {
			p.log.Printf("session stopped (graceful): %v", err)
		}
		return
	}
	// Crash: kill -9 / OOM / segfault. Respawn and resume at the last
	// known position and pause state.
	p.log.Printf("mpv exited unexpectedly — restarting playback")
	url, m := p.url, p.media
	if url == "" {
		p.media = nil
		return
	}
	v := m.Video
	pos, pause := p.lastPos, p.lastPause
	p.doNotHandlePause = true
	p.fileErr = false
	if err := p.mpv.EnsureRunning(p.ctx); err != nil {
		p.log.Printf("mpv restart failed: %v", err)
		p.media = nil
		p.shouldSendTimeline = false
		p.doNotHandlePause = false
		return
	}
	p.mpv.SetProperty("keep-open", m.HasNext())
	if err := p.mpv.LoadFile(p.ctx, url); err != nil {
		p.log.Printf("mpv restart load: %v", err)
		p.doNotHandlePause = false
		return
	}
	p.waitForDuration(30 * time.Second)
	p.mpv.SetProperty("force-media-title", v.ProperTitle())
	p.configureStreams()
	if pos > 0 {
		p.mpv.SetProperty("time-pos", pos)
	}
	p.mpv.SetProperty("pause", pause)
	p.doNotHandlePause = false
}

func (p *Player) tickLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.Tick()
		}
	}
}

// Tick sends a progress report. Port of the upstream timeline loop: it skips
// while paused (pause reporting arrives with M3's pause echo) and marks the
// item watched at ≥90%. It also carries the M3 feedback work upstream did with
// property observers: pause/mute/volume changes and local seeks are reported
// so the web UI's remote panel follows mpv.
func (p *Player) Tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.idleCheckLocked() // must run even with no media (that is the idle case)
	if p.media == nil || !p.shouldSendTimeline {
		return
	}
	if !p.mpv.Alive() {
		return
	}
	if p.aborted() {
		return
	}
	pause := false
	if x, err := p.mpv.GetProperty("pause"); err == nil {
		pause, _ = x.(bool)
	}
	if pause {
		// While paused we still report: the position does not move, but a
		// pause the web UI did not ask for (mpv's own OSC/keymap) and
		// volume/mute changes must still reach the remote panel.
		p.lastPause = pause
		// NB: no touchLocked() — a paused player is idle, and the idle check
		// is what eventually stops it (upstream idle_when_paused/stop_idle).
		if pause != p.repPause || p.volumeChangedLocked() {
			p.reportLocked()
		}
		return
	}
	p.lastPause = pause
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			// A jump far bigger than a tick's worth of playback is a local
			// seek: report it right away so the UI seek bar follows (the
			// normal work below still runs).
			if !p.lastTick.IsZero() && abs(f-p.lastPos) > 8 {
				p.lastPos = f
				p.reportLocked()
			}
			p.lastPos = f
		}
	}
	p.lastTick = time.Now()
	v := p.media.Video
	if !p.watchedMarked {
		if dur := v.GetDuration(); dur > 0 && p.lastPos >= 0.9*dur {
			p.watchedMarked = true
			if err := v.M.C.SetPlayed(p.ctx, v.ID, true); err != nil {
				p.log.Printf("set watched: %v", err)
			}
		}
	}
	p.introCheckLocked()
	p.reportLocked()
}

// reportLocked posts progress and remembers the state it reported, so the
// next Tick can spot changes (upstream's pause/mute de-dupes).
func (p *Player) reportLocked() {
	if p.media == nil || !p.shouldSendTimeline {
		return
	}
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			p.lastPos = f
		}
	}
	if x, err := p.mpv.GetProperty("mute"); err == nil {
		p.repMute, _ = x.(bool)
	}
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok {
			p.repVolume = f
		}
	}
	p.repPause = p.lastPause
	if err := p.media.C.SessionProgress(p.ctx, p.timelineOptions(false)); err != nil {
		p.log.Printf("progress: %v", err)
	}
}

// volumeChangedLocked reports whether mute or volume moved since the last
// progress report, so the web UI's sliders follow local/remote changes.
func (p *Player) volumeChangedLocked() bool {
	mute := p.repMute
	if x, err := p.mpv.GetProperty("mute"); err == nil {
		mute, _ = x.(bool)
	}
	vol := p.repVolume
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok {
			vol = f
		}
	}
	return mute != p.repMute || int(vol) != int(p.repVolume)
}

// introCheckLocked implements upstream's skip_intro/skip_credits: seek past
// the segment once, and prompt ("Seek to Skip Intro") if only the prompt
// setting applies. ponytail: the settings are single booleans (always-skip
// vs prompt-only is not distinguished); add flags if that distinction is ever
// wanted.
func (p *Player) introCheckLocked() {
	if p.media == nil || p.aborted() || p.menu.Shown() {
		return
	}
	cfg := p.media.Cfg
	if !cfg.SkipIntro && !cfg.SkipCredits {
		return
	}
	pos := p.lastPos
	v := p.media.Video
	for i := range v.Intros {
		in := &v.Intros[i]
		if in.HasTriggered || pos < in.Start || pos > in.End {
			continue
		}
		enabled := in.Type == "Outro" && cfg.SkipCredits
		if in.Type != "Outro" {
			enabled = cfg.SkipIntro
		}
		if !enabled {
			continue
		}
		in.HasTriggered = true
		p.log.Printf("skipping %s: seek to %.1fs", in.Type, in.End)
		if err := p.mpv.Command("seek", in.End, "absolute+exact"); err != nil {
			p.log.Printf("intro skip: %v", err)
			return
		}
		msg := "Skipped Intro"
		if in.Type == "Outro" {
			msg = "Skipped Credits"
		}
		p.mpv.ShowText(msg, 3000, 1)
		p.lastPos = in.End
		p.sendProgressLocked()
		return
	}
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}

func (p *Player) waitForDuration(d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if p.fileErr {
			return false
		}
		if x, err := p.mpv.GetProperty("duration"); err == nil && x != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

func (p *Player) aborted() bool {
	x, err := p.mpv.GetProperty("playback-abort")
	if err != nil {
		return true
	}
	b, _ := x.(bool)
	return b
}

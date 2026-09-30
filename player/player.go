package player

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"mpv-shim/jfin"
)

// Player is the playback state machine: it owns the current Media, drives
// mpv through the Mpv interface, and reports sessions to Jellyfin. Port of
// the core of upstream player.py (PlayerManager), minus menu/syncplay/
// trickplay, syncplay and display mirroring (out of scope).
type Player struct {
	mpv Mpv
	log *log.Logger

	mu    sync.Mutex
	ctx   context.Context
	media *jfin.Media
	url   string
	// entryID is the playlist entry mpv gave the current load. Every load
	// replaces the entry, so the end-file of the previous one is stale.
	// Atomic: the IPC reader goroutine reads it and must never block on
	// p.mu — it is the goroutine that delivers mpv's command replies, so a
	// blocked reader deadlocks every player holding p.mu.
	entryID atomic.Int64
	start   time.Time
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
	// loadFailed: mpv could not load a file. Atomic for the same reason as
	// entryID (the reader goroutine writes it).
	loadFailed atomic.Bool
	stopping   bool
	events     chan mpvEvent
	menu       *menu
	// last reported pause/mute/volume, to spot remote/UI-visible changes
	repPause    bool
	repMute     bool
	repVolume   float64
	lastTick    time.Time
	crashLoop   int             // consecutive crash restarts without playback progress
	initialEcho map[string]bool // first property-change per prop is mpv's echo
	prompted    bool            // intro "ask to skip" prompt already shown
	lastReport  time.Time       // last progress report we sent (report throttling)
	// idle-stop: when > 0, playback is stopped after this long without
	// activity (upstream stop_idle + idle_cmd_delay).
	lastActivity time.Time
	// ScreenshotDir is where TakeScreenshot writes frames.
	ScreenshotDir string
	// opt holds the runtime settings; save persists changes made by the OSD
	// preference menus.
	opt  Options
	save func(Options)
	// onProfileChange is called (without the lock) when a preference changed
	// what we ask the server for.
	onProfileChange func()
	// liveCfg returns the media config to re-request with. A Media captures
	// the config it was built with, so without this a profile change would
	// re-request the very stream the user just turned off.
	liveCfg func() jfin.MediaConfig

	// update check (upstream update_check.py)
	updateURL     string
	updateEnabled bool
	updateNotify  bool
	updMu         sync.Mutex
	update        UpdateState
	version       string
	// PauseReport mirrors the pause_report setting: report immediately when
	// the remote pauses/unpauses.
	PauseReport bool
}

// mpvEvent is one queued IPC event (the hook runs in the reader goroutine).
type mpvEvent struct {
	name   string
	args   []string
	prop   string          // property-change: property name
	data   json.RawMessage // property-change: new value
	reason string          // end-file: "eof" | "stop" | "quit" | "error" | ...
}

func New(mpv Mpv, lg *log.Logger) *Player {
	if lg == nil {
		lg = log.Default()
	}
	// pause_report defaults on; initialEcho collects mpv's subscribe echoes.
	p := &Player{
		mpv: mpv, log: lg, PauseReport: true,
		initialEcho:  map[string]bool{},
		opt:          DefaultOptions(),
		updateNotify: true, // notify_updates defaults on
	}
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
	go p.afterSpawn(ctx)
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

// SetIdleStop is the startup shortcut for the idle_stop/idle_delay_s settings;
// at runtime the preference menus change Options.IdleStop/IdleStopAfter.
func (p *Player) SetIdleStop(enabled bool, after time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.opt.IdleStop, p.opt.IdleStopAfter = enabled, after
	p.lastActivity = time.Now()
}

// touchLocked resets the idle timer (any user/remote activity counts).
func (p *Player) touchLocked() { p.lastActivity = time.Now() }

// idleCheckLocked implements the idle stop. Called from Tick.
func (p *Player) idleCheckLocked() {
	if !p.opt.IdleStop || p.opt.IdleStopAfter <= 0 || p.stopping || p.media == nil {
		return // nothing loaded: there is no playback to stop
	}
	stopAfter := p.opt.IdleStopAfter
	// A paused player counts as idle; the stop below handles it.
	if !p.aborted() && !p.lastPause {
		p.touchLocked() // playing: not idle
		return
	}
	if time.Since(p.lastActivity) < stopAfter {
		return
	}
	p.log.Printf("idle for %s — stopping playback", stopAfter)
	p.touchLocked()
	p.stopLocked()
	if p.opt.ShellCmds.Idle != "" {
		p.runShell("idle_cmd", p.opt.ShellCmds.Idle)
	}
}

// timeoutLocked is how long we wait for mpv to report a duration.
func (p *Player) timeoutLocked() time.Duration {
	if p.opt.PlaybackTimeout > 0 {
		return p.opt.PlaybackTimeout
	}
	return 30 * time.Second
}

// applySubtitleStyleLocked pushes sub-scale/sub-color/sub-pos after a file
// load (mpv resets some of them per file).
func (p *Player) applySubtitleStyleLocked() {
	if p.opt.SubSize > 0 {
		p.mpv.SetProperty("sub-scale", fmt.Sprintf("%.2f", float64(p.opt.SubSize)/100))
	}
	if p.opt.SubColor != "" {
		p.mpv.SetProperty("sub-color", p.opt.SubColor)
	}
	if pos := subPos(p.opt.SubPosition); pos != "" {
		p.mpv.SetProperty("sub-pos", pos)
	}
}

func (p *Player) HasVideo() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media != nil
}

// afterSpawn sets up the per-process bits: the key bindings and the property
// observers. Neither survives a crash respawn, so both are (re)applied on
// every new mpv incarnation.
func (p *Player) afterSpawn(ctx context.Context) {
	bound := 0
	for {
		if p.mpv.Alive() {
			if id := p.mpv.Incarnation(); id != bound {
				p.BindKeys()
				p.mu.Lock()
				p.initialEcho = map[string]bool{}
				p.mu.Unlock()
				for _, prop := range observedProps {
					// Mark the echo before subscribing: mpv may answer with the
					// current value before Observe() even returns.
					p.mu.Lock()
					p.initialEcho[prop] = true
					p.mu.Unlock()
					if err := p.mpv.Observe(prop); err != nil {
						p.log.Printf("observe %s: %v", prop, err)
					}
				}
				bound = id
			}
		}
		// 200 ms while there is no mpv (so the first spawn is picked up
		// quickly), then idle at 1 s.
		wait := time.Second
		if !p.mpv.Alive() {
			wait = 200 * time.Millisecond
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// maxCrashRestarts bounds the crash-restart loop: three respawns without the
// position moving means something is fundamentally wrong (bad file, broken
// video output), and retrying forever just burns CPU and spawns processes.
const maxCrashRestarts = 3

// introSkipWindow is how close to the end of an intro/credits segment the
// automatic skip (and the "ask to skip" prompt) kicks in. Upstream uses
// settings.local_kbps-derived timing; 15 s matches its UX for most content.
const introSkipWindow = 15 * time.Second

// observedProps are the mpv properties we watch for immediate UI feedback.
var observedProps = []string{"pause", "mute", "volume", "seeking", "time-pos", "aid", "sid"}

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
	// The item carries the config it was first built with; the settings may
	// have changed since (a transcode profile change is exactly this), and a
	// re-request has to use the current one or it gets the same stream back.
	if p.liveCfg != nil {
		m.Cfg = p.liveCfg()
	}
	p.shouldSendTimeline = false
	p.start = time.Now()
	p.runShell("pre_media_cmd", p.opt.ShellCmds.PreMedia)
	url, err := v.PlaybackURL(p.ctx)
	if err != nil {
		return fmt.Errorf("playback url: %w", err)
	}
	if url == "" {
		return errors.New("no playable URL")
	}
	if p.opt.LogDecisions {
		p.log.Printf("Playing: %s", url)
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
	id, err := p.mpv.LoadFile(p.ctx, url)
	if err != nil {
		p.doNotHandlePause = false
		return err
	}
	p.entryID.Store(id)
	if !p.waitForDuration(p.timeoutLocked()) {
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
	p.restoreVolumeLocked()
	p.applySubtitleStyleLocked()
	if p.opt.Fullscreen {
		p.mpv.SetProperty("fullscreen", true)
	}
	p.runShell("play_cmd", p.opt.ShellCmds.Play)
	p.loadChaptersLocked()
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
	// An aborted playback (failed load, mpv gone) still has to be cleaned up:
	// the server must hear the stop, or it keeps showing us as playing.
	aborted := p.aborted()
	p.shouldSendTimeline = false
	opts := p.timelineOptions(false)
	v := p.media.Video
	p.media = nil
	p.url = ""
	if !aborted {
		p.mpv.SetProperty("pause", false)
		_ = p.mpv.Stop()
	}
	v.TerminateTranscode(p.ctx)
	if err := v.M.C.SessionStopped(p.ctx, opts); err != nil {
		p.log.Printf("session stopped: %v", err)
	}
	p.runShell("stop_cmd", p.opt.ShellCmds.Stop)
	commitVolume() // playback ended: a good moment to persist the volume state
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

// Restart re-runs playback for the current item, resuming where it is. It is
// what a change to the transcode profile (bitrate, codec policy, direct play)
// triggers: the old stream no longer matches what we would ask for, so the item
// is re-requested with the new profile and continues from the same position.
func (p *Player) Restart() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil {
		return false
	}
	pos := p.lastPos
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			pos = f
		}
	}
	p.log.Printf("re-requesting the stream, resuming at %.1fs", pos)
	p.restartLocked()
	return true
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
	case "property-change":
		var pc struct {
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		}
		if json.Unmarshal(data, &pc) != nil {
			return
		}
		select {
		case p.events <- mpvEvent{name: name, prop: pc.Name, data: pc.Data}:
		default: // never drop a property change: they are the UI feedback
		}
		return
	case "file-error":
		failed = true
	case "end-file":
		// mpv 0.41: reason is "eof" (played to the end), "stop" (something
		// called stop), "quit" (window closed / mpv shutting down), "error".
		// Only "eof" means the episode finished; treating "stop"/"quit" as
		// finished is what made closing the window auto-advance the queue in a
		// loop, spawning a new mpv for every following episode.
		var e struct {
			Reason          string `json:"reason"`
			FileError       string `json:"file_error"`
			PlaylistEntryID int64  `json:"playlist_entry_id"`
		}
		_ = json.Unmarshal(data, &e)
		// `loadfile replace` ends the previous file, and that end-file
		// arrives around the new load: acting on it would stop the stream we
		// just requested (the transcode re-request, a track/profile change).
		if cur := p.entryID.Load(); cur != 0 && e.PlaylistEntryID != 0 && e.PlaylistEntryID != cur {
			p.log.Printf("ignoring end-file of replaced entry %d (playing %d)", e.PlaylistEntryID, cur)
			return
		}
		reason := e.Reason
		if e.Reason == "error" || e.FileError != "" {
			failed = true
		}
		if reason == "" {
			reason = "stop" // be conservative: no auto-advance without proof
		}
		select {
		case p.events <- mpvEvent{name: "end-file", reason: reason}:
		default:
		}
		return
	default:
		return
	}
	if failed {
		p.loadFailed.Store(true)
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
		if ev.name == "property-change" {
			p.onPropertyChange(ev.prop, ev.data)
			continue
		}
		if ev.name == "end-file" {
			p.menu.Hide() // the item is over; the menu belongs to it
			p.mu.Lock()
			p.onEndFileLocked(ev.reason)
			p.mu.Unlock()
			continue
		}
		if ev.name == "file-error" {
			p.mu.Lock()
			p.loadFailed.Store(false)
			p.log.Printf("mpv failed to load media")
			p.stopLocked()
			p.mu.Unlock()
			continue
		}
	}
}

// onEndFileLocked routes an end-file by reason. Only "eof" (the file really
// played out) marks watched and advances the queue; "stop"/"quit" means the
// user or mpv ended playback, so we just close out the session.
func (p *Player) onEndFileLocked(reason string) {
	switch reason {
	case "eof":
		p.handleEndFileLocked()
	case "error":
		p.log.Printf("mpv: playback error")
		p.stopLocked()
	default: // "stop" (remote stop, `q`) or "quit" (window closed)
		p.log.Printf("mpv: playback ended (%s) — not advancing the queue", reason)
		p.stopLocked()
	}
}

// handleEndFileLocked is the natural-end path: mark watched, advance the queue
// (auto-play), or close out the session. Port of upstream finished_callback.
func (p *Player) handleEndFileLocked() {
	m := p.media
	if m == nil {
		return
	}
	v := m.Video
	if !p.watchedMarked {
		p.watchedMarked = true
		if p.opt.ForceSetPlayed || p.opt.AutoPlay {
			if err := v.M.C.SetPlayed(p.ctx, v.ID, true); err != nil {
				p.log.Printf("set watched: %v", err)
			}
		}
	}
	if m.HasNext() && p.opt.AutoPlay {
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
	p.runShell("media_ended_cmd", p.opt.ShellCmds.MediaEnded)
}

// exitWatch reacts to mpv process death: graceful → stop report; crash →
// respawn and resume.
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
		commitVolume() // mpv is going away
		return
	}
	// Crash: kill -9 / OOM / segfault. Respawn and resume at the last
	// known position and pause state. Repeated crashes (a file mpv cannot
	// play, a broken VO, a respawn loop) must not turn into an endless
	// process churn, so give up after a few tries without progress.
	if p.crashLoop >= maxCrashRestarts {
		p.log.Printf("mpv crashed %d times in a row without playing — giving up", p.crashLoop)
		p.crashLoop = 0
		p.stopLocked()
		return
	}
	p.crashLoop++
	p.log.Printf("mpv exited unexpectedly — restarting playback (%d/%d)", p.crashLoop, maxCrashRestarts)
	url, m := p.url, p.media
	if url == "" {
		p.media = nil
		return
	}
	v := m.Video
	pos, pause := p.lastPos, p.lastPause

	// A transcode's HLS URL belongs to a PlaySessionId that may already be
	// gone; re-running play() re-fetches PlaybackInfo and terminates the old
	// encoding (upstream restart_playback). Direct play can reuse the URL.
	if v.IsTranscode {
		p.log.Printf("mpv crashed during transcode — re-requesting the stream")
		if err := p.playLocked(m, pos); err != nil {
			p.log.Printf("transcode restart: %v", err)
			p.media = nil
			p.shouldSendTimeline = false
		}
		return
	}

	p.doNotHandlePause = true
	p.loadFailed.Store(false)
	if err := p.mpv.EnsureRunning(p.ctx); err != nil {
		p.log.Printf("mpv restart failed: %v", err)
		p.media = nil
		p.shouldSendTimeline = false
		p.doNotHandlePause = false
		return
	}
	p.mpv.SetProperty("keep-open", m.HasNext())
	id, err := p.mpv.LoadFile(p.ctx, url)
	if err != nil {
		p.log.Printf("mpv restart load: %v", err)
		p.doNotHandlePause = false
		return
	}
	p.entryID.Store(id)
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
// while paused and marks the item watched at ≥90%. Like upstream's property
// observers, it also reports pause/mute/volume changes and local seeks so the
// web UI's remote panel follows mpv (see observe.go for the immediate path).
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
			if f > 0.5 {
				p.crashLoop = 0 // it is playing again
			}
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
	p.lastReport = time.Now()
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
// setting applies. Note: always-skip and prompt-only are the same boolean
// here; add separate flags if that distinction is ever wanted.
func (p *Player) introCheckLocked() {
	// Note: no menu check here — the menu pauses playback, so the paused
	// branch of Tick already covers it, and asking the menu for its state
	// while holding p.mu would invert the lock order.
	if p.media == nil || p.aborted() {
		return
	}
	// Upstream splits "always skip" from "ask to skip"; both are honoured here:
	// always → jump silently, ask → show the prompt.
	o := p.opt
	pos := p.lastPos
	v := p.media.Video
	for i := range v.Intros {
		in := &v.Intros[i]
		if in.HasTriggered || pos < in.Start || pos > in.End {
			continue
		}
		always, ask := o.SkipIntroAlways, o.SkipIntro
		if in.Type == "Outro" {
			always, ask = o.SkipCreditsAlways, o.SkipCredits
		}
		if !always && !ask {
			continue
		}
		if always || in.End-pos <= introSkipWindow.Seconds() {
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
			p.reportLocked()
			return
		}
		// "Ask to skip": only prompt near the end of the segment.
		if !p.prompted {
			p.prompted = true
			msg := "Seek to Skip Intro"
			if in.Type == "Outro" {
				msg = "Seek to Skip Credits"
			}
			p.mpv.ShowText(msg, 3000, 1)
		}
		return
	}
	p.prompted = false
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
		if p.loadFailed.Load() {
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

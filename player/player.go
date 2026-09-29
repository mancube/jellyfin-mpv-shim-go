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

	mu      sync.Mutex
	ctx     context.Context
	media   *jfin.Media
	url     string
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
	fileErr            bool
	stopping           bool
	events             chan string
}

func New(mpv Mpv, lg *log.Logger) *Player {
	if lg == nil {
		lg = log.Default()
	}
	return &Player{mpv: mpv, log: lg}
}

// Start sets the app lifetime ctx and launches the tick + exit watchers.
func (p *Player) Start(ctx context.Context) {
	p.mu.Lock()
	p.ctx = ctx
	p.events = make(chan string, 16)
	p.mu.Unlock()
	p.mpv.SetEventHook(p.handleEvent)
	go p.tickLoop(ctx)
	go p.exitWatch(ctx)
	go p.eventLoop()
}

func (p *Player) HasVideo() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.media != nil
}

// Play loads the media's video into mpv and reports session start.
// Port of upstream play + _play_media.
func (p *Player) Play(m *jfin.Media, offset float64) error {
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
	case p.events <- name:
	default: // queue full: drop (end-file at worst retries on next event)
	}
}

func (p *Player) eventLoop() {
	for name := range p.events {
		if name == "file-error" {
			p.mu.Lock()
			p.fileErr = false
			p.log.Printf("mpv failed to load media")
			p.stopLocked()
			p.mu.Unlock()
			continue
		}
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
// item watched at ≥90%.
func (p *Player) Tick() {
	p.mu.Lock()
	defer p.mu.Unlock()
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
		return
	}
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			p.lastPos = f
		}
	}
	v := p.media.Video
	if !p.watchedMarked {
		if dur := v.GetDuration(); dur > 0 && p.lastPos >= 0.9*dur {
			p.watchedMarked = true
			if err := v.M.C.SetPlayed(p.ctx, v.ID, true); err != nil {
				p.log.Printf("set watched: %v", err)
			}
		}
	}
	opts := p.timelineOptions(false)
	if err := v.M.C.SessionProgress(p.ctx, opts); err != nil {
		p.log.Printf("progress: %v", err)
	}
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

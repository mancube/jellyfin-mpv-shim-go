package player

// Options are the runtime knobs that upstream exposes as settings and that
// the player applies live (the OSD preference menus write to them and call
// Save). They are copied from config at startup and are safe to read from the
// player's mutex.

import (
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

// Options mirrors the subset of upstream's settings the player cares about.
type Options struct {
	// Keys overrides the default keybindings: mpv key name -> shim action.
	Keys map[string]string
	// MediaKeySeek: media keys seek instead of skipping episodes.
	MediaKeySeek bool
	// Seek steps (seconds) and whether the seek is keyframe-exact.
	SeekUp, SeekDown, SeekLeft, SeekRight float64
	SeekHExact, SeekVExact                bool
	// UseWebSeek prefers the remote's own skip lengths (CustomPrefs
	// SkipBackLength/SkipForwardLength) over the configured steps.
	UseWebSeek bool

	// Subtitle styling, applied with mpv's sub-scale/sub-color/sub-pos.
	SubSize     int // percent, 100 = mpv default
	SubColor    string
	SubPosition string // bottom | top | middle

	// Behaviour toggles. The OSD preference menus (prefs.go) write these live
	// and call Save, so they must stay in sync with config.json.
	AutoPlay             bool
	Fullscreen           bool
	EnableOSC            bool
	ForceSetPlayed       bool
	SkipIntro            bool
	SkipIntroAlways      bool
	SkipCredits          bool
	SkipCreditsAlways    bool
	MenuMouse            bool
	WriteLogs            bool
	CheckUpdates         bool
	TranscodeHi10p       bool
	TranscodeHDR         bool
	TranscodeDolbyVision bool
	DirectPaths          bool
	RemoteDirectPaths    bool
	RemoteKbps           int // transcode quality preset
	PlaybackTimeout      time.Duration
	IdleCmdDelay         time.Duration
	ShellCmds            ShellCmds
	LogDecisions         bool // log the URL and track choices (upstream)
}

// ShellCmds are the upstream lifecycle hooks, run detached and best-effort.
type ShellCmds struct {
	PreMedia, Play, Stop, MediaEnded, Idle, IdleEnded string
}

// DefaultOptions returns upstream's default runtime options, so a Player works
// sensibly before (or without) SetOptions.
func DefaultOptions() Options {
	return Options{
		Keys:            nil,
		SeekUp:          60,
		SeekDown:        -60,
		SeekLeft:        -5,
		SeekRight:       5,
		RemoteKbps:      25000,
		SkipIntro:       true,
		SkipCredits:     true,
		MenuMouse:       true,
		SubSize:         100,
		SubColor:        "#FFFFFFFF",
		SubPosition:     "bottom",
		AutoPlay:        true,
		Fullscreen:      true,
		EnableOSC:       true,
		PlaybackTimeout: 30 * time.Second,
		IdleCmdDelay:    60 * time.Second,
	}
}

// DefaultKeyBindings is upstream's kb_* default set, expressed as mpv key name
// -> shim action. Users override individual keys via `key_bindings`.
var DefaultKeyBindings = map[string]string{
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

// keyBindings resolves the effective bindings: defaults plus the user's
// overrides, and `null` (empty action) to unbind.
func (o Options) keyBindings() map[string]string {
	out := make(map[string]string, len(DefaultKeyBindings)+len(o.Keys))
	for k, v := range DefaultKeyBindings {
		out[k] = v
	}
	for k, v := range o.Keys {
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		if v == "" { // explicitly unbound
			delete(out, k)
			continue
		}
		out[k] = v
	}
	return out
}

// SetOptions installs the runtime options (at startup, and again whenever the
// OSD preference menus change something).
func (p *Player) SetOptions(o Options) {
	p.mu.Lock()
	p.opt = o
	p.mu.Unlock()
	// Both of these live in mpv, so they only apply once it is up; playLocked
	// re-applies the subtitle style for every file anyway.
	if !p.mpv.Alive() {
		return
	}
	if o.SubSize > 0 || o.SubColor != "" || o.SubPosition != "" {
		p.ApplySubtitleStyle()
	}
	p.BindKeys() // keybindings live in mpv too
}

// Options returns the current options (for the UI to read/render).
func (p *Player) Options() Options {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.opt
}

// SetSaveFunc installs the callback the preference menus use to persist a
// settings change. It receives the new options, because it is called with
// p.mu held: re-entering the player from here would deadlock.
func (p *Player) SetSaveFunc(f func(Options)) {
	p.mu.Lock()
	p.save = f
	p.mu.Unlock()
}

// saveNowLocked persists the settings, if a save function is installed. The
// caller must hold p.mu; the callback must not call back into the player.
func (p *Player) saveNowLocked() {
	if p.save != nil {
		p.save(p.opt)
	}
}

// RemoteKbps is the configured transcode quality (remote_kbps).
func (p *Player) RemoteKbps() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.opt.RemoteKbps > 0 {
		return p.opt.RemoteKbps
	}
	return 25000
}

// ApplySubtitleStyle pushes the subtitle size/colour/position to mpv. Port of
// upstream player.update_subtitle_visuals. Caller must not hold p.mu.
func (p *Player) ApplySubtitleStyle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.applySubtitleStyleLocked()
}

// webSeekLengths returns the remote's skip lengths in seconds (negative for
// back), or ok=false when the server did not tell us.
func (p *Player) webSeekLengths() (back, fwd float64, ok bool) {
	p.mu.Lock()
	media := p.media
	defBack, defFwd := p.opt.SeekLeft, p.opt.SeekRight
	p.mu.Unlock()
	if media == nil || media.Video == nil || media.Video.MediaSource == nil {
		return defBack, defFwd, false
	}
	prefs := media.Video.MediaSource.CustomPrefs
	get := func(keys ...string) (float64, bool) {
		for _, k := range keys {
			if v, ok := prefs[k]; ok {
				switch n := v.(type) {
				case float64:
					return n, true
				case int:
					return float64(n), true
				case string:
					var f float64
					if _, err := fmt.Sscanf(n, "%g", &f); err == nil {
						return f, true
					}
				}
			}
		}
		return 0, false
	}
	b, okB := get("SkipBackLength", "skipBackLength")
	f, okF := get("SkipForwardLength", "skipForwardLength")
	if okB && okF {
		return -b / 1000, f / 1000, true
	}
	return defBack, defFwd, false
}

// subPos maps our setting to mpv's sub-pos. mpv counts *upwards from the
// bottom*: 100 is the default (bottom) and larger values push the subtitle
// further down, so "top" is the small number. Same table as upstream's
// SUBTITLE_POS.
func subPos(pos string) string {
	switch pos {
	case "top":
		return "0"
	case "middle":
		return "80"
	case "bottom", "":
		return "100"
	}
	return "100"
}

// runShell runs a lifecycle hook detached; failures are logged, never fatal.
func (p *Player) runShell(name, cmd string) {
	if strings.TrimSpace(cmd) == "" {
		return
	}
	c := exec.Command("/bin/sh", "-c", cmd)
	if err := c.Start(); err != nil {
		p.log.Printf("%s: %v", name, err)
	}
	go func() { _ = c.Wait() }()
}

// openURL opens a link with the desktop's handler (update notifications).
func openURL(url string) error {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd = "open"
	case "windows":
		cmd, args = "rundll32", []string{"url.dll,FileProtocolHandler"}
	default:
		cmd = "xdg-open"
	}
	return exec.Command(cmd, append(args, url)...).Start()
}

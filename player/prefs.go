package player

// The OSD preference menus: Video Preferences and Player Preferences, ported
// from upstream menu.py. Unlike the track pickers these change *settings*, so
// each entry writes to the player's options, applies the change to the running
// playback where relevant, and asks the save callback to persist config.json.

import (
	"fmt"
	"strings"
	"time"
)

// qualityLevels are the transcode quality presets, in kbps (upstream
// TRANSCODE_LEVELS): used for both the remote and the local bitrate.
var qualityLevels = []struct {
	label string
	kbps  int
}{
	{"1080p 20 Mbps", 20000},
	{"1080p 12 Mbps", 12000},
	{"1080p 10 Mbps", 10000},
	{"720p 4 Mbps", 4000},
	{"720p 3 Mbps", 3000},
	{"720p 2.5 Mbps", 2500},
	{"540p 1.5 Mbps", 1500},
	{"540p 0.9 Mbps", 950},
	{"480p 0.4 Mbps", 400},
	{"320p 0.3 Mbps", 320},
}

// seekSteps are the arrow-key seek steps, in seconds (upstream seek_*).
var seekSteps = []struct {
	label string
	secs  float64
}{
	{"5 s", 5}, {"10 s", 10}, {"15 s", 15}, {"30 s", 30}, {"60 s", 60}, {"120 s", 120},
}

// idleStops are the "stop when idle" delays.
var idleStops = []struct {
	label string
	after time.Duration
}{
	{"off", 0},
	{"15 minutes", 15 * time.Minute},
	{"1 hour", time.Hour},
	{"3 hours", 3 * time.Hour},
	{"6 hours", 6 * time.Hour},
	{"24 hours", 24 * time.Hour},
}

// logLevels are the mpv msg-levels we offer.
var logLevels = []struct {
	label string
	level string
}{
	{"quiet", "quiet"}, {"error", "error"}, {"warn", "warn"},
	{"info", "info"}, {"debug", "debug"},
}

var subtitleSizes = []struct {
	label string
	pct   int
}{
	{"Tiny", 50}, {"Small", 75}, {"Normal", 100}, {"Large", 125}, {"Huge", 200},
}

var subtitleColors = []struct {
	label string
	hex   string
}{
	{"White", "#FFFFFFFF"},
	{"Yellow", "#FFFFEE00"},
	{"Black", "#FF000000"},
	{"Cyan", "#FF00FFFF"},
	{"Blue", "#FF0000FF"},
	{"Green", "#FF00FF00"},
	{"Magenta", "#FFEE00EE"},
	{"Red", "#FFFF0000"},
	{"Gray", "#FF808080"},
}

// openVideoPrefs opens the Video Preferences page from wherever we are.
func (m *menu) openVideoPrefs() {
	m.pushPrefs(videoPrefsTitle, m.videoPrefsEntries())
}

// videoPrefsEntries builds the Video Preferences rows.
func (m *menu) videoPrefsEntries() []menuEntry {
	o := m.p.Options()
	var entries []menuEntry
	entries = append(entries,
		menuEntry{fmt.Sprintf("Remote Transcode Quality: %.1f Mbps", float64(m.p.RemoteKbps())/1000), m.openRemoteQuality},
		menuEntry{fmt.Sprintf("Local Transcode Quality: %.1f Mbps", float64(o.LocalKbps)/1000), m.openLocalQuality},
		menuEntry{fmt.Sprintf("Subtitle Size: %d", o.SubSize), m.openSubtitleSize},
		menuEntry{"Subtitle Position: " + o.SubPosition, m.openSubtitlePosition},
		menuEntry{"Subtitle Color: " + o.SubColor, m.openSubtitleColor},
		m.toggle("Transcode Hi10p to 8bit", o.TranscodeHi10p, m.setTranscodeHi10p),
		m.toggle("Transcode HDR", o.TranscodeHDR, m.setTranscodeHDR),
		m.toggle("Transcode Dolby Vision", o.TranscodeDolbyVision, m.setTranscodeDV),
		m.toggle("Direct Paths", o.DirectPaths, m.setDirectPaths),
		m.toggle("Disable Direct Play", o.AlwaysTranscode, m.setAlwaysTranscode),
		m.toggle("Allow HEVC When Transcoding", o.TranscodeH265, m.setTranscodeH265),
		m.toggle("Force H.264 When Transcoding", o.ForceH264, m.setForceH264),
	)
	return entries
}

// openPlayerPrefs opens the Player Preferences page.
func (m *menu) openPlayerPrefs() {
	m.pushPrefs(playerPrefsTitle, m.playerPrefsEntries())
}

// playerPrefsEntries builds the Player Preferences rows.
func (m *menu) playerPrefsEntries() []menuEntry {
	o := m.p.Options()
	entries := []menuEntry{
		m.toggle("Auto Play", o.AutoPlay, m.setAutoPlay),
		m.toggle("Auto Fullscreen", o.Fullscreen, m.setFullscreenStart),
		m.toggle("Media Key Seek", o.MediaKeySeek, m.setMediaKeySeek),
		m.toggle("Enable OSC", o.EnableOSC, m.setEnableOSC),
		m.toggle("Use Web Seek Pref", o.UseWebSeek, m.setUseWebSeek),
		m.toggle("Write Logs to File", o.WriteLogs, m.setWriteLogs),
		m.toggle("Check for Updates", o.CheckUpdates, m.setCheckUpdates),
		m.toggle("Always Skip Intros", o.SkipIntroAlways, m.setSkipIntroAlways),
		m.toggle("Ask to Skip Intros", o.SkipIntro, m.setSkipIntroAsk),
		m.toggle("Always Skip Credits", o.SkipCreditsAlways, m.setSkipCreditsAlways),
		m.toggle("Ask to Skip Credits", o.SkipCredits, m.setSkipCreditsAsk),
		m.toggle("Mouse Menu", o.MenuMouse, m.setMenuMouse),
		m.toggle("Remember Volume", o.RememberVolume, m.setRememberVolume),
		menuEntry{"Seek Steps: " + seekStepLabel(o), m.openSeekSteps},
		menuEntry{idleStopLabel(o), m.openIdleStop},
		menuEntry{"Log Level: " + o.LogLevel, m.openLogLevel},
		m.toggle("Redact Tokens In Log", o.SanitizeOutput, m.setSanitizeOutput),
	}
	return entries
}

// toggle renders a checkbox row.
func (m *menu) toggle(label string, on bool, set func(bool)) menuEntry {
	mark := "  "
	if on {
		mark = "✔ "
	}
	return menuEntry{mark + label, func() { set(!on) }}
}

// setters mutate options, apply, persist and re-render the parent menu.

func (m *menu) setBool(set func(*Options, bool), apply func()) func(bool) {
	return func(v bool) {
		m.p.mu.Lock()
		set(&m.p.opt, v)
		m.p.saveNowLocked()
		m.p.mu.Unlock()
		if apply != nil {
			apply()
		}
		m.backToRoot() // upstream re-renders the parent menu
	}
}

// pushPrefs opens a preferences page from its parent.
func (m *menu) pushPrefs(title string, entries []menuEntry) {
	m.push(title, entries, 0)
}

// selectRow moves the highlight, so a submenu opens on the current value.
func (m *menu) selectRow(i int) {
	m.mu.Lock()
	if i >= 0 && i < len(m.frame.entries) {
		m.frame.selected = i
	}
	m.mu.Unlock()
	m.refresh()
}

// prefsEntries builds the rows of the preferences page we are in.
func (m *menu) prefsEntries() (string, []menuEntry) {
	m.mu.Lock()
	title := m.frame.title
	m.mu.Unlock()
	switch title {
	case playerPrefsTitle:
		return title, m.playerPrefsEntries()
	default:
		return videoPrefsTitle, m.videoPrefsEntries()
	}
}

// backToRoot returns to the preferences page after a change: pop out of
// whatever submenu we are in, then *replace* the preferences page in place (not
// re-push it), so the parent link stays intact and "back" keeps walking up the
// tree: submenu → preferences → root.
func (m *menu) backToRoot() {
	for i := 0; i < 8; i++ {
		m.mu.Lock()
		title, depth, shown := m.frame.title, len(m.stacks), m.shown
		m.mu.Unlock()
		if !shown {
			return
		}
		if title == videoPrefsTitle || title == playerPrefsTitle {
			page, entries := m.prefsEntries()
			m.replaceFrame(page, entries)
			return
		}
		if depth == 0 {
			return
		}
		m.Action("back")
	}
}

const (
	videoPrefsTitle  = "Video Preferences"
	playerPrefsTitle = "Player Preferences"
)

// openRemoteQuality/openLocalQuality pick the bitrate we ask the server for on
// remote and local items respectively (upstream remote_kbps/local_kbps).
func (m *menu) openRemoteQuality() {
	cur := m.p.RemoteKbps()
	title := "Remote Transcode Quality"
	entries := m.qualityEntries(func(kbps int) { m.p.opt.RemoteKbps = kbps })
	m.pushPrefs(title, entries)
	m.selectRow(indexOfQuality(cur))
}

func (m *menu) openLocalQuality() {
	cur := m.p.Options().LocalKbps
	entries := m.qualityEntries(func(kbps int) { m.p.opt.LocalKbps = kbps })
	m.pushPrefs("Local Transcode Quality", entries)
	m.selectRow(indexOfQuality(cur))
}

// qualityEntries builds the preset list, with "No Transcode" on top.
func (m *menu) qualityEntries(set func(int)) []menuEntry {
	entries := []menuEntry{{"No Transcode (2 Gbps)", m.pickQuality(set, 2147483)}}
	for _, lvl := range qualityLevels {
		lvl := lvl
		entries = append(entries, menuEntry{lvl.label, m.pickQuality(set, lvl.kbps)})
	}
	return entries
}

// indexOfQuality is the row for a kbps value ("No Transcode" is row 0).
func indexOfQuality(kbps int) int {
	for i, lvl := range qualityLevels {
		if lvl.kbps == kbps {
			return i + 1
		}
	}
	return 0
}

// pickQuality applies a preset: mutate, persist, re-render the parent page.
func (m *menu) pickQuality(set func(int), kbps int) func() {
	return func() {
		m.p.mu.Lock()
		set(kbps)
		m.p.saveNowLocked()
		m.p.mu.Unlock()
		m.backToRoot()
	}
}

func (m *menu) openSubtitleSize() {
	entries := make([]menuEntry, 0, len(subtitleSizes))
	sel := 0
	cur := m.p.Options().SubSize
	for i, s := range subtitleSizes {
		s := s
		entries = append(entries, menuEntry{s.label, m.pickSubtitle(func(o *Options) {
			o.SubSize = s.pct
		})})
		if cur == s.pct {
			sel = i
		}
	}
	m.push("Select Subtitle Size", entries, sel)
}

func (m *menu) openSubtitlePosition() {
	positions := []string{"bottom", "top", "middle"}
	labels := map[string]string{"bottom": "Bottom", "top": "Top", "middle": "Middle"}
	cur := m.p.Options().SubPosition
	entries := make([]menuEntry, 0, len(positions))
	sel := 0
	for i, pos := range positions {
		pos := pos
		entries = append(entries, menuEntry{labels[pos], m.pickSubtitle(func(o *Options) {
			o.SubPosition = pos
		})})
		if cur == pos {
			sel = i
		}
	}
	m.push("Select Subtitle Position", entries, sel)
}

func (m *menu) openSubtitleColor() {
	cur := strings.ToUpper(m.p.Options().SubColor)
	entries := make([]menuEntry, 0, len(subtitleColors))
	sel := 0
	for i, c := range subtitleColors {
		c := c
		entries = append(entries, menuEntry{c.label, m.pickSubtitle(func(o *Options) {
			o.SubColor = c.hex
		})})
		if strings.EqualFold(c.hex, cur) {
			sel = i
		}
	}
	m.push("Select Subtitle Color", entries, sel)
}

// pickSubtitle applies a subtitle change immediately (so the user sees it
// while the menu is still open) and persists it.
func (m *menu) pickSubtitle(mutate func(*Options)) func() {
	return func() {
		m.p.mu.Lock()
		mutate(&m.p.opt)
		m.p.saveNowLocked()
		m.p.mu.Unlock()
		m.p.ApplySubtitleStyle()
		m.backToRoot()
	}
}

// Concrete setters (kept small so the rows above read like upstream's menu).

func (m *menu) setAutoPlay(v bool) {
	m.setBool(func(o *Options, val bool) { o.AutoPlay = val }, nil)(v)
}
func (m *menu) setFullscreenStart(v bool) {
	m.setBool(func(o *Options, val bool) { o.Fullscreen = val }, nil)(v)
}
func (m *menu) setMediaKeySeek(v bool) {
	m.setBool(func(o *Options, val bool) { o.MediaKeySeek = val }, nil)(v)
}
func (m *menu) setUseWebSeek(v bool) {
	m.setBool(func(o *Options, val bool) { o.UseWebSeek = val }, nil)(v)
}
func (m *menu) setWriteLogs(v bool) {
	m.setBool(func(o *Options, val bool) { o.WriteLogs = val }, nil)(v)
}
func (m *menu) setCheckUpdates(v bool) {
	m.setBool(func(o *Options, val bool) { o.CheckUpdates = val }, nil)(v)
}
func (m *menu) setSkipIntroAlways(v bool) {
	m.setBool(func(o *Options, val bool) { o.SkipIntroAlways = val }, nil)(v)
}
func (m *menu) setSkipIntroAsk(v bool) {
	m.setBool(func(o *Options, val bool) { o.SkipIntro = val }, nil)(v)
}
func (m *menu) setSkipCreditsAlways(v bool) {
	m.setBool(func(o *Options, val bool) { o.SkipCreditsAlways = val }, nil)(v)
}
func (m *menu) setSkipCreditsAsk(v bool) {
	m.setBool(func(o *Options, val bool) { o.SkipCredits = val }, nil)(v)
}
func (m *menu) setTranscodeHi10p(v bool) {
	m.setBool(func(o *Options, val bool) { o.TranscodeHi10p = val }, nil)(v)
}
func (m *menu) setTranscodeHDR(v bool) {
	m.setBool(func(o *Options, val bool) { o.TranscodeHDR = val }, nil)(v)
}
func (m *menu) setTranscodeDV(v bool) {
	m.setBool(func(o *Options, val bool) { o.TranscodeDolbyVision = val }, nil)(v)
}
func (m *menu) setDirectPaths(v bool) {
	m.setBool(func(o *Options, val bool) { o.DirectPaths = val }, nil)(v)
}
func (m *menu) setMenuMouse(v bool) {
	m.setBool(func(o *Options, val bool) { o.MenuMouse = val }, nil)(v)
}

func (m *menu) setRememberVolume(v bool) {
	m.setBool(func(o *Options, val bool) { o.RememberVolume = val }, nil)(v)
}

// setEnableOSC also applies the change to the running player.
func (m *menu) setEnableOSC(v bool) {
	m.setBool(func(o *Options, val bool) { o.EnableOSC = val }, func() {
		if m.shown {
			return // the menu's own show/hide manages the OSC
		}
		m.p.mpv.SetProperty("osc", v)
	})(v)
}

// --- seek steps, idle stop, logging (Player Preferences) -------------------

// seekStepLabel describes the arrow-key steps in one line.
func seekStepLabel(o Options) string {
	return fmt.Sprintf("%.0f s / %.0f s", abs(o.SeekLeft), abs(o.SeekDown))
}

func (m *menu) openSeekSteps() {
	m.push("Seek Steps", []menuEntry{
		{"← / →  (left / right)", m.openSeekStepsHorizontal},
		{"↑ / ↓  (up / down)", m.openSeekStepsVertical},
	}, 0)
}

func (m *menu) seekStepEntries(apply func(secs float64)) []menuEntry {
	entries := make([]menuEntry, 0, len(seekSteps))
	for _, st := range seekSteps {
		st := st
		entries = append(entries, menuEntry{st.label, func() {
			m.p.mu.Lock()
			apply(st.secs)
			m.p.saveNowLocked()
			m.p.mu.Unlock()
			m.backToRoot()
		}})
	}
	return entries
}

func (m *menu) openSeekStepsHorizontal() {
	cur := abs(m.p.Options().SeekLeft)
	entries := m.seekStepEntries(func(secs float64) {
		m.p.opt.SeekLeft, m.p.opt.SeekRight = -secs, secs
	})
	m.push("Left / Right Seek Step", entries, indexOfStep(cur))
}

func (m *menu) openSeekStepsVertical() {
	cur := abs(m.p.Options().SeekDown)
	entries := m.seekStepEntries(func(secs float64) {
		m.p.opt.SeekUp, m.p.opt.SeekDown = secs, -secs
	})
	m.push("Up / Down Seek Step", entries, indexOfStep(cur))
}

// indexOfStep finds the row matching the current step so the menu opens on it.
func indexOfStep(secs float64) int {
	for i, st := range seekSteps {
		if st.secs == secs {
			return i
		}
	}
	return 0
}

// idleStopLabel describes the current idle-stop choice, including "off".
func idleStopLabel(o Options) string {
	if !o.IdleStop || o.IdleStopAfter <= 0 {
		return "Stop When Idle: off"
	}
	return fmt.Sprintf("Stop When Idle: %s", shortDuration(o.IdleStopAfter))
}

// shortDuration renders a duration compactly ("15m", "6h", "24h").
func shortDuration(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dh", int(d/(24*time.Hour)))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	default:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
}

func (m *menu) openIdleStop() {
	o := m.p.Options()
	entries := make([]menuEntry, 0, len(idleStops))
	sel := 0
	for i, is := range idleStops {
		is := is
		entries = append(entries, menuEntry{is.label, func() {
			m.p.mu.Lock()
			m.p.opt.IdleStop = is.after > 0
			m.p.opt.IdleStopAfter = is.after
			m.p.saveNowLocked()
			m.p.mu.Unlock()
			m.backToRoot()
		}})
		if o.IdleStop == (is.after > 0) && (is.after == 0 || o.IdleStopAfter == is.after) {
			sel = i
		}
	}
	m.push("Stop When Idle After", entries, sel)
}

func (m *menu) openLogLevel() {
	o := m.p.Options()
	entries := make([]menuEntry, 0, len(logLevels))
	sel := 0
	for i, l := range logLevels {
		l := l
		entries = append(entries, menuEntry{l.label, func() {
			m.p.mu.Lock()
			m.p.opt.LogLevel = l.level
			m.p.saveNowLocked()
			m.p.mu.Unlock()
			m.backToRoot()
		}})
		if o.LogLevel == l.level {
			sel = i
		}
	}
	m.push("Log Level", entries, sel)
}

func (m *menu) setSanitizeOutput(v bool) {
	m.setBool(func(o *Options, val bool) { o.SanitizeOutput = val }, nil)(v)
}

func (m *menu) setAlwaysTranscode(v bool) {
	m.setBool(func(o *Options, val bool) { o.AlwaysTranscode = val }, nil)(v)
}

func (m *menu) setTranscodeH265(v bool) {
	m.setBool(func(o *Options, val bool) { o.TranscodeH265 = val }, nil)(v)
}

func (m *menu) setForceH264(v bool) {
	m.setBool(func(o *Options, val bool) { o.ForceH264 = val }, nil)(v)
}

package player

// The OSD preference menus: Video Preferences and Player Preferences, ported
// from upstream menu.py. Unlike the track pickers these change *settings*, so
// each entry writes to the player's options, applies the change to the running
// playback where relevant, and asks the save callback to persist config.json.

import (
	"fmt"
	"strings"
)

// transcodeLevels are the remote-transcode quality presets (upstream
// TRANSCODE_LEVELS), in kbps.
var transcodeLevels = []struct {
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
		menuEntry{fmt.Sprintf("Remote Transcode Quality: %.1f Mbps", float64(m.p.RemoteKbps())/1000), m.openTranscodeQuality},
		menuEntry{fmt.Sprintf("Subtitle Size: %d", o.SubSize), m.openSubtitleSize},
		menuEntry{"Subtitle Position: " + o.SubPosition, m.openSubtitlePosition},
		menuEntry{"Subtitle Color: " + o.SubColor, m.openSubtitleColor},
		m.toggle("Transcode Hi10p to 8bit", o.TranscodeHi10p, m.setTranscodeHi10p),
		m.toggle("Transcode HDR", o.TranscodeHDR, m.setTranscodeHDR),
		m.toggle("Transcode Dolby Vision", o.TranscodeDolbyVision, m.setTranscodeDV),
		m.toggle("Direct Paths", o.DirectPaths, m.setDirectPaths),
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

// pushPrefs opens a preferences page from its parent, remembering which one so
// a change can re-render it in place (upstream re-renders the preferences menu
// after a change).
func (m *menu) pushPrefs(title string, entries []menuEntry) {
	m.mu.Lock()
	m.prefsTitle = title
	m.mu.Unlock()
	m.push(title, entries, 0)
}

// prefsEntries builds the rows of the preferences page we are in.
func (m *menu) prefsEntries() (string, []menuEntry) {
	m.mu.Lock()
	title := m.prefsTitle
	m.mu.Unlock()
	switch title {
	case playerPrefsTitle:
		return title, m.playerPrefsEntries()
	default:
		return videoPrefsTitle, m.videoPrefsEntries()
	}
}

// backToRoot returns to the preferences page after a change. If we are in one of
// its submenus we pop that single level first; then the page is *replaced* (not
// re-pushed), so the parent link stays intact and "back" keeps walking up the
// tree: submenu → preferences → root.
func (m *menu) backToRoot() {
	m.mu.Lock()
	title, onPrefs := m.prefsTitle, m.shown && m.frame.title == m.prefsTitle
	m.mu.Unlock()
	if title == "" {
		return
	}
	if !onPrefs {
		m.Action("back") // leave the submenu we were in
	}
	page, entries := m.prefsEntries()
	m.replaceFrame(page, entries)
}

const (
	videoPrefsTitle  = "Video Preferences"
	playerPrefsTitle = "Player Preferences"
)

func (m *menu) openTranscodeQuality() {
	o := m.p.Options()
	entries := []menuEntry{{"No Transcode (2 Gbps)", m.pickQuality(2147483)}}
	sel := 0
	for i, lvl := range transcodeLevels {
		lvl := lvl
		entries = append(entries, menuEntry{lvl.label, m.pickQuality(lvl.kbps)})
		if o.RemoteKbps == lvl.kbps {
			sel = i + 1
		}
	}
	m.push("Select Default Transcode Profile", entries, sel)
}

func (m *menu) pickQuality(kbps int) func() {
	return func() {
		m.p.mu.Lock()
		m.p.opt.RemoteKbps = kbps
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

// setEnableOSC also applies the change to the running player.
func (m *menu) setEnableOSC(v bool) {
	m.setBool(func(o *Options, val bool) { o.EnableOSC = val }, func() {
		if m.shown {
			return // the menu's own show/hide manages the OSC
		}
		m.p.mpv.SetProperty("osc", v)
	})(v)
}

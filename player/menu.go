package player

// The in-player OSD menu. Port of upstream menu.py (OSDMenu), reduced to the
// parts in scope (PLAN R6): root menu + audio/subtitle track pickers, rendered
// as multi-line OSD text over a filled background box, driven by keyboard
// (mpv keybindings -> script-message) and by the remote's navigation
// GeneralCommands.

import (
	"fmt"
	"sync"

	"mpv-shim/jfin"
)

type menuEntry struct {
	label  string
	action func() // nil = just a row
}

type menuFrame struct {
	title    string
	entries  []menuEntry
	selected int
}

type menu struct {
	p      *Player
	mu     sync.Mutex // guards the menu state only; never held across p.mu
	shown  bool
	stacks []menuFrame
	frame  menuFrame
	// saved OSD properties, restored on hide
	savedColor       string
	savedFontSize    int
	savedBorderStyle string
}

func newMenu(p *Player) *menu {
	return &menu{p: p, savedColor: "#C8000000", savedFontSize: 55, savedBorderStyle: "outline-and-shadow"}
}

// Shown reports whether the menu is open (own lock: callers may hold p.mu).
func (m *menu) Shown() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.shown
}

// Show opens the menu at the root, pauses playback, and renders. Port of
// upstream show_menu (minus profiles/SVP/syncplay/trickplay).
func (m *menu) Show() {
	playing := m.p.HasVideo() // before m.mu: lock order is m.mu -> p.mu, never the reverse
	m.mu.Lock()
	if m.shown {
		m.mu.Unlock()
		return
	}
	m.shown = true
	m.stacks = nil
	m.frame = menuFrame{title: "Main Menu", entries: m.rootEntries(playing)}
	m.saveOSDLocked()
	m.mu.Unlock()

	m.p.mpv.SetProperty("osd-back-color", "#CC333333")
	m.p.mpv.SetProperty("osd-font-size", 40)
	// Required for osd-back-color to render as a filled box on mpv 0.36+.
	m.p.mpv.SetProperty("osd-border-style", "background-box")
	m.p.mpv.SetProperty("osc", false)
	m.refresh()
	m.p.SetPaused(true)
}

func (m *menu) rootEntries(playing bool) []menuEntry {
	var e []menuEntry
	if playing {
		e = append(e,
			menuEntry{"Change Audio", func() { m.openAudio() }},
			menuEntry{"Change Subtitles", func() { m.openSubtitle() }},
			menuEntry{"Quit and Mark Unwatched", m.unwatchedQuit},
		)
	}
	return append(e, menuEntry{"Close Menu", m.Hide})
}

// Hide closes the menu, restores the OSD and unpauses. Port of hide_menu.
func (m *menu) Hide() {
	m.mu.Lock()
	if !m.shown {
		m.mu.Unlock()
		return
	}
	m.shown = false
	m.frame = menuFrame{}
	m.stacks = nil
	color, size, border := m.savedColor, m.savedFontSize, m.savedBorderStyle
	m.mu.Unlock()

	m.p.mpv.ShowText("", 0, 0)
	m.p.mpv.SetProperty("osd-back-color", color)
	m.p.mpv.SetProperty("osd-font-size", size)
	m.p.mpv.SetProperty("osd-border-style", border)
	m.p.mpv.SetProperty("osc", true)
	m.p.SetPaused(false)
}

func (m *menu) saveOSDLocked() {
	if v, err := m.p.mpv.GetProperty("osd-back-color"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			m.savedColor = s
		}
	}
	if v, err := m.p.mpv.GetProperty("osd-font-size"); err == nil {
		if f, ok := v.(float64); ok && f > 0 {
			m.savedFontSize = int(f)
		}
	}
	if v, err := m.p.mpv.GetProperty("osd-border-style"); err == nil {
		if s, ok := v.(string); ok && s != "" {
			m.savedBorderStyle = s
		}
	}
}

// Action applies a navigation action. Port of upstream menu_action.
func (m *menu) Action(action string) {
	m.mu.Lock()
	if !m.shown {
		if action == "home" || action == "ok" {
			m.mu.Unlock()
			m.Show()
			return
		}
		m.mu.Unlock()
		return
	}
	switch action {
	case "up":
		m.frame.selected = (m.frame.selected - 1 + len(m.frame.entries)) % len(m.frame.entries)
	case "down":
		m.frame.selected = (m.frame.selected + 1) % len(m.frame.entries)
	case "back":
		if n := len(m.stacks); n > 0 {
			m.frame = m.stacks[n-1]
			m.stacks = m.stacks[:n-1]
		} else {
			m.mu.Unlock()
			m.Hide()
			return
		}
	case "home":
		m.mu.Unlock()
		entries := m.rootEntries(m.p.HasVideo())
		m.mu.Lock()
		m.stacks = nil
		m.frame = menuFrame{title: "Main Menu", entries: entries}
	case "ok":
		sel := m.frame.selected
		if sel < len(m.frame.entries) && m.frame.entries[sel].action != nil {
			fn := m.frame.entries[sel].action
			m.mu.Unlock()
			fn() // actions re-enter the menu (openAudio/Hide) or the player
			return
		}
	}
	m.mu.Unlock()
	m.refresh()
}

// push swaps in a submenu, remembering the current frame (upstream put_menu).
func (m *menu) push(title string, entries []menuEntry, selected int) {
	m.mu.Lock()
	m.stacks = append(m.stacks, m.frame)
	m.frame = menuFrame{title: title, entries: entries, selected: selected}
	m.mu.Unlock()
	m.refresh()
}

// streamLabel formats a track like upstream: DisplayTitle plus the raw title,
// falling back to the language.
func streamLabel(s jfin.MediaStream) string {
	name := s.Title
	if name == "" {
		name = s.Language
	}
	if name == "" {
		return fmt.Sprintf("%s %d", s.Type, s.Index)
	}
	if s.IsExternal {
		return fmt.Sprintf("%s (external)", name)
	}
	return name
}

func (m *menu) openAudio() {
	m.p.mu.Lock()
	v := m.p.media.Video
	m.p.mu.Unlock()
	if v == nil {
		return
	}
	var entries []menuEntry
	sel := 0
	i := 0
	for _, s := range v.MediaSource.MediaStreams {
		if s.Type != "Audio" {
			continue
		}
		idx := s.Index
		entries = append(entries, menuEntry{streamLabel(s), func() { m.p.SetStreams(&idx, nil); m.Action("back") }})
		if v.Aid != nil && *v.Aid == idx {
			sel = i
		}
		i++
	}
	m.push("Select Audio Track", entries, sel)
}

func (m *menu) openSubtitle() {
	m.p.mu.Lock()
	v := m.p.media.Video
	m.p.mu.Unlock()
	if v == nil {
		return
	}
	off := -1
	entries := []menuEntry{{"None", func() { m.p.SetStreams(nil, &off); m.Action("back") }}}
	sel := 0
	i := 1
	for _, s := range v.MediaSource.MediaStreams {
		if s.Type != "Subtitle" {
			continue
		}
		idx := s.Index
		entries = append(entries, menuEntry{streamLabel(s), func() { m.p.SetStreams(nil, &idx); m.Action("back") }})
		if v.Sid != nil && *v.Sid == idx {
			sel = i
		}
		i++
	}
	m.push("Select Subtitle Track", entries, sel)
}

// unwatchedQuit stops playback and marks the item unwatched (upstream
// unwatched_quit).
func (m *menu) unwatchedQuit() {
	m.p.mu.Lock()
	var v *jfin.Video
	if m.p.media != nil {
		v = m.p.media.Video
	}
	ctx := m.p.ctx
	m.p.mu.Unlock()
	m.Hide()
	m.p.Stop()
	if v != nil {
		if err := v.M.C.SetPlayed(ctx, v.ID, false); err != nil {
			m.p.log.Printf("set unwatched: %v", err)
		}
	}
}

// refresh redraws the menu as multi-line OSD text (upstream refresh_menu).
func (m *menu) refresh() {
	m.mu.Lock()
	if !m.shown || len(m.frame.entries) == 0 {
		m.mu.Unlock()
		return
	}
	text := m.frame.title
	for i, e := range m.frame.entries {
		if i == m.frame.selected {
			text += fmt.Sprintf("\n   **%s**", e.label)
		} else {
			text += fmt.Sprintf("\n   %s", e.label)
		}
	}
	m.mu.Unlock()
	// 2^30 ms: sticky until the next refresh/hide.
	m.p.mpv.ShowText(text, 1<<30, 1)
}

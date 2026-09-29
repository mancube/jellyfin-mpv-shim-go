package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mpv-shim/jfin"
	"mpv-shim/player"
)

// Session is what the status screen shows. main passes the live objects; the
// model only reads them.
type Session struct {
	Account jfin.Account
	WS      *jfin.WS
	Player  *player.Player
	Logs    *LogRing
	// Quit stops the whole app (the tray uses it too).
	Quit func()
}

type statusModel struct {
	s       *Session
	deps    Deps
	setup   *setupModel
	inSetup bool
}

func newStatusModel(s *Session, d Deps) statusModel {
	return statusModel{s: s, deps: d}
}

func (m statusModel) Init() tea.Cmd { return tick() }

func (m statusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.s.Quit != nil {
				m.s.Quit()
			}
			return m, tea.Quit
		case "a", "r", "i":
			m.inSetup = true
			sm := newSetupModel(m.deps)
			m.setup = &sm
			return m, m.setup.Init()
		}
	case tickMsg:
		return m, tick()
	}
	return m, nil
}

func (m statusModel) View() string {
	if m.inSetup {
		return m.setup.View()
	}
	st := m.s.Player.Status()
	conn := "disconnected"
	if m.s.WS.Connected() {
		conn = "connected"
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("mpv-shim — %s  (%s)\n", m.s.Account.Server, m.s.Account.Username))
	b.WriteString(fmt.Sprintf("server: %s\n\n", conn))
	if st.Playing {
		state := "playing"
		if st.Paused {
			state = "paused"
		}
		b.WriteString(fmt.Sprintf("now playing: %s\n", st.Title))
		b.WriteString(fmt.Sprintf("  %s / %s  (%s)  vol %.0f%%%s\n\n",
			fmtTime(st.Position), fmtTime(st.Duration), state, st.Volume, muteMark(st.Mute)))
	} else {
		b.WriteString("now playing: —\n\n")
	}
	b.WriteString("log\n")
	for _, l := range m.s.Logs.Tail(12) {
		b.WriteString("  " + l + "\n")
	}
	b.WriteString("\naccounts: a add   i quick connect   r remove   q quit\n")
	return b.String()
}

func muteMark(mute bool) string {
	if mute {
		return "  [muted]"
	}
	return ""
}

func fmtTime(sec float64) string {
	if sec <= 0 {
		return "--:--"
	}
	d := time.Duration(sec) * time.Second
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// RunSetup runs the account wizard and returns when the user quits it.
func RunSetup(d Deps) error {
	_, err := tea.NewProgram(newSetupModel(d)).Run()
	return err
}

// RunStatus runs the live status screen (blocking).
func RunStatus(s *Session, d Deps) error {
	_, err := tea.NewProgram(newStatusModel(s, d)).Run()
	return err
}

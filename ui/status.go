package ui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

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
		case "a", "r", "i", "p":
			sm := newSetupModel(m.deps)
			m.setup = &sm
			m.inSetup = true
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
	return strings.Join([]string{
		m.headerView(),
		m.nowPlayingView(),
		m.logView(),
		"",
		hints([2]string{"a", "accounts"}, [2]string{"p", "add account"}, [2]string{"q", "quit"}),
		"",
	}, "\n")
}

func (m statusModel) headerView() string {
	conn := styBad.Render("● offline")
	if m.s.WS.Connected() {
		conn = styOK.Render("● online")
	}
	line := conn + styDim.Render("   "+clip(m.s.Account.Server, 44)) +
		styDim.Render("  ·  ") + clip(m.s.Account.Username, 20)
	right := styDim.Render(clip(m.s.Account.DeviceID, 8))
	gap := 58 - lipgloss.Width(line) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return panel("mpv-shim", line+strings.Repeat(" ", gap)+right)
}

func (m statusModel) nowPlayingView() string { return nowPlaying(m.s.Player.Status()) }

// nowPlaying renders the playback panel for a status snapshot.
func nowPlaying(st player.Status) string {
	if !st.Playing && st.Title == "" {
		return panelW("now playing", 60, styDim.Render("nothing playing — cast something from the web UI"))
	}
	state := styOK.Render("▶ playing")
	if st.Paused {
		state = styWarn.Render("❚❚ paused")
	}
	vol := fmt.Sprintf("vol %.0f%%", st.Volume)
	if st.Mute {
		vol += "  " + styBad.Render("muted")
	}
	title := styText.Render(clip(st.Title, 44))
	volStyle := styDim.Render(vol)
	gap := 58 - lipgloss.Width(title) - lipgloss.Width(volStyle)
	if gap < 1 {
		gap = 1
	}
	return panelW("now playing", 60,
		title+strings.Repeat(" ", gap)+volStyle,
		"",
		state+"  "+bar(frac(st.Position, st.Duration), 26),
		styDim.Render(fmtTime(st.Position)+" / "+fmtTime(st.Duration)),
	)
}

func (m statusModel) logView() string {
	lines := m.s.Logs.Tail(10)
	if len(lines) == 0 {
		lines = []string{""}
	}
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		out = append(out, styDim.Render("│ ")+styText.Render(clip(l, 58)))
	}
	return styTitle.Render("log") + "\n" + strings.Join(out, "\n")
}

func frac(pos, dur float64) float64 {
	if dur <= 0 {
		return 0
	}
	return pos / dur
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
	_, err := tea.NewProgram(newSetupModel(d), tea.WithAltScreen()).Run()
	return err
}

// RunStatus runs the live status screen (blocking).
func RunStatus(s *Session, d Deps) error {
	_, err := tea.NewProgram(newStatusModel(s, d), tea.WithAltScreen()).Run()
	return err
}

package ui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"mpv-shim/jfin"
	"mpv-shim/player"
)

// Session is what the status screen and the tray show. main passes the live
// objects; they only read from it.
type Session struct {
	Account jfin.Account
	WS      *jfin.WS
	Player  *player.Player
	Logs    *LogRing
	// ConfigDir and LogPath back the tray's "open folder/log" items.
	ConfigDir string
	LogPath   string
	// OpenOSD opens the in-player OSD menu (the tray's "Player Menu" item).
	OpenOSD func()
	// UpdateNote is a one-line notice (e.g. a new release is available).
	UpdateNote func() string
	// OpenUpdatePage opens that release in the browser.
	OpenUpdatePage func()
	// Quit stops the whole app: the session, mpv and the TUI.
	Quit func()
	// Disconnect drops the connection but keeps playback and the UI up;
	// Reconnect starts the socket loop again. The tray item toggles between
	// the two based on Connected.
	Disconnect func()
	Reconnect  func()
	Connected  func() bool
	// QuitUI asks the TUI program to exit; installed by RunStatus.
	QuitUI func()

	accounts chan struct{} // tray → TUI: open the account wizard
	logf     func(string, ...any)
}

// RequestAccounts asks the TUI to show the account wizard (tray item).
func (s *Session) RequestAccounts() {
	if s.accounts == nil {
		return
	}
	select {
	case s.accounts <- struct{}{}:
	default: // already pending
	}
}

// SetLogf lets the tray log through the app logger.
func (s *Session) SetLogf(f func(string, ...any)) { s.logf = f }

func (s *Session) Logf(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// accountsMsg is delivered when the tray asks for the account wizard.
type accountsMsg struct{}

// NewSession builds a Session with its tray channels set up.
func NewSession(a jfin.Account, ws *jfin.WS, pl *player.Player, logs *LogRing) *Session {
	return &Session{Account: a, WS: ws, Player: pl, Logs: logs, accounts: make(chan struct{}, 1)}
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

func (m statusModel) Init() tea.Cmd { return tea.Batch(tick(), waitAccounts(m.s)) }

// waitAccounts turns a tray "Configure Servers…" click into a message.
func waitAccounts(s *Session) tea.Cmd {
	return func() tea.Msg {
		if s.accounts == nil {
			return nil
		}
		<-s.accounts
		return accountsMsg{}
	}
}

func (m statusModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			if m.s.Quit != nil {
				m.s.Quit()
			}
			return m, tea.Quit // the TUI leaves; main then stops the session
		case "r":
			// r = reconnect when we are offline, otherwise the accounts list
			// (tray parity: the connection item toggles the same way).
			if m.s.Connected != nil && !m.s.Connected() {
				if m.s.Reconnect != nil {
					m.s.Reconnect()
				}
				return m, nil
			}
			return m.openSetup()
		case "a", "i", "p":
			return m.openSetup()
		}
	case accountsMsg:
		return m.openSetup()
	case tickMsg:
		return m, tick()
	}
	return m, nil
}

// openSetup switches to the account wizard (keys or tray) and keeps watching
// for further tray clicks.
func (m statusModel) openSetup() (tea.Model, tea.Cmd) {
	if m.setup == nil {
		sm := newSetupModel(m.deps)
		m.setup = &sm
	}
	m.inSetup = true
	return m, tea.Batch(m.setup.Init(), waitAccounts(m.s))
}

func (m statusModel) View() string {
	if m.inSetup {
		return m.setup.View()
	}
	views := []string{m.headerView()}
	if m.s.Connected != nil && !m.s.Connected() {
		views = append(views, styWarn.Render("  disconnected — press r or use the tray to reconnect"))
	}
	views = append(views, m.nowPlayingView())
	if m.s.UpdateNote != nil {
		if note := m.s.UpdateNote(); note != "" {
			views = append(views, styWarn.Render("▲ "+note))
		}
	}
	views = append(views, m.logView(), "",
		hints([2]string{"a", "accounts"}, [2]string{"p", "add account"},
			[2]string{"r", "reconnect"}, [2]string{"q", "quit"}), "")
	return strings.Join(views, "\n")
}

func (m statusModel) headerView() string {
	conn := styBad.Render("● offline")
	switch m.s.WS.State() {
	case jfin.StateConnected:
		conn = styOK.Render("● online")
	case jfin.StateReconnecting:
		conn = styWarn.Render("● reconnecting")
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
	_, err := tea.NewProgram(newSetupModel(d),
		tea.WithAltScreen(),
		tea.WithInput(os.Stdin), // never fall back to opening /dev/tty
		tea.WithOutput(os.Stdout),
	).Run()
	return err
}

// RunStatus runs the live status screen (blocking). onStart is called once the
// program exists, so callers can wire up the tray there: a tray Quit can then
// always reach QuitUI, even if it is clicked immediately.
func RunStatus(s *Session, d Deps, onStart func()) error {
	prog := tea.NewProgram(newStatusModel(s, d),
		tea.WithAltScreen(),
		tea.WithInput(os.Stdin),   // never fall back to opening /dev/tty: with a
		tea.WithOutput(os.Stdout), // redirected stdin that silently eats keys
	)
	s.QuitUI = prog.Quit
	if onStart != nil {
		onStart()
	}
	_, err := prog.Run()
	return err
}

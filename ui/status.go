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
	// OpenConsole opens a terminal with the live status window (tray-only mode).
	OpenConsole func()
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
	notice  string // transient one-line feedback ("already connected")
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
	case backMsg:
		return m.closeSetup()
	case tea.KeyMsg:
		// While the account wizard is open it owns the keyboard, except for
		// the two keys that belong to the host: esc = back, ctrl+c = quit.
		if m.inSetup {
			switch msg.String() {
			case "ctrl+c":
				if m.s.Quit != nil {
					m.s.Quit()
				}
				return m, tea.Quit
			case "esc":
				return m.closeSetup()
			}
			if m.setup != nil {
				next, cmd := m.setup.Update(msg)
				if sm, ok := next.(setupModel); ok {
					m.setup = &sm
				}
				return m, cmd
			}
			return m.closeSetup()
		}
		switch msg.String() {
		case "ctrl+c", "q":
			if m.s.Quit != nil {
				m.s.Quit()
			}
			return m, tea.Quit // the TUI leaves; main then stops the session
		case "r":
			// r is always "reconnect" — never an alias for something else.
			if m.online() {
				m.notice = "already connected"
				return m, nil
			}
			if m.s.Reconnect != nil {
				m.s.Reconnect()
				m.notice = "reconnecting…"
			}
			return m, nil
		case "a":
			return m.openSetup()
		}
	case accountsMsg:
		return m.openSetup()
	case tickMsg:
		m.notice = ""
		return m, tick()
	}
	return m, nil
}

// openSetup switches to the account wizard (keys or tray) and keeps watching
// for further tray clicks. The wizard is embedded: its "q"/"esc" come back here
// instead of quitting the app.
func (m statusModel) openSetup() (tea.Model, tea.Cmd) {
	if m.setup == nil {
		sm := newSetupModel(m.deps)
		m.setup = &sm
	}
	m.setup.embedded = true
	m.setup.onBack = func() tea.Cmd { return func() tea.Msg { return backMsg{} } }
	m.inSetup = true
	return m, tea.Batch(m.setup.Init(), waitAccounts(m.s))
}

// closeSetup returns from the wizard to the status screen.
func (m statusModel) closeSetup() (tea.Model, tea.Cmd) {
	m.inSetup = false
	m.setup = nil
	return m, nil
}

func (m statusModel) View() string {
	if m.inSetup {
		return m.setup.View()
	}
	views := []string{m.headerView(), m.connectionView(), m.nowPlayingView()}
	if m.s.UpdateNote != nil {
		if note := m.s.UpdateNote(); note != "" {
			views = append(views, styWarn.Render("▲ "+note))
		}
	}
	views = append(views, m.logView(), "",
		hints([2]string{"a", "accounts"}, [2]string{"r", "reconnect"},
			[2]string{"q", "quit"}), "")
	return strings.Join(views, "\n")
}

// online reports whether the socket is up right now.
func (m statusModel) online() bool {
	return m.s.WS != nil && m.s.WS.State() == jfin.StateConnected
}

// connectionView is the single place that explains the connection state, so the
// header dot and this line can never disagree.
func (m statusModel) connectionView() string {
	var text string
	switch {
	case m.online():
		text = styOK.Render("● connected") + styDim.Render("  — ready to cast")
	case m.s.WS != nil && m.s.WS.State() == jfin.StateReconnecting:
		text = styWarn.Render("● reconnecting") + styDim.Render("  — retrying, r to retry now")
	default:
		text = styBad.Render("● offline") + styDim.Render("  — r to reconnect, check the server")
	}
	if m.notice != "" {
		text += styDim.Render("   (" + m.notice + ")")
	}
	return text
}

func (m statusModel) headerView() string {
	line := clip(m.s.Account.Server, 44) + styDim.Render("  ·  ") + clip(m.s.Account.Username, 20)
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
	// prog.Quit() must never run on the program's own goroutine: bubbletea
	// hands messages over an unbuffered channel, so sending from inside Update
	// deadlocks the loop. That is why "q" used to only disconnect.
	s.QuitUI = func() { go prog.Quit() }
	if onStart != nil {
		onStart()
	}
	_, err := prog.Run()
	return err
}

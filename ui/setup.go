package ui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"mpv-shim/jfin"
)

// Account store + client factory the TUI needs. main passes them in so this
// package does not depend on the config types.
type Deps struct {
	Creds    *jfin.CredFile
	CredPath string
	// NewClient returns an unauthenticated client for a server URL.
	NewClient func(server string) *jfin.Client
}

// --- setup wizard ---------------------------------------------------------

type screen int

const (
	screenList screen = iota
	screenAddPassword
	screenAddQuickConnect
	screenQuickCode
)

type setupModel struct {
	deps Deps
	scr  screen
	sel  int
	err  string

	inputs []textinput.Model
	focus  int

	qc     *jfin.QuickConnect
	qcWait int // seconds elapsed waiting for the exchange

	// embedded = the wizard is shown inside the status screen (not as its own
	// program). Then "q"/"esc" go back to the status screen instead of quitting
	// the app, and ctrl+c quits.
	embedded bool
	onBack   func() tea.Cmd
}

// backMsg asks the host screen to close the wizard.
type backMsg struct{}

func newSetupModel(d Deps) setupModel {
	mk := func(ph string) textinput.Model {
		ti := textinput.New()
		ti.Placeholder = ph
		return ti
	}
	pass := mk("password")
	pass.EchoMode = textinput.EchoPassword
	m := setupModel{deps: d, inputs: []textinput.Model{
		mk("https://jellyfin.example.com"),
		mk("username"),
		pass,
	}}
	m.inputs[0].Focus()
	return m
}

func (m setupModel) Init() tea.Cmd { return nil }

func (m setupModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// In an input screen the focused field owns the keystroke first;
		// only the control keys are handled here.
		if m.scr == screenAddPassword || m.scr == screenAddQuickConnect {
			if m.focus < len(m.inputs) {
				var cmd tea.Cmd
				m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
				if !isControlKey(msg) {
					return m, cmd
				}
			}
		}
		switch msg.String() {
		case "ctrl+c":
			return m, tea.Quit
		case "b": // explicit "back", whatever the screen
			if m.scr == screenList {
				return m.backOrQuit()
			}
			m.scr, m.err = screenList, ""
		case "q", "esc":
			if m.scr == screenList {
				return m.backOrQuit()
			}
			if m.scr != screenQuickCode { // a pending Quick Connect keeps polling
				m.scr, m.err = screenList, ""
			}
		case "up", "k":
			if m.scr == screenList && len(m.deps.Creds.Accounts) > 0 {
				m.sel = (m.sel - 1 + len(m.deps.Creds.Accounts)) % len(m.deps.Creds.Accounts)
			}
		case "down", "j":
			if m.scr == screenList && len(m.deps.Creds.Accounts) > 0 {
				m.sel = (m.sel + 1) % len(m.deps.Creds.Accounts)
			}
		case "r":
			if m.scr == screenList && len(m.deps.Creds.Accounts) > 0 {
				a := m.deps.Creds.Accounts[m.sel]
				m.deps.Creds.Remove(a.Server, a.Username)
				m.sel = 0
				m.save()
			}
		case "p":
			if m.scr == screenList {
				m.scr, m.focus, m.err = screenAddPassword, 0, ""
				m.reset()
			}
		case "i":
			if m.scr == screenList {
				m.scr, m.focus, m.err = screenAddQuickConnect, 0, ""
				m.reset()
			}
		case "tab":
			return m.moveFocus(1)
		case "shift+tab":
			return m.moveFocus(-1)
		case "enter":
			return m.onEnter()
		}
	case backMsg:
		return m.backOrQuit()
	case quickPollMsg:
		return m.quickResult(msg)
	case tickMsg:
		if m.scr == screenQuickCode {
			return m.pollQuickConnect()
		}
		return m, tick()
	}
	return m, nil
}

// moveFocus moves the field cursor (tab / shift+tab).
func (m setupModel) moveFocus(delta int) (tea.Model, tea.Cmd) {
	m.focus += delta
	if m.focus < 0 {
		m.focus = 0
	}
	if m.focus >= len(m.inputs) {
		m.focus = len(m.inputs) - 1
	}
	for i := range m.inputs {
		m.inputs[i].Blur()
	}
	m.inputs[m.focus].Focus()
	return m, nil
}

func (m setupModel) reset() {
	for i := range m.inputs {
		m.inputs[i].SetValue("")
		m.inputs[i].Blur()
	}
	if m.focus < len(m.inputs) {
		m.inputs[m.focus].Focus()
	}
}

func (m setupModel) onEnter() (tea.Model, tea.Cmd) {
	switch m.scr {
	case screenAddPassword:
		switch m.focus {
		case 0, 1:
			return m.moveFocus(1)
		default:
			server := strings.TrimRight(strings.TrimSpace(m.inputs[0].Value()), "/")
			user := strings.TrimSpace(m.inputs[1].Value())
			pass := m.inputs[2].Value()
			if server == "" || user == "" || pass == "" {
				m.err = "server, username and password are required"
				return m, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			c := m.deps.NewClient(server)
			resp, err := c.Login(ctx, user, pass)
			if err != nil {
				m.err = fmt.Sprintf("login failed: %v", err)
				return m, nil
			}
			m.store(c, user, server, resp.User.Name)
			return m, nil
		}
	case screenAddQuickConnect:
		server := strings.TrimRight(strings.TrimSpace(m.inputs[0].Value()), "/")
		if server == "" {
			m.err = "server URL is required"
			return m, nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c := m.deps.NewClient(server)
		if !c.QuickConnectEnabled(ctx) {
			m.err = "server does not offer Quick Connect"
			return m, nil
		}
		qc, err := c.QuickConnectInitiate(ctx)
		if err != nil {
			m.err = fmt.Sprintf("quick connect: %v", err)
			return m, nil
		}
		m.qc, m.qcWait, m.err = qc, 0, ""
		m.scr = screenQuickCode
		return m, tea.Batch(tick())
	}
	return m, nil
}

// quickPollMsg is the Quick Connect exchange result.
type quickPollMsg struct {
	resp *jfin.LoginResponse
	err  error
	done bool
}

func (m setupModel) pollQuickConnect() (tea.Model, tea.Cmd) {
	m.qcWait++
	return m, tea.Batch(tick(), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c := m.deps.NewClient(m.qcServer())
		resp, ok, err := c.QuickConnectExchange(ctx, m.qc)
		return quickPollMsg{resp: resp, err: err, done: ok}
	})
}

func (m setupModel) qcServer() string {
	return strings.TrimRight(strings.TrimSpace(m.inputs[0].Value()), "/")
}

func (m *setupModel) save() {
	if err := m.deps.Creds.Save(m.deps.CredPath); err != nil {
		m.err = fmt.Sprintf("saving credentials: %v", err)
	}
}

// store persists a successful login as the active account.
func (m *setupModel) store(c *jfin.Client, username, server, display string) {
	m.deps.Creds.Upsert(jfin.Account{
		Server:      server,
		Username:    username,
		User:        display,
		AccessToken: c.Token,
		UserID:      c.UserID,
		DeviceID:    c.DeviceID,
	})
	m.deps.Creds.SetActive(server, username)
	m.save()
	m.scr, m.err, m.qc = screenList, "", nil
}

// backOrQuit leaves the list screen: embedded in the status UI it asks the host
// to take us back, on its own it quits the wizard program.
func (m setupModel) backOrQuit() (tea.Model, tea.Cmd) {
	if m.embedded && m.onBack != nil {
		return m, m.onBack()
	}
	return m, tea.Quit
}

// isControlKey reports whether a key drives the wizard rather than the text
// field (typing must win over our single-letter shortcuts).
func isControlKey(k tea.KeyMsg) bool {
	switch k.String() {
	case "ctrl+c", "enter", "tab", "shift+tab", "esc", "up", "down":
		return true
	}
	return false
}

func (m setupModel) View() string {
	var b strings.Builder
	switch m.scr {
	case screenList:
		b.WriteString(panelW("accounts", 56, m.accountRows()...))
		b.WriteString("\n")
		rows := [][2]string{
			{"p", "add with password"},
			{"i", "add with Quick Connect"},
			{"r", "remove"},
		}
		b.WriteString(hints(append(rows, m.listHints()...)...))
	case screenAddPassword:
		labels := []string{"Server", "Username", "Password"}
		rows := make([]string, 0, len(m.inputs))
		for i, ti := range m.inputs {
			label := styDim.Render(pad(labels[i], 10))
			if i == m.focus {
				label = styTitle.Render(pad(labels[i], 10))
			}
			rows = append(rows, label+ti.View())
		}
		b.WriteString(panelW("add account", 52, rows...))
		b.WriteString("\n")
		b.WriteString(hints([2]string{"enter", "next / log in"}, [2]string{"shift+tab", "back field"},
			[2]string{"esc", "cancel"}, [2]string{"b", "back"}))
	case screenAddQuickConnect:
		b.WriteString(panelW("add account · quick connect", 52,
			styDim.Render(pad("Server", 10))+m.inputs[0].View(),
			"",
			styDim.Render("The server shows a code; enter it in the web UI to authorize."),
		))
		b.WriteString("\n")
		b.WriteString(hints([2]string{"enter", "start"}, [2]string{"esc", "back"}))
	case screenQuickCode:
		body := []string{}
		if m.qc != nil {
			body = append(body,
				kv("Code", styOK.Render(m.qc.Code)),
				kv("Server", m.qcServer()),
				"",
				styDim.Render("Open the web UI, then Quick Connect → enter the code above."),
			)
		}
		body = append(body, "", styDim.Render(fmt.Sprintf("waiting %ds…", m.qcWait)))
		b.WriteString(panelW("quick connect", 52, body...))
		b.WriteString("\n")
		b.WriteString(hints([2]string{"esc", "cancel"}))
	}
	if m.err != "" {
		b.WriteString("\n" + styBad.Render("✖ "+m.err) + "\n")
	}
	return b.String()
}

// listHints are the trailing hints of the account list, which differ between
// the standalone wizard and the wizard embedded in the status screen.
func (m setupModel) listHints() [][2]string {
	if m.embedded {
		return [][2]string{{"esc", "back"}, {"ctrl+c", "quit"}}
	}
	return [][2]string{{"q", "quit"}}
}

// accountRows renders the account list, one line each.
func (m setupModel) accountRows() []string {
	accts := m.deps.Creds.Accounts
	if len(accts) == 0 {
		return []string{styDim.Render("no accounts yet — add one below")}
	}
	rows := make([]string, 0, len(accts))
	for i, a := range accts {
		active := ""
		if i == m.deps.Creds.Active {
			active = styOK.Render(" ← active")
		}
		cursor := "  "
		if i == m.sel {
			cursor = styTitle.Render("▸ ")
		}
		name := a.Username
		if a.User != "" && a.User != a.Username {
			name = a.Username + styDim.Render(" ("+a.User+")")
		}
		rows = append(rows, cursor+pad(clip(a.Server, 40), 41)+name+active)
	}
	return rows
}

// quickResult applies the async Quick Connect exchange result.
func (m setupModel) quickResult(msg quickPollMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		m.err = fmt.Sprintf("quick connect: %v", msg.err)
		return m, nil
	}
	if !msg.done {
		return m, nil
	}
	server := m.qcServer()
	c := m.deps.NewClient(server)
	c.Token, c.UserID = msg.resp.AccessToken, msg.resp.User.ID
	m.store(c, msg.resp.User.Name, server, msg.resp.User.Name)
	return m, nil
}

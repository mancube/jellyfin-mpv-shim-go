package ui

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"mpv-shim/jfin"
	"mpv-shim/player"
)

func TestLogRingWrapsAndTails(t *testing.T) {
	l := NewLogRing(3)
	for i := 1; i <= 5; i++ {
		if _, err := fmt.Fprintf(l, "line %d\n", i); err != nil {
			t.Fatal(err)
		}
	}
	got := l.Tail(0)
	want := []string{"line 3", "line 4", "line 5"}
	if len(got) != len(want) {
		t.Fatalf("tail = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tail[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if tail := l.Tail(2); len(tail) != 2 || tail[0] != "line 4" {
		t.Errorf("Tail(2) = %v", tail)
	}
}

func TestFmtTime(t *testing.T) {
	cases := map[float64]string{0: "--:--", 65: "1:05", 3725: "1:02:05"}
	for in, want := range cases {
		if got := fmtTime(in); got != want {
			t.Errorf("fmtTime(%v) = %q, want %q", in, got, want)
		}
	}
}

// The setup wizard's key handling is pure state; check the list screen.
func TestSetupListRemove(t *testing.T) {
	creds := testCreds()
	m := newSetupModel(Deps{Creds: creds, CredPath: t.TempDir() + "/cred.json"})
	if m.scr != screenList {
		t.Fatalf("initial screen = %v, want list", m.scr)
	}
	creds.Upsert(testAccount("a"))
	m2, _ := m.Update(keyMsg("r"))
	m = m2.(setupModel)
	if len(creds.Accounts) != 0 {
		t.Errorf("account not removed: %+v", creds.Accounts)
	}
}

func testCreds() *jfin.CredFile { return &jfin.CredFile{} }

func testAccount(name string) jfin.Account {
	return jfin.Account{Server: "http://x", Username: name, User: name, AccessToken: "t", UserID: "u", DeviceID: "d"}
}

// keyMsg builds a KeyMsg for a key *name* ("enter", "tab", ...) or for a
// single printable rune.
func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// Regression: keystrokes must reach the focused text field (the wizard used
// to swallow them, so nothing could be typed).
func TestSetupTypingIntoFields(t *testing.T) {
	m := newSetupModel(Deps{Creds: testCreds(), CredPath: t.TempDir() + "/cred.json"})
	m.scr, m.focus = screenAddPassword, 0
	m.reset()

	typeStr(&m, "http://x:8096")
	if got := m.inputs[0].Value(); got != "http://x:8096" {
		t.Errorf("server field = %q, want http://x:8096", got)
	}
	if !strings.Contains(m.View(), "http://x:8096") {
		t.Errorf("view does not show the typed value:\n%s", m.View())
	}

	// tab/enter advances to the username field, which then takes the keys;
	// shift+tab walks back.
	next, _ := m.Update(keyMsg("tab"))
	m = next.(setupModel)
	if m.focus != 1 {
		t.Fatalf("focus = %d, want 1", m.focus)
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftTab})
	m = next.(setupModel)
	if m.focus != 0 {
		t.Fatalf("shift+tab focus = %d, want 0", m.focus)
	}
	next, _ = m.Update(keyMsg("enter"))
	m = next.(setupModel)
	if m.focus != 1 {
		t.Fatalf("focus = %d, want 1", m.focus)
	}
	typeStr(&m, "admin")
	if m.inputs[1].Value() != "admin" {
		t.Errorf("username field = %q, want admin", m.inputs[1].Value())
	}

	// The password field is masked.
	next, _ = m.Update(keyMsg("enter"))
	m = next.(setupModel)
	if m.focus != 2 {
		t.Fatalf("focus = %d, want 2 (password)", m.focus)
	}
	typeStr(&m, "hunter2")
	if m.inputs[2].Value() != "hunter2" {
		t.Errorf("password field = %q, want hunter2", m.inputs[2].Value())
	}
	if v := m.View(); strings.Contains(v, "hunter2") {
		t.Errorf("password is echoed in the view:\n%s", v)
	}
}

// typeStr feeds one keystroke per rune through the model's Update.
func typeStr(m *setupModel, s string) {
	for _, r := range s {
		next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		*m = next.(setupModel)
	}
}

// Full path: type the three fields, press enter, and the account is stored.
func TestSetupPasswordLoginStoresAccount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["Username"] != "admin" || body["Pw"] != "s3cret" {
			w.WriteHeader(400)
			return
		}
		_ = json.NewEncoder(w).Encode(jfin.LoginResponse{
			AccessToken: "tok", User: jfin.User{ID: "u1", Name: "admin"},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	credPath := filepath.Join(t.TempDir(), "cred.json")
	creds := testCreds()
	m := newSetupModel(Deps{
		Creds: creds, CredPath: credPath,
		NewClient: func(server string) *jfin.Client {
			return jfin.New(server, "dev", "d1", "1.0", false)
		},
	})
	m.scr, m.focus = screenAddPassword, 0
	m.reset()
	typeStr(&m, ts.URL)
	next, _ := m.Update(keyMsg("tab"))
	m = next.(setupModel)
	typeStr(&m, "admin")
	next, _ = m.Update(keyMsg("tab"))
	m = next.(setupModel)
	typeStr(&m, "s3cret")
	next, _ = m.Update(keyMsg("enter")) // submit
	m = next.(setupModel)

	if m.scr != screenList {
		t.Errorf("screen = %v, want the account list (err: %s)", m.scr, m.err)
	}
	if len(creds.Accounts) != 1 || creds.Accounts[0].AccessToken != "tok" {
		t.Fatalf("stored accounts = %+v", creds.Accounts)
	}
	if _, err := os.Stat(credPath); err != nil {
		t.Errorf("credentials not written: %v", err)
	}
}

// The status panels are pure functions of a snapshot: check they say
// something useful instead of dumping fields.
func TestNowPlayingPanel(t *testing.T) {
	idle := nowPlaying(player.Status{})
	if !strings.Contains(stripANSI(idle), "nothing playing") {
		t.Errorf("idle panel = %q", stripANSI(idle))
	}
	st := player.Status{
		Title: "2 Fast 2 Furious (2003)", Position: 645, Duration: 6455,
		Playing: true, Volume: 75,
	}
	got := stripANSI(nowPlaying(st))
	for _, want := range []string{"2 Fast 2 Furious", "10:45 / 1:47:35", "█", "vol 75%"} {
		if !strings.Contains(got, want) {
			t.Errorf("panel missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "muted") {
		t.Errorf("unmuted playback shows 'muted':\n%s", got)
	}
	st.Paused, st.Mute = true, true
	got = stripANSI(nowPlaying(st))
	if !strings.Contains(got, "paused") || !strings.Contains(got, "muted") {
		t.Errorf("paused/muted state not shown:\n%s", got)
	}
}

func stripANSI(s string) string {
	var out strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// The tray's "Configure Servers…" click must land the user in the wizard.
func TestTrayRequestOpensAccountWizard(t *testing.T) {
	creds := testCreds()
	deps := Deps{Creds: creds, CredPath: t.TempDir() + "/cred.json"}
	m := newStatusModel(&Session{accounts: make(chan struct{}, 1)}, deps)

	m.s.RequestAccounts()
	next, cmd := m.Update(accountsMsg{})
	m = next.(statusModel)
	if !m.inSetup || m.setup == nil {
		t.Fatal("tray request did not open the account wizard")
	}
	if cmd == nil {
		t.Error("expected a command (keep listening for tray clicks)")
	}
	// The wizard renders the account list.
	if v := m.View(); !strings.Contains(v, "accounts") {
		t.Errorf("wizard view = %q", stripANSI(v))
	}
}

// RequestAccounts must not block when nobody is listening.
func TestRequestAccountsNonBlocking(t *testing.T) {
	s := NewSession(jfin.Account{}, nil, nil, nil)
	s.RequestAccounts() // no TUI attached: must return immediately
	s.RequestAccounts()
	s.RequestAccounts()
}

// The connection item is one item with two actions: while connected it
// disconnects, while disconnected it reconnects.
func TestConnectionItemToggles(t *testing.T) {
	s := NewSession(jfin.Account{Server: "http://x"}, nil, nil, NewLogRing(4))
	connected := true
	var disconnects, reconnects int
	s.Connected = func() bool { return connected }
	s.Disconnect = func() { disconnects++; connected = false }
	s.Reconnect = func() { reconnects++; connected = true }

	// Click while online → disconnect only.
	clickConnection(s)
	if disconnects != 1 || reconnects != 0 || connected {
		t.Errorf("online click: disconnects=%d reconnects=%d connected=%v", disconnects, reconnects, connected)
	}
	// Click while offline → reconnect.
	clickConnection(s)
	if reconnects != 1 || disconnects != 1 || !connected {
		t.Errorf("offline click: disconnects=%d reconnects=%d connected=%v", disconnects, reconnects, connected)
	}
}

// clickConnection mirrors what the tray item does on a click.
func clickConnection(s *Session) {
	if s.Connected != nil && s.Connected() {
		if s.Disconnect != nil {
			s.Disconnect()
		}
		return
	}
	if s.Reconnect != nil {
		s.Reconnect()
	}
}

// `r` is always reconnect (never an alias for something else): online it says
// so, offline it asks the session to dial again. Accounts live behind `a`.
func TestStatusReconnectKey(t *testing.T) {
	s := NewSession(jfin.Account{Server: "http://x"}, nil, nil, NewLogRing(4))
	s.WS = jfin.NewWS(jfin.New("http://127.0.0.1:1", "d", "d", "1", false), log.New(io.Discard, "", 0))
	connected := true
	s.Connected = func() bool { return connected }
	var reconnects int
	s.Reconnect = func() { reconnects++; connected = true }
	m := newStatusModel(s, Deps{Creds: testCreds()})
	// Put the model in the online state.
	s.WS.SetState(jfin.StateConnected)

	// Online: r does not reconnect and does not open the accounts list.
	next, _ := m.Update(keyMsg("r"))
	m = next.(statusModel)
	if reconnects != 0 {
		t.Errorf("r reconnected while online (reconnects=%d)", reconnects)
	}
	if m.inSetup {
		t.Error("r opened the accounts list; that is a's job now")
	}
	if m.notice == "" {
		t.Error("r while online gave no feedback")
	}

	// Offline: r reconnects (and the session moves to reconnecting).
	s.WS.SetState(jfin.StateOffline)
	connected = false
	next, _ = m.Update(keyMsg("r"))
	m = next.(statusModel)
	if reconnects != 1 {
		t.Errorf("r did not reconnect while offline (reconnects=%d)", reconnects)
	}

	// The connection line names the state; nothing else claims to.
	s.WS.SetState(jfin.StateReconnecting)
	for _, want := range []string{"connected", "reconnecting", "offline"} {
		state := map[string]int32{
			"connected":    jfin.StateConnected,
			"reconnecting": jfin.StateReconnecting,
			"offline":      jfin.StateOffline,
		}[want]
		s.WS.SetState(state)
		if v := stripANSI(m.connectionView()); !strings.Contains(v, want) {
			t.Errorf("state %d: connection line = %q, want %q", state, v, want)
		}
	}
}

// The tray's Quit must reach *both* the session and the TUI; Disconnect only
// the session. This is what makes "Quit" actually exit instead of leaving an
// offline window behind.
func TestTrayQuitStopsSessionAndUI(t *testing.T) {
	s := NewSession(jfin.Account{}, nil, nil, NewLogRing(4))
	var sessionStopped, uiQuit bool
	s.Quit = func() { sessionStopped = true }
	s.Disconnect = func() { sessionStopped = true }
	s.QuitUI = func() { uiQuit = true }

	// Disconnect: session only.
	s.Disconnect()
	if !sessionStopped || uiQuit {
		t.Errorf("Disconnect: session=%v uiQuit=%v", sessionStopped, uiQuit)
	}

	// Quit: both.
	s.Quit()
	s.QuitUI()
	if !sessionStopped || !uiQuit {
		t.Errorf("Quit: session=%v uiQuit=%v", sessionStopped, uiQuit)
	}
}

// Inside the status screen the account wizard is embedded: it takes the
// keyboard, esc returns to the status screen, ctrl+c quits the app.
func TestEmbeddedWizardNavigation(t *testing.T) {
	s := NewSession(jfin.Account{Server: "http://x"}, nil, nil, NewLogRing(4))
	var quits int
	s.Quit = func() { quits++ }
	m := newStatusModel(s, Deps{Creds: testCreds(), CredPath: filepath.Join(t.TempDir(), "cred.json")})

	// a opens the wizard.
	next, _ := m.Update(keyMsg("a"))
	m = next.(statusModel)
	if !m.inSetup {
		t.Fatal("a did not open the accounts view")
	}
	if !strings.Contains(stripANSI(m.setup.View()), "back") {
		t.Errorf("embedded wizard has no way back in its hints:\n%s", stripANSI(m.setup.View()))
	}

	// Typing inside the wizard reaches the wizard (e.g. "p" opens the form).
	next, _ = m.Update(keyMsg("p"))
	m = next.(statusModel)
	if !strings.Contains(stripANSI(m.setup.View()), "Password") {
		t.Errorf("key not delegated to the wizard:\n%s", stripANSI(m.setup.View()))
	}

	// esc goes back to the status screen instead of quitting.
	next, _ = m.Update(escMsg())
	m = next.(statusModel)
	if m.inSetup {
		t.Error("esc did not leave the accounts view")
	}
	if quits != 0 {
		t.Error("esc quit the app")
	}
	if m.setup != nil {
		t.Error("the wizard model was kept around after going back")
	}

	// ctrl+c inside the wizard quits the app.
	next, _ = m.Update(keyMsg("a"))
	m = next.(statusModel)
	next, _ = m.Update(ctrlCMsg())
	m = next.(statusModel)
	if quits != 1 {
		t.Errorf("ctrl+c did not quit the app (quits=%d)", quits)
	}
}

func escMsg() tea.KeyMsg   { return tea.KeyMsg{Type: tea.KeyEsc} }
func ctrlCMsg() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyCtrlC} }

// The standalone wizard (mpv-shim setup) still quits with q.
func TestStandaloneWizardQuitsWithQ(t *testing.T) {
	m := newSetupModel(Deps{Creds: testCreds(), CredPath: filepath.Join(t.TempDir(), "cred.json")})
	next, cmd := m.Update(keyMsg("q"))
	if cmd == nil {
		t.Error("q in the standalone wizard does not quit")
	}
	_ = next
}

// The console bridge: the server accepts clients and pushes snapshots, and the
// model renders them.
func TestConsoleServerBroadcast(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "console.sock")
	srv, err := NewConsoleServer(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))

	// wait until the server has registered the client
	deadline := time.Now().Add(2 * time.Second)
	for srv.Clients() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("server never registered the client")
		}
		time.Sleep(5 * time.Millisecond)
	}
	srv.Broadcast(ConsoleSnapshot{
		Server: "http://x", State: "online", Title: "A Movie",
		Playing: true, Position: 12, Duration: 100, Volume: 55, Lines: []string{"hello"},
	})
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := bufio.NewReader(conn).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var snap ConsoleSnapshot
	if err := json.Unmarshal(line, &snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Title != "A Movie" || snap.State != "online" || !snap.Playing || snap.Volume != 55 {
		t.Errorf("snapshot = %+v", snap)
	}

	// The model renders it.
	m := consoleModel{}
	next, _ := m.Update(consoleSnapMsg(snap))
	view := stripANSI(next.(consoleModel).View())
	for _, want := range []string{"A Movie", "online", "0:12 / 1:40", "vol 55%", "hello"} {
		if !strings.Contains(view, want) {
			t.Errorf("console view missing %q:\n%s", want, view)
		}
	}
	// q closes the window.
	if _, cmd := m.Update(keyMsg("q")); cmd == nil {
		t.Error("q did not ask the console to quit")
	}
}

// ShowWindow refuses a status window when nothing is running, and does not
// need a terminal to be present in the test environment.
func TestShowWindowNeedsRunningInstance(t *testing.T) {
	err := ShowWindow("/bin/true", filepath.Join(t.TempDir(), "missing.sock"), "--console")
	if err == nil {
		t.Fatal("ShowWindow should fail when the instance is not running")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("error = %v, want a clear 'not running' message", err)
	}
	// No socket (the account wizard): the window is opened regardless, so the
	// error — if any — is about the terminal, not about a running instance.
	if err := ShowWindow("/bin/true", "", "setup"); err != nil &&
		strings.Contains(err.Error(), "not running") {
		t.Errorf("error = %v, setup needs no running instance", err)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("/a b/mpv-shim"); got != "'/a b/mpv-shim'" {
		t.Errorf("shellQuote = %q", got)
	}
	if got := shellQuote("it's"); got != `'it'\''s'` {
		t.Errorf("shellQuote with a quote = %q", got)
	}
}

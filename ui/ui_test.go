package ui

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

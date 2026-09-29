package ui

import (
	"fmt"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"mpv-shim/jfin"
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

func keyMsg(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

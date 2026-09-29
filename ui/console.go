package ui

// The console bridge: when mpv-shim runs without a terminal (tray-only), the
// tray's "Show Console" opens a terminal running `mpv-shim --console`, which
// renders the same status screen — fed by the running process over a small unix
// socket. One JSON line per client per second, nothing more.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// ConsoleSnapshot is everything the console window renders.
type ConsoleSnapshot struct {
	Server   string   `json:"server"`
	Device   string   `json:"device"`
	State    string   `json:"state"` // online | reconnecting | offline
	Title    string   `json:"title"`
	Playing  bool     `json:"playing"`
	Paused   bool     `json:"paused"`
	Position float64  `json:"position"`
	Duration float64  `json:"duration"`
	Volume   float64  `json:"volume"`
	Muted    bool     `json:"muted"`
	Lines    []string `json:"lines"`
	Note     string   `json:"note"`
}

// --- server side ----------------------------------------------------------

// ConsoleServer broadcasts snapshots to every connected console window.
type ConsoleServer struct {
	path     string
	ln       net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool
	last     ConsoleSnapshot
	haveLast bool
}

// NewConsoleServer removes a stale socket and starts listening.
func NewConsoleServer(path string) (*ConsoleServer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	s := &ConsoleServer{path: path, ln: ln, conns: map[net.Conn]struct{}{}}
	go s.accept()
	return s, nil
}

// Path is the socket the clients connect to.
func (s *ConsoleServer) Path() string { return s.path }

func (s *ConsoleServer) accept() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = conn.Close()
			return
		}
		s.conns[conn] = struct{}{}
		// Send the current state right away so the window is never blank for
		// a full tick.
		if s.haveLast {
			if b, err := json.Marshal(s.last); err == nil {
				_, _ = conn.Write(append(b, '\n'))
			}
		}
		s.mu.Unlock()
	}
}

// Clients is how many console windows are attached (tests, diagnostics).
func (s *ConsoleServer) Clients() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// Broadcast sends one snapshot to every console window.
func (s *ConsoleServer) Broadcast(snap ConsoleSnapshot) {
	s.mu.Lock()
	s.last, s.haveLast = snap, true
	s.mu.Unlock()
	b, err := json.Marshal(snap)
	if err != nil {
		return
	}
	b = append(b, '\n')
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		if _, err := c.Write(b); err != nil {
			_ = c.Close()
			delete(s.conns, c)
		}
	}
}

// Close stops the server and removes the socket file.
func (s *ConsoleServer) Close() {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.Close()
	}
	s.conns = map[net.Conn]struct{}{}
	s.mu.Unlock()
	_ = s.ln.Close()
	_ = os.Remove(s.path)
}

// --- client side ----------------------------------------------------------

type consoleSnapMsg ConsoleSnapshot

// consoleModel renders the snapshots the running process sends.
type consoleModel struct {
	snap ConsoleSnapshot
}

func (m consoleModel) Init() tea.Cmd { return nil }

func (m consoleModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case consoleSnapMsg:
		m.snap = ConsoleSnapshot(msg)
	case tea.KeyMsg:
		if k := msg.String(); k == "q" || k == "ctrl+c" || k == "esc" {
			return m, tea.Quit
		}
	}
	return m, nil
}

func (m consoleModel) View() string {
	s := m.snap
	conn := styBad.Render("● " + s.State)
	switch s.State {
	case "online":
		conn = styOK.Render("● online")
	case "reconnecting":
		conn = styWarn.Render("● reconnecting")
	}
	header := panel("mpv-shim", conn+styDim.Render("   "+clip(s.Server, 40))+
		styDim.Render("  ·  ")+clip(s.Device, 24))

	var nowPlaying []string
	switch {
	case s.Playing:
		state := styOK.Render("▶ playing")
		if s.Paused {
			state = styWarn.Render("❚❚ paused")
		}
		vol := fmt.Sprintf("vol %.0f%%", s.Volume)
		if s.Muted {
			vol += "  " + styBad.Render("muted")
		}
		nowPlaying = []string{
			styText.Render(clip(s.Title, 44)),
			"",
			state + "  " + bar(frac(s.Position, s.Duration), 26),
			styDim.Render(fmtTime(s.Position) + " / " + fmtTime(s.Duration) + "   " + vol),
		}
	default:
		nowPlaying = []string{styDim.Render("nothing playing — cast something from the web UI")}
	}

	out := []string{header, panelW("now playing", 60, nowPlaying...)}
	if s.Note != "" {
		out = append(out, styWarn.Render("▲ "+s.Note))
	}
	lines := s.Lines
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	logLines := []string{styTitle.Render("log")}
	for _, l := range lines {
		logLines = append(logLines, styDim.Render("│ ")+styText.Render(clip(l, 58)))
	}
	out = append(out, strings.Join(logLines, "\n"), "",
		hints([2]string{"q", "close this window"}), "")
	return strings.Join(out, "\n")
}

// RunConsoleClient connects to a running instance and shows its status until
// the user closes the window. This is what the tray's "Show Console" starts.
func RunConsoleClient(ctx context.Context, path string) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return fmt.Errorf("mpv-shim is not running (no socket at %s)", path)
	}
	defer conn.Close()

	prog := tea.NewProgram(consoleModel{},
		tea.WithAltScreen(), tea.WithInput(os.Stdin), tea.WithOutput(os.Stdout))
	go func() {
		<-ctx.Done()
		prog.Quit()
	}()

	// The instance went away (socket closed): close this window too.
	go func() {
		sc := bufio.NewScanner(conn)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			var snap ConsoleSnapshot
			if json.Unmarshal(sc.Bytes(), &snap) == nil {
				prog.Send(consoleSnapMsg(snap))
			}
		}
		prog.Quit()
	}()
	// q / ctrl+c / esc only stop the program: the scanner is still blocked on
	// the socket, so returning here is what actually closes the window. The
	// deferred conn.Close unblocks that goroutine.
	_, _ = prog.Run()
	return nil
}

// --- launching a terminal --------------------------------------------------

// terminalEmulators lists the emulators we try, with the arguments each needs
// to run a command. The first one installed wins.
var terminalEmulators = []struct {
	bin  string
	args []string // %s is where the command goes
}{
	{"konsole", []string{"-e", "%s"}},
	{"gnome-terminal", []string{"--", "%s"}},
	{"xfce4-terminal", []string{"-x", "%s"}},
	{"alacritty", []string{"-e", "%s"}},
	{"kitty", []string{"%s"}},
	{"wezterm", []string{"start", "--", "%s"}},
	{"foot", []string{"%s"}},
	{"x-terminal-emulator", []string{"-e", "%s"}},
}

// ShowWindow opens a terminal running `selfPath args...` — the status window
// (socket set) or the account wizard (socket empty). selfPath is this
// executable; it reports the command to run when no emulator is installed.
func ShowWindow(selfPath, socket string, args ...string) error {
	if socket != "" {
		// A status window attaches to the running instance: fail before
		// spawning a terminal that would only show "not running".
		if err := dialCheck(socket); err != nil {
			return err
		}
	}
	quoted := make([]string, 0, len(args)+1)
	quoted = append(quoted, shellQuote(selfPath))
	for _, a := range args {
		quoted = append(quoted, shellQuote(a))
	}
	shell := strings.Join(quoted, " ")
	for _, emu := range terminalEmulators {
		bin, err := exec.LookPath(emu.bin)
		if err != nil {
			continue
		}
		args := make([]string, 0, len(emu.args))
		for _, a := range emu.args {
			if a == "%s" {
				args = append(args, shell)
			} else {
				args = append(args, a)
			}
		}
		cmd := exec.Command(bin, args...)
		cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
		if err := cmd.Start(); err != nil {
			continue
		}
		go func() { _ = cmd.Wait() }()
		return nil
	}
	return fmt.Errorf("no terminal emulator found (tried %s); run `%s` yourself",
		emulatorList(), shell)
}

func emulatorList() string {
	var names []string
	for _, e := range terminalEmulators {
		names = append(names, e.bin)
	}
	return strings.Join(names, ", ")
}

// dialCheck verifies something is listening before we spawn a terminal.
func dialCheck(socket string) error {
	c, err := net.DialTimeout("unix", socket, 500*time.Millisecond)
	if err != nil {
		return fmt.Errorf("mpv-shim is not running: %w", err)
	}
	_ = c.Close()
	return nil
}

// shellQuote quotes a path for the shell.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		return `"` + s + `"`
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

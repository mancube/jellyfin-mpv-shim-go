// Package player wraps an mpv process (JSON IPC over a unix socket) and
// drives playback state: queue, timeline reports, crash recovery.
package player

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"

	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Mpv is the mpv control surface. Proc implements it against a real process;
// tests use a fake. All methods are safe for concurrent use.
type Mpv interface {
	EnsureRunning(ctx context.Context) error
	LoadFile(ctx context.Context, url string) error
	Stop() error
	SetProperty(name string, value any)
	GetProperty(name string) (any, error)
	SubAdd(url string) error
	ShowText(text string, ms, level int)
	Keybind(key, cmd string)   // bind a key to an mpv command ("" unbinds)
	Observe(name string) error // push property changes as property-change events
	Command(args ...any) error
	Screenshot(dir string) error
	Alive() bool
	Incarnation() int // spawn counter: changes when mpv (re)started
	Graceful() bool
	Exit() <-chan struct{}
	SetEventHook(func(name string, data json.RawMessage))
	Kill()
}

// mouseLua is the embedded OSD-menu mouse script (upstream mouse.lua).
//
//go:embed mouse.lua
var mouseLua []byte

// ProcOpts configures one mpv process.
type ProcOpts struct {
	Path       string // mpv binary
	IPCDir     string // dir for the unix socket (created)
	ConfigDir  string // mpv --config-dir; empty = mpv's default (the user's)
	AuthHeader string // Authorization header value, sent via --http-header-fields; empty disables
	MediaKeys  bool
	MenuMouse  bool   // reserved: the mouse script is always loaded
	LogLevel   string // mpv --msg-level=all=<level> ("" = mpv default)
	Log        *log.Logger
}

type rpcMsg struct {
	Event     string          `json:"event"`
	Data      json.RawMessage `json:"data"`
	Args      []string        `json:"args"` // client-message payload
	Name      string          `json:"name"` // property-change
	ID        *int64          `json:"id"`   // property-change
	RequestID *int64          `json:"request_id"`
	Error     json.RawMessage `json:"error"`
}

// Proc is a managed mpv process with JSON IPC. Port of upstream's
// python-mpv / python-mpv-jsonipc usage: spawn, probe, correlate
// request_ids, observe events, respawn on demand.
type Proc struct {
	path       string
	ipcDir     string
	cfgDir     string
	authHeader string
	mediaKeys  bool
	menuMouse  bool
	logLevel   string
	log        *log.Logger

	obsMu       sync.Mutex // guards observeIDs
	spawnMu     sync.Mutex // serializes respawns
	mu          sync.Mutex
	cmd         *exec.Cmd
	conn        net.Conn
	r           *bufio.Reader
	wmu         sync.Mutex // serializes writes to conn
	pending     map[int64]chan *rpcMsg
	nextID      int64
	death       chan struct{} // stable; monitor sends a token per death
	incarnation int
	graceful    bool
	exitClean   bool // mpv exited on request (not a crash)
	hook        func(name string, data json.RawMessage)
	dead        bool // Kill() called: never spawn again
}

func NewProc(o ProcOpts) *Proc {
	if o.Log == nil {
		o.Log = log.Default()
	}
	return &Proc{
		path: o.Path, ipcDir: o.IPCDir, cfgDir: o.ConfigDir,
		authHeader: o.AuthHeader, mediaKeys: o.MediaKeys, menuMouse: o.MenuMouse,
		logLevel: o.LogLevel, log: o.Log,
		pending: map[int64]chan *rpcMsg{},
		death:   make(chan struct{}, 1),
	}
}

// Exit returns the stable channel on which a token is sent whenever an mpv
// process dies. The channel is never replaced, so a waiter can't miss a
// death across respawns.
func (p *Proc) Exit() <-chan struct{} {
	return p.death
}

func (p *Proc) Alive() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conn != nil
}

func (p *Proc) Incarnation() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.incarnation
}

// Graceful reports whether mpv exited because someone asked it to (the
// `shutdown` event, a clean quit, or SIGTERM/SIGINT — which is what closing
// the player window does) rather than crashing (SIGKILL/SIGSEGV/OOM, or a
// non-zero exit). The player only respawns after a real crash.
func (p *Proc) Graceful() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.graceful || p.exitClean
}

func (p *Proc) SetEventHook(h func(name string, data json.RawMessage)) {
	p.mu.Lock()
	p.hook = h
	p.mu.Unlock()
}

// EnsureRunning starts mpv (if needed) and waits until the IPC socket
// answers. Idempotent; concurrent calls serialize on the spawn.
func (p *Proc) EnsureRunning(ctx context.Context) error {
	p.spawnMu.Lock()
	defer p.spawnMu.Unlock()
	p.mu.Lock()
	if p.dead {
		p.mu.Unlock()
		return errors.New("mpv: terminated")
	}
	if p.conn != nil {
		p.mu.Unlock()
		return nil
	}
	p.mu.Unlock()
	return p.spawn(ctx)
}

func (p *Proc) spawn(ctx context.Context) error {
	sock := filepath.Join(p.ipcDir, "shim.sock")
	_ = os.Remove(sock)
	args := []string{
		"--no-terminal",
		"--idle",                     // stay alive until the first loadfile; empty playlist would exit
		"--input-ipc-server=" + sock, // bare path; mpv v0.41 rejects unix: prefix
	}
	if p.cfgDir != "" {
		if err := os.MkdirAll(p.cfgDir, 0o700); err != nil {
			return err
		}
		args = append(args, "--config-dir="+p.cfgDir)
	}
	// The mouse script is always loaded and enabled/disabled at runtime through
	// the shim-menu-enable message, so `menu_mouse` can be toggled from the OSD
	// menu without restarting mpv (same design as upstream's mouse.lua).
	if script, err := p.mouseScript(); err == nil {
		args = append(args, "--script="+script)
	} else {
		p.log.Printf("mouse menu: %v", err)
	}
	if p.logLevel != "" {
		args = append(args, "--msg-level=all="+p.logLevel)
	}
	if p.mediaKeys {
		args = append(args, "--input-media-keys=yes")
	} else {
		args = append(args, "--input-media-keys=no")
	}
	if p.authHeader != "" {
		// mpv takes the whole header line, not Name=Value; the token rides in
		// the Authorization header, never in the URL.
		args = append(args, "--http-header-fields=Authorization: "+p.authHeader)
	}
	logF, err := os.Create(filepath.Join(p.ipcDir, "mpv.log"))
	if err != nil {
		return err
	}
	cmd := exec.Command(p.path, args...)
	cmd.Stdout = logF
	cmd.Stderr = logF
	stdin, err2 := os.Open(os.DevNull)
	if err2 == nil {
		cmd.Stdin = stdin
	}
	if err := cmd.Start(); err != nil {
		_ = logF.Close()
		return fmt.Errorf("mpv: %w", err)
	}
	_ = logF.Close() // the child holds its own dup

	p.mu.Lock()
	p.incarnation++
	id := p.incarnation
	p.cmd = cmd
	p.pending = map[int64]chan *rpcMsg{}
	p.nextID = 0
	p.graceful = false
	p.exitClean = false
	p.mu.Unlock()
	go p.monitor(cmd, id)

	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			p.killProcess(cmd)
			return ctx.Err()
		case <-p.death:
			// The buffer was empty when we spawned, so this token is ours.
			return errors.New("mpv: process exited during start")
		default:
		}
		conn, derr := net.DialTimeout("unix", sock, time.Second)
		if derr == nil {
			if r, ok := p.probe(conn); ok {
				// r already holds any read-ahead bytes, so keep it.
				p.mu.Lock()
				p.conn = conn
				p.r = r
				p.mu.Unlock()
				go p.readLoop()
				return nil
			}
			_ = conn.Close()
		}
		if time.Now().After(deadline) {
			p.killProcess(cmd)
			return errors.New("mpv: start timeout")
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// monitor reaps the process and signals death if it's the current
// incarnation.
func (p *Proc) monitor(cmd *exec.Cmd, id int) {
	werr := cmd.Wait()
	p.mu.Lock()
	if id != p.incarnation {
		p.mu.Unlock()
		return
	}
	p.conn = nil
	p.exitClean = exitedCleanly(werr)
	for pid, ch := range p.pending {
		b, _ := json.Marshal("mpv: process exited")
		ch <- &rpcMsg{RequestID: &pid, Error: b}
		delete(p.pending, pid)
	}
	p.mu.Unlock()
	select {
	case p.death <- struct{}{}:
	default: // coalesced; a token is already pending
	}
}

// exitedCleanly classifies a process exit: a clean quit, or SIGTERM/SIGINT
// (closing the player window) is "asked to stop"; a fatal signal (SIGKILL,
// SIGSEGV, SIGABRT…) or a non-zero exit status is a crash.
func exitedCleanly(err error) bool {
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		status, ok := ee.Sys().(syscall.WaitStatus)
		if ok && status.Signaled() {
			switch status.Signal() {
			case syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP:
				return true
			}
			return false
		}
		return false
	}
	return false
}

// probe writes a get_property and checks the reply. `pause` is used because
// it always exists; `version` was removed from mpv's property list (v0.41).
// It returns the reader so read-ahead bytes are not lost.
func (p *Proc) probe(conn net.Conn) (*bufio.Reader, bool) {
	b, _ := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{[]any{"get_property", "pause"}, -1})
	b = append(b, '\n')
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := conn.Write(b); err != nil {
		return nil, false
	}
	r := bufio.NewReader(conn)
	for i := 0; i < 16; i++ {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return nil, false
		}
		var m rpcMsg
		if json.Unmarshal(line, &m) == nil && m.RequestID != nil && *m.RequestID == -1 {
			// Lift the probe deadline so readLoop is not time-bombed 2s later.
			_ = conn.SetDeadline(time.Time{})
			return r, mpvError(m.Error) == nil
		}
	}
	return nil, false
}

// readLoop dispatches responses and events until the connection dies.
func (p *Proc) readLoop() {
	for {
		line, err := p.r.ReadBytes('\n')
		if len(line) > 0 {
			var m rpcMsg
			if json.Unmarshal(line, &m) == nil {
				if m.RequestID != nil {
					p.mu.Lock()
					ch := p.pending[*m.RequestID]
					if ch != nil {
						delete(p.pending, *m.RequestID)
					}
					p.mu.Unlock()
					if ch != nil {
						ch <- &m
					}
				} else if m.Event != "" {
					data := m.Data
					if m.Event == "property-change" && m.Name != "" {
						// Re-shape into {"name":…,"data":…} so the player hook
						// sees one uniform payload.
						b, _ := json.Marshal(struct {
							Name string          `json:"name"`
							Data json.RawMessage `json:"data"`
						}{m.Name, m.Data})
						data = b
					}
					if len(m.Args) > 0 {
						// client-message carries "args", not "data".
						b, _ := json.Marshal(m.Args)
						data = b
					}
					if m.Event == "shutdown" {
						p.mu.Lock()
						p.graceful = true
						p.mu.Unlock()
					}
					p.mu.Lock()
					hook := p.hook
					p.mu.Unlock()
					if hook != nil {
						hook(m.Event, data)
					}
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// command sends one IPC command and returns the response Data.
func (p *Proc) command(args ...any) (json.RawMessage, error) {
	p.mu.Lock()
	if p.conn == nil {
		p.mu.Unlock()
		return nil, errors.New("mpv: not running")
	}
	p.nextID++
	id := p.nextID
	ch := make(chan *rpcMsg, 1)
	p.pending[id] = ch
	w := p.conn // unbuffered: a buffered writer that is never flushed loses the command
	p.mu.Unlock()

	b, err := json.Marshal(struct {
		Command   []any `json:"command"`
		RequestID int64 `json:"request_id"`
	}{args, id})
	if err != nil {
		p.dropPending(id)
		return nil, err
	}
	b = append(b, '\n')
	p.wmu.Lock()
	_, err = w.Write(b)
	p.wmu.Unlock()
	if err != nil {
		p.dropPending(id)
		return nil, err
	}
	select {
	case m := <-ch:
		return m.Data, mpvError(m.Error)
	case <-time.After(10 * time.Second):
		p.dropPending(id)
		return nil, errors.New("mpv: command timeout")
	}
}

func (p *Proc) dropPending(id int64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func mpvError(raw json.RawMessage) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "success" {
			return nil
		}
		return errors.New("mpv: " + s)
	}
	var o struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &o) == nil && o.Error != "" {
		return errors.New("mpv: " + o.Error)
	}
	return nil
}

func (p *Proc) LoadFile(ctx context.Context, url string) error {
	// loadfile replace — port of python-mpv play()
	_, err := p.command("loadfile", url, "replace")
	return err
}

func (p *Proc) Stop() error {
	_, err := p.command("stop")
	return err
}

func (p *Proc) SetProperty(name string, value any) {
	if _, err := p.command("set_property", name, value); err != nil {
		p.log.Printf("mpv: set %s: %v", name, err)
	}
}

func (p *Proc) GetProperty(name string) (any, error) {
	data, err := p.command("get_property", name)
	if err != nil {
		return nil, err
	}
	if len(data) == 0 || string(data) == "null" {
		return nil, nil
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return nil, nil
	}
	return v, nil
}

// SubAdd loads an external subtitle and selects it. The "cached" flag makes
// re-adding the same file a re-select instead of stacking duplicate tracks
// (configureStreams runs on every play/restart/track switch).
func (p *Proc) SubAdd(url string) error {
	_, err := p.command("sub-add", url, "cached")
	return err
}

func (p *Proc) ShowText(text string, ms, level int) {
	if _, err := p.command("show-text", text, ms, level); err != nil {
		p.log.Printf("mpv: show-text: %v", err)
	}
}

// Keybind binds a key (input.conf naming: "c", "up", "escape") to a complete
// mpv command; an empty cmd removes it. This is mpv's documented client-API
// way to claim keys (upstream does the same via python-mpv's on_key_press).
func (p *Proc) Keybind(key, cmd string) {
	if _, err := p.command("keybind", key, cmd); err != nil {
		p.log.Printf("mpv: keybind %s: %v", key, err)
	}
}

// Command runs a raw mpv IPC command.
func (p *Proc) Command(args ...any) error {
	_, err := p.command(args...)
	return err
}

// observeIDs numbers the properties we watch, so a change event can be
// attributed; mpv echoes the id back.
var observeIDs = map[string]int64{
	"pause": 1, "mute": 2, "volume": 3, "seeking": 4, "time-pos": 5, "eof-reached": 6,
}

// mouseScript materialises mouse.lua into the IPC dir (mpv needs a real path).
func (p *Proc) mouseScript() (string, error) {
	name := filepath.Join(p.ipcDir, "mouse.lua")
	return name, os.WriteFile(name, mouseLua, 0o600)
}

// Observe subscribes to a property; changes arrive as
// property-change {"name":…,"data":…} events. This is how the player learns
// about state we did not cause (the web UI's play/pause, a seek bar drag) the
// moment it happens, instead of on the next 5 s tick.
func (p *Proc) Observe(name string) error {
	p.obsMu.Lock()
	id, ok := observeIDs[name]
	if !ok {
		id = int64(len(observeIDs) + 1)
		observeIDs[name] = id
	}
	p.obsMu.Unlock()
	_, err := p.command("observe_property", id, name) // note: id first
	return err
}

// Screenshot writes a video frame into dir (upstream's TakeScreenshot).
// mpv 0.41 dropped `screenshot-to-file` and the old flag/argument form: the
// destination comes from the screenshot-template option now.
func (p *Proc) Screenshot(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmpl := filepath.Join(dir, "shot-%03d.jpg")
	if _, err := p.command("set_property", "screenshot-template", tmpl); err != nil {
		return fmt.Errorf("screenshot-template: %w", err)
	}
	_, err := p.command("screenshot", "video")
	return err
}

// Kill terminates the process: SIGTERM, escalating to SIGKILL after 3s.
// The escalation runs async — Kill is the app-shutdown path and must not
// block.
func (p *Proc) Kill() {
	p.mu.Lock()
	cmd := p.cmd
	p.dead = true
	p.conn = nil
	p.mu.Unlock()
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	go func() {
		time.Sleep(3 * time.Second)
		_ = cmd.Process.Signal(syscall.SIGKILL)
	}()
}

func (p *Proc) killProcess(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

var _ Mpv = (*Proc)(nil)

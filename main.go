// mpv-shim: a minimal Jellyfin v12 cast client that plays through local mpv.
// See PLAN.md for scope and RESEARCH.md for protocol notes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"mpv-shim/jfin"
	"mpv-shim/player"
	"mpv-shim/ui"
)

var version = "0.1.0-dev" // overridden via -ldflags "-X main.version=..."

func main() {
	os.Exit(run())
}

func run() int {
	sub := ""
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("mpv-shim", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "mpv-shim %s — Jellyfin → mpv cast client\n\n", version)
		fmt.Fprintln(fs.Output(), "Usage:")
		fmt.Fprintln(fs.Output(), "  mpv-shim login <server> <username> <password>   log in and store credentials")
		fmt.Fprintln(fs.Output(), "  mpv-shim accounts [rm <index>]                  list/remove saved accounts")
		fmt.Fprintln(fs.Output(), "  mpv-shim setup                                 TUI: add/remove accounts (password or Quick Connect)")
		fmt.Fprintln(fs.Output(), "  mpv-shim                                       status (session loop from M2)")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "config file (default <config dir>/config.json)")
	server := fs.String("server", "", "server URL, e.g. http://localhost:8096")
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password")
	debug := fs.Bool("debug", false, "verbose logging")
	headless := fs.Bool("headless", false, "no TUI: log to stdout only (daemon mode)")
	loginOnly := fs.Bool("login-only", false, "log in and exit")
	statusOnly := fs.Bool("status", false, "print connection status and exit")
	showVersion := fs.Bool("version", false, "print version and exit")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *showVersion {
		fmt.Println("mpv-shim", version)
		return 0
	}
	log.SetFlags(0)
	if *debug {
		log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	}

	cfgDir, err := ConfigDir()
	if err != nil {
		log.Fatal(err)
	}
	if *configPath == "" {
		*configPath = filepath.Join(cfgDir, "config.json")
	}
	s := DefaultSettings()
	if err := s.Load(*configPath); err != nil {
		log.Fatalf("config: %v", err)
	}
	if *server != "" {
		s.Server = strings.TrimRight(strings.TrimSpace(*server), "/")
	}
	if *username != "" {
		s.Username = *username
	}
	if s.ClientUUID == "" {
		s.ClientUUID = newUUID()
	}
	credPath := jfin.CredPath(cfgDir)
	creds := &jfin.CredFile{}
	if err := creds.Load(credPath); err != nil {
		log.Fatalf("credentials: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	switch sub {
	case "login":
		pos := fs.Args()
		if len(pos) > 0 {
			s.Server = strings.TrimRight(strings.TrimSpace(pos[0]), "/")
		}
		if len(pos) > 1 {
			*username = pos[1]
		}
		if len(pos) > 2 {
			*password = pos[2]
		}
		if s.Server == "" || *username == "" || *password == "" {
			fmt.Fprintln(os.Stderr, "usage: mpv-shim login <server> <username> <password>")
			return 2
		}
		return doLogin(ctx, &s, *username, *password, creds, credPath, *configPath)
	case "accounts":
		return doAccounts(fs.Args(), creds, credPath)
	case "setup":
		return doSetup(&s, creds, credPath)
	case "":
		if *loginOnly {
			return doLogin(ctx, &s, *username, *password, creds, credPath, *configPath)
		}
		if *statusOnly {
			return doStatus(ctx, &s, creds)
		}
		a, ok := creds.ActiveAccount()
		if !ok {
			// First run: offer the wizard when there is a terminal, else point
			// at the CLI login.
			if !*headless && isTTY() {
				if rc := doSetup(&s, creds, credPath); rc != 0 {
					return rc
				}
				if a, ok = creds.ActiveAccount(); !ok {
					fmt.Fprintln(os.Stderr, "no account configured; run: mpv-shim setup")
					return 1
				}
			} else {
				fmt.Fprintln(os.Stderr, "no accounts configured. Run: mpv-shim login <server> <username> <password>")
				return 1
			}
		}
		return runSession(&s, a, creds, credPath, !*headless && isTTY())
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (expected login, accounts, setup)\n", sub)
		return 2
	}
}

func doLogin(ctx context.Context, s *Settings, username, password string, creds *jfin.CredFile, credPath, cfgPath string) int {
	if s.Server == "" || username == "" || password == "" {
		fmt.Fprintln(os.Stderr, "missing server/username/password (flags or login <url> <user> <pass>)")
		return 2
	}
	client := jfin.New(s.Server, s.PlayerName, s.ClientUUID, version, s.IgnoreSSL)
	resp, err := client.Login(ctx, username, password)
	if err != nil {
		log.Printf("login failed: %v", err)
		return 1
	}
	creds.Upsert(jfin.Account{
		Server:      s.Server,
		Username:    username,
		User:        resp.User.Name,
		AccessToken: client.Token,
		UserID:      client.UserID,
		DeviceID:    s.ClientUUID,
	})
	creds.SetActive(s.Server, username)
	if err := creds.Save(credPath); err != nil {
		log.Printf("saving credentials: %v", err)
		return 1
	}
	s.Username = username
	if err := s.Save(cfgPath); err != nil {
		log.Printf("saving config: %v", err)
	}
	log.Printf("logged in to %s as %q (%s), credentials in %s", s.Server, resp.User.Name, s.Server, credPath)
	return 0
}

func doStatus(ctx context.Context, s *Settings, creds *jfin.CredFile) int {
	a, ok := creds.ActiveAccount()
	if !ok {
		fmt.Fprintln(os.Stderr, "no accounts configured. Run: mpv-shim login <server> <username> <password>")
		return 1
	}
	client := jfin.New(a.Server, s.PlayerName, a.DeviceID, version, s.IgnoreSSL)
	client.Token, client.UserID = a.AccessToken, a.UserID
	var u jfin.User
	if err := client.Get(ctx, "/Users/"+url.PathEscape(a.UserID), &u); err != nil {
		log.Printf("server unreachable or token expired: %v", err)
		log.Printf("re-login: mpv-shim login %s %s <password>", a.Server, a.Username)
		return 1
	}
	fmt.Printf("server:  %s\n", a.Server)
	fmt.Printf("user:    %s (%s)\n", u.Name, a.Username)
	fmt.Printf("device:  %s [%s]\n", s.PlayerName, a.DeviceID)
	fmt.Printf("version: %s\n", version)
	return 0
}

// session bundles the live objects: REST client, mpv, player and the
// /socket connection. The TUI/tray only read from it.
type session struct {
	account jfin.Account
	client  *jfin.Client
	proc    *player.Proc
	pl      *player.Player
	ws      *jfin.WS
	logs    *ui.LogRing
	ipcDir  string
	lg      *log.Logger
	cancel  context.CancelFunc
}

// newSession wires the client, mpv, player and WS event handlers.
func newSession(s *Settings, a jfin.Account, lg *log.Logger, logs *ui.LogRing) (*session, error) {
	client := jfin.New(a.Server, s.PlayerName, a.DeviceID, version, s.IgnoreSSL)
	client.Token, client.UserID = a.AccessToken, a.UserID

	// The mpv IPC socket needs a world-safe dir; it's removed on exit.
	ipcDir, err := os.MkdirTemp("", "mpv-shim-*")
	if err != nil {
		return nil, fmt.Errorf("ipc dir: %w", err)
	}
	proc := player.NewProc(player.ProcOpts{
		Path:       s.MpvPath,
		IPCDir:     ipcDir,
		ConfigDir:  s.MpvConfigDir,
		AuthHeader: client.AuthHeader(),
		MediaKeys:  s.MediaKeys,
		Log:        lg,
	})
	pl := player.New(proc, lg)

	mcfg := jfin.MediaConfig{
		LocalKbps: s.LocalKbps, RemoteKbps: s.RemoteKbps,
		TranscodeH265: s.TranscodeH265, ForceH264: s.ForceH264,
		SkipIntro: s.SkipIntro, SkipCredits: s.SkipCredits,
	}

	ws := jfin.NewWS(client, lg)
	ws.On("Play", func(ctx context.Context, data json.RawMessage) {
		go handlePlay(ctx, client, pl, mcfg, data) // don't block the WS read loop
	})
	// v12's remote-control API (POST /Sessions/{id}/Command) delivers play
	// commands as GeneralCommand {Name, Arguments}; the web UI cast path uses
	// the "Play" message above. Route the play-ish ones into handlePlay.
	ws.On("GeneralCommand", func(ctx context.Context, data json.RawMessage) {
		var d struct {
			Name      string          `json:"Name"`
			Arguments json.RawMessage `json:"Arguments"`
		}
		if json.Unmarshal(data, &d) != nil {
			return
		}
		var pr jfin.PlayRequest
		switch d.Name {
		case "PlayNow", "PlayNext", "PlayLast", "PlayInstantMix":
			var args struct {
				Item    string   `json:"Item"`
				ItemIDs []string `json:"ItemIds"`
			}
			if json.Unmarshal(d.Arguments, &args) == nil {
				if args.Item != "" {
					args.ItemIDs = append(args.ItemIDs, args.Item)
				}
				pr = jfin.PlayRequest{PlayCommand: d.Name, ItemIDs: args.ItemIDs}
			}
		default:
			// Everything else is remote control of the current playback.
			// Not blocking the WS read loop: menu actions do IPC round-trips.
			go handleGeneralCommand(pl, d.Name, d.Arguments)
			return
		}
		b, _ := json.Marshal(pr)
		go handlePlay(ctx, client, pl, mcfg, b)
	})
	ws.On("Playstate", func(ctx context.Context, data json.RawMessage) {
		go handlePlaystate(pl, data)
	})

	return &session{account: a, client: client, proc: proc, pl: pl, ws: ws, logs: logs, ipcDir: ipcDir, lg: lg}, nil
}

// run keeps the /socket connection alive until ctx is canceled, then tears
// playback down and kills mpv (upstream mpv_shim.py shutdown order).
func (sess *session) run(ctx context.Context) {
	ctx, sess.cancel = context.WithCancel(ctx)
	defer sess.cancel()
	sess.pl.Start(ctx)
	defer sess.pl.Shutdown()
	defer os.RemoveAll(sess.ipcDir)
	sess.lg.Printf("mpv-shim %s — server %s, user %s, device %s", version, sess.account.Server, sess.account.Username, sess.account.DeviceID)
	sess.lg.Printf("session loop running, Ctrl-C to quit")
	if err := sess.ws.Run(ctx); err != nil && ctx.Err() == nil {
		sess.lg.Printf("session loop ended: %v", err)
	}
	sess.lg.Printf("bye")
}

// runSession is the whole app for one account: the session loop plus, when
// there is a terminal, the TUI status screen and the desktop tray.
func runSession(s *Settings, a jfin.Account, creds *jfin.CredFile, credPath string, interactive bool) int {
	ring := ui.NewLogRing(200)
	lg := log.New(io.MultiWriter(ring, os.Stderr), "", log.Flags())
	sess, err := newSession(s, a, lg, ring)
	if err != nil {
		lg.Printf("%v", err)
		return 1
	}

	sigCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sess.run(sigCtx)

	if !interactive {
		<-sigCtx.Done() // headless: logs only (daemon mode)
		return 0
	}

	quitOnce := sync.Once{}
	uiSess := &ui.Session{
		Account: a, WS: sess.ws, Player: sess.pl, Logs: ring,
		Quit: func() { quitOnce.Do(func() { stop() }) },
	}
	if ok := ui.RunTray(uiSess); !ok {
		lg.Printf("tray: no system tray host found (GNOME needs the AppIndicator extension); the TUI is the full surface")
	}
	deps := ui.Deps{
		Creds:    creds,
		CredPath: credPath,
		NewClient: func(server string) *jfin.Client {
			return jfin.New(server, s.PlayerName, s.ClientUUID, version, s.IgnoreSSL)
		},
	}
	_ = ui.RunStatus(uiSess, deps)
	stop()
	return 0
}

// handlePlay is the WS "Play" event: build the queue (PlayNow) or extend it
// (PlayNext/PlayLast) and drive the player. Port of upstream onPlay.
func handlePlay(ctx context.Context, client *jfin.Client, pl *player.Player, cfg jfin.MediaConfig, data json.RawMessage) {
	var d jfin.PlayRequest
	if err := json.Unmarshal(data, &d); err != nil {
		log.Printf("play: bad event: %v", err)
		return
	}
	if len(d.ItemIDs) == 0 {
		return
	}
	seq := 0
	if d.StartIndex != nil {
		seq = *d.StartIndex
	}
	offset := 0.0
	if d.StartPositionTicks != nil {
		offset = float64(*d.StartPositionTicks) / 1e7
	}
	cmd := d.PlayCommand
	if !pl.HasVideo() && cmd != "PlayNow" {
		log.Printf("play: %s with no active playlist, treating as PlayNow", cmd)
		cmd = "PlayNow"
	}
	switch cmd {
	case "PlayNow":
		m, err := jfin.NewMedia(ctx, client, cfg, d.ItemIDs, seq, d.ControllingUserID,
			d.AudioStreamIndex, d.SubtitleStreamIndex, d.MediaSourceID)
		if err != nil {
			log.Printf("play: %v", err)
			return
		}
		if err := pl.Play(m, offset); err != nil {
			log.Printf("play: %v", err)
		}
	case "PlayNext":
		pl.InsertQueue(d.ItemIDs, false)
	case "PlayLast":
		pl.InsertQueue(d.ItemIDs, true)
	default:
		log.Printf("play: unknown command %q", cmd)
	}
}

// handlePlaystate is the WS "Playstate" event: the transport controls
// (play/pause, seek, next/prev, stop). Port of upstream
// event_handler.play_state.
func handlePlaystate(pl *player.Player, data json.RawMessage) {
	var d struct {
		Command           string `json:"Command"`
		SeekPositionTicks *int64 `json:"SeekPositionTicks"`
	}
	if json.Unmarshal(data, &d) != nil {
		return
	}
	switch d.Command {
	case "PlayPause":
		pl.TogglePause()
	case "Pause":
		pl.SetPaused(true)
	case "Unpause":
		pl.SetPaused(false)
	case "PreviousTrack":
		pl.Prev()
	case "NextTrack":
		pl.Next()
	case "Stop":
		pl.Stop()
	case "Seek":
		if d.SeekPositionTicks != nil {
			pl.Seek(float64(*d.SeekPositionTicks)/1e7, true)
		}
	default:
		log.Printf("playstate: unknown command %q", d.Command)
	}
}

// navigation maps the remote's navigation commands to shim key actions
// (upstream event_handler.NAVIGATION_DICT).
var navigation = map[string]string{
	"Back":         "back",
	"Select":       "ok",
	"MoveUp":       "up",
	"MoveDown":     "down",
	"MoveRight":    "right",
	"MoveLeft":     "left",
	"GoHome":       "home",
	"GoToSettings": "home",
}

// handleGeneralCommand is the WS "GeneralCommand" remote-control surface:
// volume, mute, track selection, fullscreen and menu navigation. Port of
// upstream event_handler.general_command.
func handleGeneralCommand(pl *player.Player, name string, args json.RawMessage) {
	if action, ok := navigation[name]; ok {
		pl.Key(action)
		return
	}
	var a struct {
		Volume *int `json:"Volume"`
		Index  *int `json:"Index"`
	}
	_ = json.Unmarshal(args, &a)
	switch name {
	case "SetVolume":
		if a.Volume != nil {
			pl.SetVolume(*a.Volume)
		}
	case "Mute":
		pl.SetMute(true)
	case "Unmute":
		pl.SetMute(false)
	case "SetAudioStreamIndex":
		if a.Index != nil {
			pl.SetStreams(a.Index, nil)
		}
	case "SetSubtitleStreamIndex":
		if a.Index != nil {
			pl.SetStreams(nil, a.Index)
		}
	case "ToggleFullscreen", "":
		pl.ToggleFullscreen()
	case "DisplayContent":
		// Nothing to mirror (out of scope); ignore.
	default:
		log.Printf("general command: unhandled %q", name)
	}
}

// doSetup runs the TUI account wizard.
func doSetup(s *Settings, creds *jfin.CredFile, credPath string) int {
	if !isTTY() {
		fmt.Fprintln(os.Stderr, "setup needs a terminal; use: mpv-shim login <server> <username> <password>")
		return 2
	}
	deps := ui.Deps{
		Creds:    creds,
		CredPath: credPath,
		NewClient: func(server string) *jfin.Client {
			return jfin.New(server, s.PlayerName, s.ClientUUID, version, s.IgnoreSSL)
		},
	}
	return intErr(ui.RunSetup(deps))
}

// isTTY reports whether stdout is a terminal (so the TUI can take it over).
func isTTY() bool {
	fi, err := os.Stdout.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func intErr(err error) int {
	if err != nil {
		fmt.Fprintf(os.Stderr, "tui: %v\n", err)
		return 1
	}
	return 0
}

func doAccounts(args []string, creds *jfin.CredFile, credPath string) int {
	if len(args) >= 2 && args[0] == "rm" {
		idx, err := strconv.Atoi(args[1])
		if err != nil || idx < 0 || idx >= len(creds.Accounts) {
			fmt.Fprintf(os.Stderr, "usage: mpv-shim accounts rm <index 0..%d>\n", len(creds.Accounts)-1)
			return 2
		}
		a := creds.Accounts[idx]
		creds.Remove(a.Server, a.Username)
		if err := creds.Save(credPath); err != nil {
			log.Printf("saving credentials: %v", err)
			return 1
		}
		fmt.Printf("removed %s (%s)\n", a.Server, a.Username)
		return 0
	}
	if len(creds.Accounts) == 0 {
		fmt.Println("no accounts. Add one: mpv-shim login <server> <username> <password>")
		return 0
	}
	for i, a := range creds.Accounts {
		marker := " "
		if i == creds.Active {
			marker = "*"
		}
		fmt.Printf("%c %d: %s  %s (%s)\n", marker[0], i, a.Server, a.Username, a.User)
	}
	return 0
}

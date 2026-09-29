// mpv-shim: a minimal Jellyfin v12 cast client that plays through local mpv.
// See PLAN.md for scope and RESEARCH.md for protocol notes.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mpv-shim/jfin"
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
		fmt.Fprintln(fs.Output(), "  mpv-shim                                       status (session loop from M2)")
		fmt.Fprintln(fs.Output(), "\nFlags:")
		fs.PrintDefaults()
	}
	configPath := fs.String("config", "", "config file (default <config dir>/config.json)")
	server := fs.String("server", "", "server URL, e.g. http://localhost:8096")
	username := fs.String("username", "", "username")
	password := fs.String("password", "", "password")
	debug := fs.Bool("debug", false, "verbose logging")
	loginOnly := fs.Bool("login-only", false, "log in and exit")
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
	case "":
		if *loginOnly {
			return doLogin(ctx, &s, *username, *password, creds, credPath, *configPath)
		}
		return doStatus(ctx, &s, creds)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (expected login, accounts)\n", sub)
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

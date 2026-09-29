package main

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Settings is the on-disk configuration (config.json). Keys map 1:1 to upstream
// jellyfin-mpv-shim settings where one exists (PLAN.md §4).
type Settings struct {
	Server        string `json:"server"` // e.g. http://localhost:8096
	Username      string `json:"username"`
	PlayerName    string `json:"player_name"`    // device name in the Jellyfin UI (default: hostname)
	ClientUUID    string `json:"client_uuid"`    // stable device id, generated on first run
	MpvPath       string `json:"mpv_path"`       // empty = "mpv" from PATH
	MpvConfigDir  string `json:"mpv_config_dir"` // empty = use the user's own mpv config dir
	LocalKbps     int    `json:"local_kbps"`
	RemoteKbps    int    `json:"remote_kbps"`
	TranscodeH265 bool   `json:"transcode_h265"`
	ForceH264     bool   `json:"force_h264"`
	SkipIntro     bool   `json:"skip_intro"`
	SkipCredits   bool   `json:"skip_credits"`
	IdleStop      bool   `json:"idle_stop"`
	IdleDelayS    int    `json:"idle_delay_s"`
	PauseReport   bool   `json:"pause_report"`
	IgnoreSSL     bool   `json:"ignore_ssl"`
	LogLevel      string `json:"log_level"`
	WriteLog      bool   `json:"write_log"`
	MediaKeys     bool   `json:"media_keys"`
}

func DefaultSettings() Settings {
	return Settings{
		PlayerName:  hostName(),
		MpvPath:     "mpv",
		LocalKbps:   10000,
		RemoteKbps:  25000,
		IdleStop:    true,
		IdleDelayS:  3600,
		PauseReport: true,
		LogLevel:    "info",
		MediaKeys:   true,
	}
}

// Load reads settings from path, preserving defaults for absent keys.
// A missing file is not an error (first run).
func (s *Settings) Load(path string) error {
	*s = DefaultSettings()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return err
	}
	s.Server = strings.TrimRight(strings.TrimSpace(s.Server), "/")
	// Migration: "mpv" was the old default device name; configs written by
	// older builds have it stored, so switch those to the hostname (nobody
	// deliberately names their device "mpv") and write it back.
	if s.PlayerName == "mpv" {
		s.PlayerName = hostName()
		_ = s.Save(path)
	}
	return nil
}

func (s *Settings) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}

// ConfigDir is the per-platform data dir:
// ~/.config/mpv-shim (Linux/XDG), %appdata%\mpv-shim (Windows),
// ~/Library/Application Support/mpv-shim (macOS).
func ConfigDir() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("APPDATA")
		if base == "" {
			return "", errors.New("APPDATA not set")
		}
		return filepath.Join(base, "mpv-shim"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, "Library", "Application Support", "mpv-shim"), nil
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
			return filepath.Join(x, "mpv-shim"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, ".config", "mpv-shim"), nil
	}
}

// hostName is the default device name, so the machine shows up in the
// Jellyfin UI as itself ("livingroom-pc") rather than a generic "mpv".
func hostName() string {
	if h, err := os.Hostname(); err == nil {
		if h = strings.TrimSpace(h); h != "" {
			return h
		}
	}
	return "mpv"
}

func newUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return ""
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

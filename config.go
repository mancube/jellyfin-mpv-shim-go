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

	// --- keybindings (upstream kb_*) ------------------------------------
	// Each entry maps an mpv key name to a shim action, e.g. {"m": "menu"}.
	KeyBindings map[string]string `json:"key_bindings,omitempty"`

	// --- seeking --------------------------------------------------------
	SeekUp       float64 `json:"seek_up"`        // seconds for the up arrow (60)
	SeekDown     float64 `json:"seek_down"`      // -60
	SeekLeft     float64 `json:"seek_left"`      // -5
	SeekRight    float64 `json:"seek_right"`     // 5
	SeekHExact   bool    `json:"seek_h_exact"`   // keyframe-exact horizontal seeks
	SeekVExact   bool    `json:"seek_v_exact"`   // keyframe-exact vertical seeks
	UseWebSeek   bool    `json:"use_web_seek"`   // use the remote's skip lengths
	MediaKeySeek bool    `json:"media_key_seek"` // media keys seek instead of skipping

	// --- playback -------------------------------------------------------
	AutoPlay         bool `json:"auto_play"`
	Fullscreen       bool `json:"fullscreen"`
	RaiseMPV         bool `json:"raise_mpv"`
	EnableOSC        bool `json:"enable_osc"`
	PlaybackTimeoutS int  `json:"playback_timeout"` // seconds waiting for duration
	ForceSetPlayed   bool `json:"force_set_played"`

	// --- subtitles ------------------------------------------------------
	SubtitleSize     int    `json:"subtitle_size"`     // percent, 100 = normal
	SubtitleColor    string `json:"subtitle_color"`    // #RRGGBBAA
	SubtitlePosition string `json:"subtitle_position"` // bottom | top | middle

	// --- paths ----------------------------------------------------------
	DirectPaths       bool              `json:"direct_paths"`
	RemoteDirectPaths bool              `json:"remote_direct_paths"`
	PathSubstitutions map[string]string `json:"path_substitutions,omitempty"`

	// --- device profile knobs -------------------------------------------
	AlwaysTranscode      bool   `json:"always_transcode"`
	TranscodeHi10p       bool   `json:"transcode_hi10p"`
	TranscodeHDR         bool   `json:"transcode_hdr"`
	TranscodeDolbyVision bool   `json:"transcode_dolby_vision"`
	ForceVideoCodec      string `json:"force_video_codec,omitempty"`
	ForceAudioCodec      string `json:"force_audio_codec,omitempty"`

	// --- tracks ---------------------------------------------------------
	LangFilterAudio string         `json:"lang_filter_audio,omitempty"` // comma list
	LangFilterSub   string         `json:"lang_filter_sub,omitempty"`
	LanguageConfig  []LanguageRule `json:"language_config,omitempty"`

	// --- misc -----------------------------------------------------------
	SkipIntroAlways   bool   `json:"skip_intro_always"`
	SkipCreditsAlways bool   `json:"skip_credits_always"`
	MenuMouse         bool   `json:"menu_mouse"`
	ScreenshotDir     string `json:"screenshot_dir,omitempty"`
	SanitizeOutput    bool   `json:"sanitize_output"`
	CheckUpdates      bool   `json:"check_updates"`
	NotifyUpdates     bool   `json:"notify_updates"`
	UpdateURL         string `json:"update_url,omitempty"`
	HealthCheckS      int    `json:"health_check_interval"` // 0 disables
	ConnectRetryMins  int    `json:"connect_retry_mins"`    // 0 = retry forever
	IdleCmdDelayS     int    `json:"idle_cmd_delay"`
	MediaEndedCmd     string `json:"media_ended_cmd,omitempty"`
	IdleCmd           string `json:"idle_cmd,omitempty"`
	IdleEndedCmd      string `json:"idle_ended_cmd,omitempty"`
	PlayCmd           string `json:"play_cmd,omitempty"`
	PreMediaCmd       string `json:"pre_media_cmd,omitempty"`
	StopCmd           string `json:"stop_cmd,omitempty"`
}

// LanguageRule is one ordered auto-track rule (upstream language_config): the
// first rule whose constraints all match decides the audio/subtitle track.
type LanguageRule struct {
	AudioLang string `json:"audio_lang,omitempty"`
	SubLang   string `json:"sub_lang,omitempty"`
	AudioNone bool   `json:"audio_none,omitempty"`
	SubNone   bool   `json:"sub_none,omitempty"`
	Enabled   bool   `json:"enabled"`
	Priority  int    `json:"priority,omitempty"`
	Note      string `json:"note,omitempty"`
}

func DefaultSettings() Settings {
	return Settings{
		PlayerName:           hostName(),
		SeekUp:               60,
		SeekDown:             -60,
		SeekLeft:             -5,
		SeekRight:            5,
		AutoPlay:             true,
		Fullscreen:           true,
		RaiseMPV:             true,
		EnableOSC:            true,
		PlaybackTimeoutS:     30,
		SubtitleSize:         100,
		SubtitleColor:        "#FFFFFFFF",
		SubtitlePosition:     "bottom",
		TranscodeDolbyVision: true,
		MenuMouse:            true,
		SanitizeOutput:       true,
		CheckUpdates:         true,
		NotifyUpdates:        true,
		HealthCheckS:         300,
		IdleCmdDelayS:        60,
		MpvPath:              "mpv",
		LocalKbps:            10000,
		RemoteKbps:           25000,
		IdleStop:             true,
		IdleDelayS:           3600,
		PauseReport:          true,
		LogLevel:             "info",
		MediaKeys:            true,
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

// --- playback behaviour -------------------------------------------------

// Keys is the keybinding table: mpv key name -> shim action. Empty means the
// built-in default (see player.DefaultKeyBindings).
func (s *Settings) Keys() map[string]string {
	if len(s.KeyBindings) == 0 {
		return nil
	}
	out := make(map[string]string, len(s.KeyBindings))
	for k, v := range s.KeyBindings {
		out[k] = v
	}
	return out
}

// SubPosition maps our setting to mpv's sub-pos value (upstream
// SUBTITLE_POS).
func (s *Settings) SubPosition() string {
	switch s.SubtitlePosition {
	case "top", "middle":
		return s.SubtitlePosition
	default:
		return "bottom"
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

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRoundTrip(t *testing.T) {
	s := DefaultSettings()
	if s.LocalKbps != 10000 || s.RemoteKbps != 25000 {
		t.Fatalf("defaults = %+v", s)
	}
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	s.Server = "http://localhost:8096/"
	s.Username = "admin"
	s.RemoteKbps = 9999
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	var loaded Settings
	if err := loaded.Load(path); err != nil {
		t.Fatal(err)
	}
	if loaded.Server != "http://localhost:8096" {
		t.Errorf("server not normalized: %q", loaded.Server)
	}
	if loaded.Username != "admin" || loaded.RemoteKbps != 9999 {
		t.Errorf("saved values lost: %+v", loaded)
	}
	if loaded.LocalKbps != 10000 {
		t.Errorf("defaults not preserved: %+v", loaded)
	}
}

func TestSettingsLoadMissingFile(t *testing.T) {
	var s Settings
	if err := s.Load(filepath.Join(t.TempDir(), "nope.json")); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if s.PlayerName != hostName() {
		t.Errorf("defaults not applied: %+v", s)
	}
}

func TestNewUUID(t *testing.T) {
	a, b := newUUID(), newUUID()
	if len(a) != 36 || a == b {
		t.Fatalf("uuids = %q, %q", a, b)
	}
	if a[14] != '4' || !strings.ContainsAny(string(a[19]), "89ab") {
		t.Errorf("not a v4 uuid: %q", a)
	}
}

// A config written by an older build stored the old "mpv" default; loading it
// migrates the device name to the hostname.
func TestSettingsMigratesOldPlayerName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"server":"http://x","player_name":"mpv","client_uuid":"u"}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	var s Settings
	if err := s.Load(path); err != nil {
		t.Fatal(err)
	}
	if s.PlayerName != hostName() {
		t.Errorf("PlayerName = %q, want %q", s.PlayerName, hostName())
	}
	// Migrated value is persisted, so the web UI shows the new name.
	var reread Settings
	if err := reread.Load(path); err != nil {
		t.Fatal(err)
	}
	if reread.PlayerName != hostName() {
		t.Errorf("migration not persisted: %q", reread.PlayerName)
	}
	// An explicit name is left alone.
	path2 := filepath.Join(dir, "config2.json")
	if err := os.WriteFile(path2, []byte(`{"player_name":"livingroom"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var s2 Settings
	if err := s2.Load(path2); err != nil {
		t.Fatal(err)
	}
	if s2.PlayerName != "livingroom" {
		t.Errorf("explicit name overwritten: %q", s2.PlayerName)
	}
}

// Every new setting round-trips through config.json, and the runtime helpers
// (Keys, SubPosition) behave as the player expects.
func TestNewSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	s := DefaultSettings()
	s.SeekLeft, s.SeekRight = -15, 30
	s.SeekHExact = true
	s.KeyBindings = map[string]string{"c": "fullscreen", "x": "menu"}
	s.SubtitleSize, s.SubtitleColor, s.SubtitlePosition = 125, "#FFEE00EE", "top"
	s.AutoPlay, s.Fullscreen, s.EnableOSC = false, false, false
	s.SkipIntroAlways = true
	s.PathSubstitutions = map[string]string{"/data": "/mnt/nas"}
	s.LanguageConfig = []LanguageRule{{AudioLang: "eng", Enabled: true, Priority: 5}}
	s.TranscodeHDR = true
	s.MediaEndedCmd = "notify-send done"
	s.UpdateURL = "https://example/api"
	if err := s.Save(path); err != nil {
		t.Fatal(err)
	}
	var got Settings
	if err := got.Load(path); err != nil {
		t.Fatal(err)
	}
	if got.SeekLeft != -15 || got.SeekRight != 30 || !got.SeekHExact {
		t.Errorf("seek settings lost: %+v", got)
	}
	if keys := got.Keys(); keys["c"] != "fullscreen" || keys["x"] != "menu" {
		t.Errorf("key bindings lost: %v", keys)
	}
	if got.SubtitleSize != 125 || got.SubtitleColor != "#FFEE00EE" || got.SubPosition() != "top" {
		t.Errorf("subtitle settings lost: %d %q %q", got.SubtitleSize, got.SubtitleColor, got.SubPosition())
	}
	if got.AutoPlay || got.Fullscreen || got.EnableOSC {
		t.Error("explicitly disabled toggles came back on")
	}
	if !got.SkipIntroAlways || !got.TranscodeHDR {
		t.Error("skip/transcode flags lost")
	}
	if got.PathSubstitutions["/data"] != "/mnt/nas" {
		t.Errorf("path substitutions lost: %v", got.PathSubstitutions)
	}
	if len(got.LanguageConfig) != 1 || got.LanguageConfig[0].AudioLang != "eng" || got.LanguageConfig[0].Priority != 5 {
		t.Errorf("language rules lost: %+v", got.LanguageConfig)
	}
	if got.MediaEndedCmd != "notify-send done" || got.UpdateURL != "https://example/api" {
		t.Errorf("hooks lost: %q %q", got.MediaEndedCmd, got.UpdateURL)
	}

	// A config without the new keys still gets sensible defaults.
	var fresh Settings
	if err := fresh.Load(filepath.Join(dir, "missing.json")); err != nil {
		t.Fatal(err)
	}
	if fresh.SeekUp != 60 || fresh.SeekRight != 5 || fresh.PlaybackTimeoutS != 30 ||
		fresh.SubtitleSize != 100 || !fresh.AutoPlay || !fresh.EnableOSC || !fresh.MenuMouse {
		t.Errorf("defaults for the new settings = %+v", fresh)
	}
	if fresh.Keys() != nil {
		t.Errorf("no key bindings should mean nil (use the defaults), got %v", fresh.Keys())
	}
	if fresh.SubPosition() != "bottom" {
		t.Errorf("SubPosition default = %q", fresh.SubPosition())
	}
}

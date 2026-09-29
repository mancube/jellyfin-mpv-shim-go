package main

import (
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
	if s.PlayerName != "mpv" {
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

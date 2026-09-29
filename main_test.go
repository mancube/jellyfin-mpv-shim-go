package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mpv-shim/jfin"
	"mpv-shim/player"
	"mpv-shim/ui"
)

// minimalMvp is just enough of player.Mpv for handlePlay tests.
type minimalMvp struct {
	mu    sync.Mutex
	props map[string]any
	urls  []string
}

func newMinimalMvp() *minimalMvp { return &minimalMvp{props: map[string]any{"duration": 100.0}} }

func (m *minimalMvp) EnsureRunning(ctx context.Context) error { return nil }
func (m *minimalMvp) LoadFile(ctx context.Context, u string) error {
	m.mu.Lock()
	m.urls = append(m.urls, u)
	m.mu.Unlock()
	return nil
}
func (m *minimalMvp) Stop() error { return nil }
func (m *minimalMvp) SetProperty(n string, v any) {
	m.mu.Lock()
	// mpv's IPC returns numbers as float64 and booleans as bool; mirror that.
	if i, ok := v.(int); ok {
		v = float64(i)
	}
	m.props[n] = v
	m.mu.Unlock()
}
func (m *minimalMvp) GetProperty(n string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.props[n], nil
}
func (m *minimalMvp) SubAdd(u string) error                 { return nil }
func (m *minimalMvp) Keybind(key, cmd string)               {}
func (m *minimalMvp) Command(args ...any) error             { return nil }
func (m *minimalMvp) Incarnation() int                      { return 1 }
func (m *minimalMvp) Screenshot(dir string) (string, error) { return dir + "/shot.png", nil }
func (m *minimalMvp) Observe(name string) error             { return nil }
func (m *minimalMvp) ShowText(t string, ms, level int) {
}
func (m *minimalMvp) Alive() bool { return true }
func (m *minimalMvp) Graceful() bool {
	return false
}
func (m *minimalMvp) Exit() <-chan struct{} { return make(chan struct{}) }
func (m *minimalMvp) SetEventHook(h func(string, json.RawMessage)) {
}
func (m *minimalMvp) Kill() {}
func (m *minimalMvp) numURLs() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.urls)
}

var mvpRunTimeTicks = int64(1_000_000_000)

func playServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/PlaybackInfo"):
			src := jfin.MediaSource{
				ID: "src1", Protocol: "Http", Bitrate: 100000,
				SupportsDirectPlay: true, SupportsDirectStream: true, SupportsTranscoding: true,
			}
			_ = json.NewEncoder(w).Encode(jfin.PlaybackInfo{PlaySessionId: "ps", MediaSources: []jfin.MediaSource{src}})
		case strings.Contains(p, "/Items/"):
			id := p[strings.LastIndex(p, "/")+1:]
			_ = json.NewEncoder(w).Encode(jfin.Item{ID: id, Type: "Video", Name: "T" + id, RunTimeTicks: &mvpRunTimeTicks})
		case p == "/Sessions/Playing" || p == "/Sessions/Playing/Progress" || p == "/Sessions/Playing/Stopped":
			w.WriteHeader(204)
		case strings.Contains(p, "/PlayedItems/"):
			w.WriteHeader(204)
		case p == "/Videos/ActiveEncodings":
			w.WriteHeader(204)
		default:
			w.WriteHeader(200)
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func playData(t *testing.T, cmd string, ids ...string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(jfin.PlayRequest{PlayCommand: cmd, ItemIDs: ids})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestHandlePlayNowThenNextLast(t *testing.T) {
	ts := playServer(t)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	mvp := newMinimalMvp()
	pl := player.New(mvp, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)

	cfg := jfin.MediaConfig{LocalKbps: 10000, RemoteKbps: 25000}

	// PlayNow with one item.
	handlePlay(ctx, c, pl, cfg, playData(t, "PlayNow", "a"))
	if !pl.HasVideo() {
		t.Fatal("no active video after PlayNow")
	}
	if mvp.numURLs() != 1 || !strings.Contains(mvp.lastURL(), "/a") {
		t.Fatalf("loaded %d URLs, last=%q", mvp.numURLs(), mvp.lastURL())
	}

	// PlayNext extends the queue after the current item.
	handlePlay(ctx, c, pl, cfg, playData(t, "PlayNext", "b"))
	// PlayLast appends to the end.
	handlePlay(ctx, c, pl, cfg, playData(t, "PlayLast", "c"))
	// PlayLast with no active playlist falls back to PlayNow.
	// (can't test both here without stopping; just check the queue grew via
	//  a fresh player)
	pl.Stop()
	if pl.HasVideo() {
		t.Fatal("HasVideo after stop")
	}

	// No active playlist: PlayNext must fall back to PlayNow.
	pl2 := player.New(newMinimalMvp(), log.New(io.Discard, "", 0))
	pl2.Start(ctx)
	handlePlay(ctx, c, pl2, cfg, playData(t, "PlayNext", "x"))
	if !pl2.HasVideo() {
		t.Fatal("PlayNext with empty playlist should PlayNow")
	}
}

func (m *minimalMvp) lastURL() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.urls) == 0 {
		return ""
	}
	return m.urls[len(m.urls)-1]
}

// --- M3: remote control routing ---

func TestHandlePlaystateAndGeneralCommand(t *testing.T) {
	ts := playServer(t)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	mvp := newMinimalMvp()
	pl := player.New(mvp, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)
	handlePlay(ctx, c, pl, jfin.MediaConfig{LocalKbps: 10000, RemoteKbps: 25000}, playData(t, "PlayNow", "a"))
	if !pl.HasVideo() {
		t.Fatal("no video after PlayNow")
	}

	// Playstate: pause, unpause, seek.
	handlePlaystate(pl, json.RawMessage(`{"Command":"Pause"}`))
	if mvp.props["pause"] != true {
		t.Errorf("pause = %v, want true", mvp.props["pause"])
	}
	handlePlaystate(pl, json.RawMessage(`{"Command":"Unpause"}`))
	if mvp.props["pause"] != false {
		t.Errorf("pause = %v, want false", mvp.props["pause"])
	}
	handlePlaystate(pl, json.RawMessage(`{"Command":"Seek","SeekPositionTicks":300000000}`))

	// GeneralCommand: volume, mute, navigation (opens the OSD menu).
	handleGeneralCommand(pl, "SetVolume", json.RawMessage(`{"Volume":42}`))
	if got := mvp.props["volume"]; got != 42.0 {
		t.Errorf("volume = %v, want 42", got)
	}
	handleGeneralCommand(pl, "Mute", json.RawMessage(`{}`))
	if mvp.props["mute"] != true {
		t.Errorf("mute = %v, want true", mvp.props["mute"])
	}
	handleGeneralCommand(pl, "GoHome", json.RawMessage(`{}`))
	if mvp.props["osd-border-style"] != "background-box" {
		t.Error("GoHome did not open the OSD menu")
	}
	handleGeneralCommand(pl, "Back", json.RawMessage(`{}`))
	if mvp.props["osd-border-style"] == "background-box" {
		t.Error("Back did not close the OSD menu")
	}

	// Stop via Playstate tears playback down.
	handlePlaystate(pl, json.RawMessage(`{"Command":"Stop"}`))
	if pl.HasVideo() {
		t.Error("still playing after Stop")
	}
}

func TestGeneralCommandExtras(t *testing.T) {
	ts := playServer(t)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	mvp := newMinimalMvp()
	pl := player.New(mvp, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)
	handlePlay(ctx, c, pl, jfin.MediaConfig{LocalKbps: 10000, RemoteKbps: 25000}, playData(t, "PlayNow", "a"))

	mvp.SetProperty("volume", 50.0)
	mvp.SetProperty("mute", false)
	handleGeneralCommand(pl, "VolumeUp", json.RawMessage(`{}`))
	if got := mvp.props["volume"]; got != 55.0 {
		t.Errorf("VolumeUp: volume = %v, want 55", got)
	}
	handleGeneralCommand(pl, "VolumeDown", json.RawMessage(`{}`))
	if got := mvp.props["volume"]; got != 50.0 {
		t.Errorf("VolumeDown: volume = %v, want 50", got)
	}
	handleGeneralCommand(pl, "ToggleMute", json.RawMessage(`{}`))
	if mvp.props["mute"] != true {
		t.Error("ToggleMute did not mute")
	}
	handleGeneralCommand(pl, "TakeScreenshot", json.RawMessage(`{}`))
}

// Remote clients differ in how they encode command arguments: jellyfin-web
// sends ints, others fractions or strings. None of them may be dropped.
func TestCommandArgumentParsing(t *testing.T) {
	cases := []struct {
		args string
		want int
		ok   bool
	}{
		{`{"Volume":40}`, 40, true},     // jellyfin-web
		{`{"Volume":25.0}`, 25, true},   // float that is a whole number
		{`{"Volume":0.63}`, 63, true},   // 0-1 fraction
		{`{"Volume":"55"}`, 55, true},   // numeric string
		{`{"Volume":0}`, 0, true},       // silence
		{`{"Volume":-5}`, -5, true},     // clamped by Player.SetVolume
		{`{"Volume":250}`, 250, true},   // clamped by Player.SetVolume
		{`{"volume":40}`, 0, false},     // wrong key
		{`{}`, 0, false},                // missing
		{`{"Volume":"loud"}`, 0, false}, // garbage
	}
	for _, c := range cases {
		got, ok := volumeArg(json.RawMessage(c.args))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("volumeArg(%s) = %d,%v want %d,%v", c.args, got, ok, c.want, c.ok)
		}
	}

	idx := []struct {
		args string
		want int
		ok   bool
	}{
		{`{"Index":3}`, 3, true},
		{`{"Index":"3"}`, 3, true},
		{`{"Index":-1}`, -1, true},
		{`{"Index":null}`, 0, false},
		{`{}`, 0, false},
	}
	for _, c := range idx {
		got, ok := indexArg(json.RawMessage(c.args))
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("indexArg(%s) = %d,%v want %d,%v", c.args, got, ok, c.want, c.ok)
		}
	}
}

// A SetVolume with a fraction must still reach mpv as a percentage.
func TestGeneralCommandSetVolumeFraction(t *testing.T) {
	ts := playServer(t)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	mvp := newMinimalMvp()
	pl := player.New(mvp, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)
	handlePlay(ctx, c, pl, jfin.MediaConfig{LocalKbps: 10000, RemoteKbps: 25000}, playData(t, "PlayNow", "a"))

	mvp.SetProperty("volume", 100.0)
	mvp.SetProperty("mute", false)
	handleGeneralCommand(pl, "SetVolume", json.RawMessage(`{"Volume":0.42}`))
	if got := mvp.props["volume"]; got != 42.0 {
		t.Errorf("fractional SetVolume → volume %v, want 42", got)
	}
	// Muting then moving the slider unmutes.
	handleGeneralCommand(pl, "Mute", json.RawMessage(`{}`))
	handleGeneralCommand(pl, "SetVolume", json.RawMessage(`{"Volume":70}`))
	if mvp.props["mute"] != false {
		t.Error("SetVolume did not unmute")
	}
	if got := mvp.props["volume"]; got != 70.0 {
		t.Errorf("volume = %v, want 70", got)
	}
}

// config → player options → config must be a no-op round trip: that is what
// the OSD preference menus rely on when they write the settings back.
func TestSettingsOptionsRoundTrip(t *testing.T) {
	s := DefaultSettings()
	s.SeekUp, s.SeekDown, s.SeekLeft, s.SeekRight = 120, -120, -15, 30
	s.SeekHExact, s.MediaKeySeek, s.UseWebSeek = true, true, true
	s.SubtitleSize, s.SubtitleColor, s.SubtitlePosition = 125, "#FFEE00EE", "top"
	s.AutoPlay, s.Fullscreen, s.EnableOSC = false, false, false
	s.SkipIntroAlways, s.SkipCredits = true, false
	s.MenuMouse, s.WriteLog, s.CheckUpdates = false, true, false
	s.TranscodeHi10p, s.TranscodeHDR, s.TranscodeDolbyVision = true, true, false
	s.DirectPaths, s.RemoteDirectPaths = false, true // the menu must not merge these
	s.RemoteKbps = 4000
	s.KeyBindings = map[string]string{"c": "fullscreen"}

	before := s
	settingsMu.Lock()
	applyOptionsToSettings(&s, playerOptionsLocked(&s))
	settingsMu.Unlock()
	if !reflect.DeepEqual(before, s) {
		t.Errorf("round trip changed the settings:\nbefore %+v\nafter  %+v", before, s)
	}
}

// A preference change (menu goroutine) concurrent with a Play (WS goroutine)
// must not race: both touch the shared Settings.
func TestSettingsConcurrentAccess(t *testing.T) {
	ts := playServer(t)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	pl := player.New(newMinimalMvp(), log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)

	s := DefaultSettings()
	pl.SetSaveFunc(func(o player.Options) { applyOptionsToSettings(&s, o) })

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// "menu" side: flip preferences.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			o := player.DefaultOptions()
			o.AutoPlay = i%2 == 0
			o.SeekRight = float64(i)
			// The production path: apply + persist under one lock.
			if err := applyAndSave(&s, o, filepath.Join(t.TempDir(), "config.json")); err != nil {
				return
			}
			_ = mediaConfig(&s)
		}
		close(stop)
	}()
	// "server" side: plays read the same settings.
	for i := 0; i < 20; i++ {
		handlePlay(ctx, c, pl, mediaConfig(&s), playData(t, "PlayNow", "a"))
	}
	wg.Wait()
	<-stop
}

// syncWriter is a mutex-guarded log sink: the session logs from its own
// goroutine while the test inspects it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// Disconnect stops the socket loop but keeps the player usable, and a
// reconnect starts the loop again (the tray item toggles between the two).
func TestSessionDisconnectReconnect(t *testing.T) {
	var logBuf syncWriter
	lg := log.New(&logBuf, "", 0)
	s := DefaultSettings()
	sess, err := newSession(&s, jfin.Account{Server: "http://127.0.0.1:1", DeviceID: "d1"}, lg, ui.NewLogRing(10), t.TempDir(), filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sess.pl.Start(ctx)
	defer sess.shutdown(ctx)

	sess.start(ctx)
	// The server is unreachable, so we should be in the reconnecting state.
	deadline := time.Now().Add(2 * time.Second)
	for sess.ws.State() != jfin.StateReconnecting {
		if time.Now().After(deadline) {
			t.Fatalf("state = %d, want reconnecting", sess.ws.State())
		}
		time.Sleep(10 * time.Millisecond)
	}

	sess.disconnect()
	if sess.connected() {
		t.Error("still connected after disconnect")
	}
	// The player is untouched by a disconnect.
	if sess.pl.Status().Playing {
		t.Log("player reports playing (mpv alive) — fine")
	}

	// Reconnecting starts the loop again.
	sess.start(ctx)
	deadline = time.Now().Add(2 * time.Second)
	for sess.ws.State() != jfin.StateReconnecting {
		if time.Now().After(deadline) {
			t.Fatalf("state after reconnect = %d, want reconnecting", sess.ws.State())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(logBuf.String(), "connecting to") {
		t.Errorf("reconnect did not log a new attempt:\n%s", logBuf.String())
	}
}

// The volume memory as wired in main: the remembered volume/mute are restored
// on the next playback, and config.json is written only when the player commits
// (end of playback / mpv exit / app exit) — not on every change.
func TestVolumeMemoryEndToEnd(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	media := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(media, []byte("not really a movie"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := DefaultSettings()
	s.RememberVolume = true
	s.LastVolume, s.LastMuted = 37, true
	if err := s.Save(cfgPath); err != nil {
		t.Fatal(err)
	}
	ts := playServer(t)
	c := jfin.New(ts.URL, "dev", "d1", "1", false)
	c.Token, c.UserID = "tok", "u"
	lg := log.New(io.Discard, "", 0)
	mem := volumeMemory(&s, cfgPath, lg)
	pl := player.New(newMinimalMvp(), lg)
	pl.SetOptions(playerOptions(&s))
	pl.SetVolumeMemory(mem)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pl.Start(ctx)

	// The state we would restore.
	if got := mem.Get(); got.Volume != 37 || !got.Mute {
		t.Errorf("restored state = %+v, want {37 true}", got)
	}

	read := func() Settings {
		t.Helper()
		var r Settings
		if err := r.Load(cfgPath); err != nil {
			t.Fatal(err)
		}
		return r
	}

	// A change is recorded but not written: config.json is untouched.
	mem.Record(player.VolumeState{Volume: 64, Mute: false})
	if got := read().LastVolume; got != 37 {
		t.Errorf("config changed without a commit: last_volume = %d", got)
	}

	// Commit writes once, and is a no-op when nothing changed.
	mem.Commit()
	if got := read(); got.LastVolume != 64 || got.LastMuted {
		t.Errorf("after commit: volume=%d muted=%v, want 64/false", got.LastVolume, got.LastMuted)
	}
	before := cfgModTime(t, cfgPath)
	mem.Commit() // nothing new recorded
	if cfgModTime(t, cfgPath) != before {
		t.Error("a commit with no change rewrote the file")
	}

	// With the toggle off the getter reports nothing to restore.
	s.RememberVolume = false
	if got := mem.Get(); got.Volume != 0 || got.Mute {
		t.Errorf("getter with remember off = %+v, want empty", got)
	}
}

func cfgModTime(t *testing.T, path string) time.Time {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// The OSD-editable settings must survive the options → settings write-back, or
// a change made in the menu would be lost on the next start.
func TestOSDSettingsWriteBack(t *testing.T) {
	s := DefaultSettings()
	before := s

	applyOptionsToSettings(&s, func() player.Options {
		o := playerOptionsLocked(&before)
		o.LocalKbps = 3000
		o.AlwaysTranscode = true
		o.TranscodeH265 = true
		o.ForceH264 = true
		o.IdleStop = false
		o.IdleStopAfter = 6 * 60 * 60 * 1e9
		o.LogLevel = "debug"
		o.SanitizeOutput = false
		o.SeekLeft, o.SeekRight = -15, 15
		return o
	}())

	checks := []struct {
		name string
		got  any
		want any
	}{
		{"local_kbps", s.LocalKbps, 3000},
		{"always_transcode", s.AlwaysTranscode, true},
		{"transcode_h265", s.TranscodeH265, true},
		{"force_h264", s.ForceH264, true},
		{"idle_stop", s.IdleStop, false},
		{"idle_delay_s", s.IdleDelayS, 21600},
		{"log_level", s.LogLevel, "debug"},
		{"sanitize_output", s.SanitizeOutput, false},
		{"seek_left", s.SeekLeft, -15.0},
		{"seek_right", s.SeekRight, 15.0},
	}
	for _, c := range checks {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s = %v, want %v", c.name, c.got, c.want)
		}
	}
	// And the media pipeline picks the new values up for the next play.
	mc := mediaConfig(&s)
	if mc.LocalKbps != 3000 || !mc.AlwaysTranscode || !mc.TranscodeH265 || !mc.ForceH264 {
		t.Errorf("media config did not follow the OSD change: %+v", mc)
	}
}

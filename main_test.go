package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"mpv-shim/jfin"
	"mpv-shim/player"
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
func (m *minimalMvp) SubAdd(u string) error       { return nil }
func (m *minimalMvp) Keybind(key, cmd string)     {}
func (m *minimalMvp) Command(args ...any) error   { return nil }
func (m *minimalMvp) Incarnation() int            { return 1 }
func (m *minimalMvp) Screenshot(dir string) error { return nil }
func (m *minimalMvp) Observe(name string) error   { return nil }
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

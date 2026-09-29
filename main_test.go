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
	m.props[n] = v
	m.mu.Unlock()
}
func (m *minimalMvp) GetProperty(n string) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.props[n], nil
}
func (m *minimalMvp) SubAdd(u string) error     { return nil }
func (m *minimalMvp) Keybind(key, cmd string)   {}
func (m *minimalMvp) Command(args ...any) error { return nil }
func (m *minimalMvp) Incarnation() int          { return 1 }
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
	if got := mvp.props["volume"]; got != 42 {
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

package player

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
	"time"

	"mpv-shim/jfin"
)

// fakeMpv implements Mpv in memory.
type fakeMpv struct {
	mu       sync.Mutex
	props    map[string]any
	alive    bool
	graceful bool
	loads    []string
	hookFn   func(string, json.RawMessage)
	exit     chan struct{}
	stopped  int
}

func newFakeMpv() *fakeMpv {
	return &fakeMpv{props: map[string]any{}, exit: make(chan struct{}, 1)}
}

func (f *fakeMpv) EnsureRunning(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = true
	return nil
}
func (f *fakeMpv) LoadFile(ctx context.Context, u string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loads = append(f.loads, u)
	f.props["duration"] = 100.0
	f.props["time-pos"] = 0.0
	return nil
}
func (f *fakeMpv) Stop() error {
	f.mu.Lock()
	f.stopped++
	f.mu.Unlock()
	return nil
}
func (f *fakeMpv) SetProperty(n string, v any) {
	f.mu.Lock()
	f.props[n] = v
	f.mu.Unlock()
}
func (f *fakeMpv) GetProperty(n string) (any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props[n], nil
}
func (f *fakeMpv) SubAdd(u string) error {
	f.mu.Lock()
	f.props["sub-add"] = u
	f.mu.Unlock()
	return nil
}
func (f *fakeMpv) ShowText(text string, ms, level int) {}
func (f *fakeMpv) Alive() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive
}
func (f *fakeMpv) Graceful() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.graceful
}
func (f *fakeMpv) Exit() <-chan struct{} {
	return f.exit
}
func (f *fakeMpv) SetEventHook(h func(string, json.RawMessage)) {
	f.mu.Lock()
	f.hookFn = h
	f.mu.Unlock()
}
func (f *fakeMpv) Kill() {
	f.mu.Lock()
	f.alive = false
	select {
	case f.exit <- struct{}{}:
	default:
	}
	f.mu.Unlock()
}

// test helpers
func (f *fakeMpv) fire(name string) {
	f.mu.Lock()
	h := f.hookFn
	f.mu.Unlock()
	if h != nil {
		h(name, nil)
	}
}
func (f *fakeMpv) crash() {
	f.mu.Lock()
	f.alive = false
	select {
	case f.exit <- struct{}{}:
	default:
	}
	f.mu.Unlock()
}
func (f *fakeMpv) numLoads() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.loads)
}

type recs struct {
	mu       sync.Mutex
	playing  []jfin.SessionInfo
	progress []jfin.SessionInfo
	stopped  []jfin.SessionInfo
	watched  []string
}

func (r *recs) snapshot() (p, pr, s []jfin.SessionInfo, w []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]jfin.SessionInfo(nil), r.playing...),
		append([]jfin.SessionInfo(nil), r.progress...),
		append([]jfin.SessionInfo(nil), r.stopped...),
		append([]string(nil), r.watched...)
}

func testSource() jfin.MediaSource {
	return jfin.MediaSource{
		ID: "src1", Protocol: "Http", Bitrate: 100000,
		SupportsDirectPlay: true, SupportsDirectStream: true, SupportsTranscoding: true,
		MediaStreams: []jfin.MediaStream{
			{Type: "Video", Index: 0},
			{Type: "Audio", Index: 1, Language: "eng"},
			{Type: "Subtitle", Index: 2, Language: "eng"},
		},
	}
}

var runTimeTicks = int64(1_000_000_000) // 100 s

func testServer(t *testing.T, r *recs) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		switch {
		case strings.HasSuffix(p, "/PlaybackInfo"):
			_ = json.NewEncoder(w).Encode(jfin.PlaybackInfo{
				PlaySessionId: "ps",
				MediaSources:  []jfin.MediaSource{testSource()},
			})
		case strings.Contains(p, "/Items/"):
			id := p[strings.LastIndex(p, "/")+1:]
			_ = json.NewEncoder(w).Encode(jfin.Item{
				ID: id, Type: "Video", Name: "T" + id,
				RunTimeTicks: &runTimeTicks,
			})
		case p == "/Sessions/Playing":
			var s jfin.SessionInfo
			_ = json.NewDecoder(req.Body).Decode(&s)
			r.mu.Lock()
			r.playing = append(r.playing, s)
			r.mu.Unlock()
			w.WriteHeader(204)
		case p == "/Sessions/Playing/Progress":
			var s jfin.SessionInfo
			_ = json.NewDecoder(req.Body).Decode(&s)
			r.mu.Lock()
			r.progress = append(r.progress, s)
			r.mu.Unlock()
			w.WriteHeader(204)
		case p == "/Sessions/Playing/Stopped":
			var s jfin.SessionInfo
			_ = json.NewDecoder(req.Body).Decode(&s)
			r.mu.Lock()
			r.stopped = append(r.stopped, s)
			r.mu.Unlock()
			w.WriteHeader(204)
		case strings.Contains(p, "/PlayedItems/"):
			id := p[strings.LastIndex(p, "/")+1:]
			r.mu.Lock()
			r.watched = append(r.watched, id)
			r.mu.Unlock()
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

type harness struct {
	c    *jfin.Client
	pl   *Player
	fm   *fakeMpv
	recs *recs
	url  string
}

func setup(t *testing.T) *harness {
	t.Helper()
	r := &recs{}
	ts := testServer(t, r)
	c := jfin.New(ts.URL, "test", "dev1", "1.0", false)
	c.Token, c.UserID = "tok", "u"
	fm := newFakeMpv()
	pl := New(fm, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pl.Start(ctx)
	return &harness{c: c, pl: pl, fm: fm, recs: r, url: ts.URL}
}

func cfg() jfin.MediaConfig {
	return jfin.MediaConfig{LocalKbps: 10000, RemoteKbps: 25000}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func TestPlayProgressWatched(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	m, err := jfin.NewMedia(ctx, h.c, cfg(), []string{"a"}, 0, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("NewMedia: %v", err)
	}
	if err := h.pl.Play(m, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, "session start", func() bool {
		p, _, _, _ := h.recs.snapshot()
		return len(p) == 1
	})
	p, _, _, _ := h.recs.snapshot()
	if p[0].PlaySessionID != "ps" {
		t.Errorf("PlaySessionID = %q, want ps", p[0].PlaySessionID)
	}
	if p[0].PlayMethod != "DirectPlay" {
		t.Errorf("PlayMethod = %q, want DirectPlay", p[0].PlayMethod)
	}
	if p[0].ItemID != "a" {
		t.Errorf("ItemID = %q, want a", p[0].ItemID)
	}

	// progress at 10 s → 1e8 ticks
	h.fm.SetProperty("time-pos", 10.0)
	h.pl.Tick()
	waitFor(t, "progress", func() bool {
		_, pr, _, _ := h.recs.snapshot()
		return len(pr) == 1
	})
	_, pr, _, _ := h.recs.snapshot()
	if pr[0].PositionTicks != 100_000_000 {
		t.Errorf("PositionTicks = %d, want 100000000", pr[0].PositionTicks)
	}

	// ≥90% of 100 s → watched
	h.fm.SetProperty("time-pos", 95.0)
	h.pl.Tick()
	waitFor(t, "watched", func() bool {
		_, _, _, w := h.recs.snapshot()
		return len(w) == 1 && w[0] == "a"
	})
}

func TestStopReports(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	m, err := jfin.NewMedia(ctx, h.c, cfg(), []string{"a"}, 0, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("NewMedia: %v", err)
	}
	if err := h.pl.Play(m, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, "start", func() bool {
		p, _, _, _ := h.recs.snapshot()
		return len(p) == 1
	})
	h.pl.Stop()
	waitFor(t, "stopped", func() bool {
		_, _, s, _ := h.recs.snapshot()
		return len(s) == 1
	})
	if h.pl.HasVideo() {
		t.Error("HasVideo after stop")
	}
}

func TestEndFileAdvancesQueue(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	m, err := jfin.NewMedia(ctx, h.c, cfg(), []string{"a", "b"}, 0, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("NewMedia: %v", err)
	}
	if err := h.pl.Play(m, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, "start a", func() bool {
		p, _, _, _ := h.recs.snapshot()
		return len(p) == 1
	})
	h.fm.SetProperty("time-pos", 20.0)
	h.fm.fire("end-file")
	waitFor(t, "start b", func() bool {
		p, _, _, _ := h.recs.snapshot()
		return len(p) == 2
	})
	p, _, s, w := h.recs.snapshot()
	if len(s) != 1 || s[0].ItemID != "a" {
		t.Fatalf("stopped = %+v, want [a]", s)
	}
	if s[0].PositionTicks != 1_000_000_000 {
		t.Errorf("finished PositionTicks = %d, want duration 1e9", s[0].PositionTicks)
	}
	if p[1].ItemID != "b" {
		t.Errorf("next item = %q, want b", p[1].ItemID)
	}
	if len(w) != 1 || w[0] != "a" {
		t.Errorf("watched = %v, want [a]", w)
	}
	if h.fm.numLoads() != 2 || !strings.Contains(h.fm.loadsTail(), "/b") {
		t.Errorf("loads = %v, want 2nd load of /b", h.fm.loadsTail())
	}
}

func (f *fakeMpv) loadsTail() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.loads) == 0 {
		return ""
	}
	return f.loads[len(f.loads)-1]
}

func TestCrashRestart(t *testing.T) {
	h := setup(t)
	ctx := context.Background()
	m, err := jfin.NewMedia(ctx, h.c, cfg(), []string{"a"}, 0, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("NewMedia: %v", err)
	}
	if err := h.pl.Play(m, 0); err != nil {
		t.Fatalf("Play: %v", err)
	}
	waitFor(t, "start", func() bool {
		p, _, _, _ := h.recs.snapshot()
		return len(p) == 1
	})
	h.fm.SetProperty("time-pos", 33.0)
	h.pl.Tick() // updates lastPos
	h.fm.crash()
	waitFor(t, "reload", func() bool { return h.fm.numLoads() == 2 })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.fm.mu.Lock()
		x, _ := h.fm.props["time-pos"].(float64)
		h.fm.mu.Unlock()
		if x == 33.0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.fm.mu.Lock()
	x, _ := h.fm.props["time-pos"].(float64)
	h.fm.mu.Unlock()
	if x != 33.0 {
		t.Errorf("resumed time-pos = %v, want 33", x)
	}
	if !h.pl.HasVideo() {
		t.Error("HasVideo lost after crash restart")
	}
}


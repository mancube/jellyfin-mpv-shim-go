package player

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mpv-shim/jfin"
)

// fakeMpv implements Mpv in memory.
type fakeMpv struct {
	mu          sync.Mutex
	props       map[string]any
	alive       bool
	graceful    bool
	loads       []string
	texts       []string
	binds       []string
	cmds        []string
	shots       []string
	observed    []string
	incarnation int
	hookFn      func(string, json.RawMessage)
	exit        chan struct{}
	stopped     int
}

func newFakeMpv() *fakeMpv {
	return &fakeMpv{props: map[string]any{}, exit: make(chan struct{}, 1)}
}

func (f *fakeMpv) EnsureRunning(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.alive = true
	f.incarnation++ // a spawn: observers/bindings must be re-applied
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
	// mpv's IPC returns numbers as float64; mirror that.
	switch t := v.(type) {
	case int:
		v = float64(t)
	case string:
		if n == "volume" || n == "osd-font-size" {
			if fv, err := strconv.ParseFloat(t, 64); err == nil {
				v = fv
			}
		}
	}
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
	f.cmds = append(f.cmds, "sub-add "+u+" cached")
	f.mu.Unlock()
	return nil
}
func (f *fakeMpv) ShowText(text string, ms, level int) {
	f.mu.Lock()
	f.texts = append(f.texts, text)
	f.mu.Unlock()
}
func (f *fakeMpv) Keybind(key, cmd string) {
	f.mu.Lock()
	f.binds = append(f.binds, key+"="+cmd)
	f.mu.Unlock()
}
func (f *fakeMpv) Command(args ...any) error {
	f.mu.Lock()
	f.cmds = append(f.cmds, fmt.Sprint(args...))
	// Mirror mpv: after a seek, time-pos reflects the new position.
	if len(args) >= 3 && fmt.Sprint(args[0]) == "seek" {
		to, _ := args[1].(float64)
		cur, _ := f.props["time-pos"].(float64)
		if fmt.Sprint(args[2]) == "absolute" || fmt.Sprint(args[2]) == "absolute+exact" {
			f.props["time-pos"] = to
		} else {
			f.props["time-pos"] = cur + to
		}
	}
	f.mu.Unlock()
	return nil
}
func (f *fakeMpv) Screenshot(dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.shots = append(f.shots, dir)
	return nil
}

// Observe records the subscription; changeProp simulates mpv's event.
func (f *fakeMpv) Observe(name string) error {
	f.mu.Lock()
	f.observed = append(f.observed, name)
	f.mu.Unlock()
	return nil
}

// changeProp mimics mpv: the property already holds the new value when the
// property-change event fires.
func (f *fakeMpv) changeProp(name string, value any) {
	f.mu.Lock()
	f.props[name] = value
	h := f.hookFn
	f.mu.Unlock()
	if h == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"name": name, "data": value})
	h("property-change", b)
}

// endFile fires the real end-file event with mpv 0.41's reason field.
func (f *fakeMpv) endFile(reason string) {
	f.mu.Lock()
	h := f.hookFn
	f.mu.Unlock()
	if h == nil {
		return
	}
	b, _ := json.Marshal(map[string]any{"reason": reason})
	h("end-file", b)
}

func (f *fakeMpv) Incarnation() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.incarnation
}
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
func (f *fakeMpv) fire(name string, args ...any) {
	f.mu.Lock()
	h := f.hookFn
	f.mu.Unlock()
	if h == nil {
		return
	}
	var data json.RawMessage
	if len(args) > 0 {
		data, _ = json.Marshal(args[0])
	}
	h(name, data)
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

// lastText returns the most recent OSD text (menu rendering).
func (f *fakeMpv) lastText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.texts) == 0 {
		return ""
	}
	return f.texts[len(f.texts)-1]
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
		ID: "src1", Protocol: "File", Bitrate: 100000,
		SupportsDirectPlay: true, SupportsDirectStream: true, SupportsTranscoding: true,
		MediaStreams: []jfin.MediaStream{
			{Type: "Video", Index: 0},
			{Type: "Audio", Index: 1, Language: "eng", Title: "English"},
			{Type: "Subtitle", Index: 2, Language: "eng", Title: "English", DeliveryMethod: "Embed"},
			{Type: "Audio", Index: 3, Language: "spa", Title: "Spanish"},
			{Type: "Subtitle", Index: 4, Language: "eng", Title: "English (ext)", IsExternal: true,
				DeliveryMethod: "External", DeliveryUrl: "/Videos/a/Subtitles/4/Stream.vtt"},
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
		case p == "/MediaSegments/src1":
			_ = json.NewEncoder(w).Encode(struct {
				Items []jfin.MediaSegment `json:"Items"`
			}{Items: []jfin.MediaSegment{
				{Type: "Intro", StartTicks: 0, EndTicks: 30 * 1e7},
			}})
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
	h.fm.endFile("eof")
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

// --- M3: remote control + OSD menu ---

// playOne starts playback of item "a" and waits for the session start report.
func playOne(t *testing.T, h *harness, c jfin.MediaConfig) *jfin.Media {
	t.Helper()
	m, err := jfin.NewMedia(context.Background(), h.c, c, []string{"a"}, 0, "", nil, nil, nil)
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
	return m
}

func (f *fakeMpv) prop(name string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.props[name]
}

func (f *fakeMpv) numCmds() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.cmds)
}

func TestRemoteVolumeMutePauseSeek(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	// Volume: the server spams SetVolume, so an unchanged value must not
	// produce a second report (upstream's de-dupe).
	h.fm.SetProperty("volume", 100.0)
	h.pl.SetVolume(50)
	waitFor(t, "volume report", func() bool { return h.pl.GetVolume() == 50 })
	_, pr0, _, _ := h.recs.snapshot()
	n := len(pr0)
	h.pl.SetVolume(50)
	time.Sleep(50 * time.Millisecond)
	_, pr1, _, _ := h.recs.snapshot()
	if len(pr1) != n {
		t.Errorf("unchanged SetVolume reported again (%d → %d)", n, len(pr1))
	}
	if pr0[n-1].VolumeLevel != 50 {
		t.Errorf("reported volume = %d, want 50", pr0[n-1].VolumeLevel)
	}

	// Mute.
	h.pl.SetMute(true)
	waitFor(t, "mute report", func() bool {
		_, pr, _, _ := h.recs.snapshot()
		return pr[len(pr)-1].IsMuted
	})

	// Pause toggles the mpv property and reports the paused state.
	h.pl.TogglePause()
	waitFor(t, "pause report", func() bool {
		_, pr, _, _ := h.recs.snapshot()
		return pr[len(pr)-1].IsPaused
	})
	if h.fm.prop("pause") != true {
		t.Errorf("mpv pause = %v, want true", h.fm.prop("pause"))
	}
	h.pl.TogglePause()
	if h.fm.prop("pause") != false {
		t.Errorf("mpv pause = %v, want false", h.fm.prop("pause"))
	}

	// Seek: absolute, reported at the new position.
	h.pl.Seek(42, true)
	waitFor(t, "seek report", func() bool {
		_, pr, _, _ := h.recs.snapshot()
		return pr[len(pr)-1].PositionTicks == 42*1e7
	})
}

func TestMenuOpenNavigateSelectAudio(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	h.pl.Key("menu")
	if !h.pl.menu.Shown() {
		t.Fatal("menu not shown after 'c'")
	}
	if got := h.fm.lastText(); !strings.Contains(got, "Change Audio") || !strings.Contains(got, "Close Menu") {
		t.Errorf("root menu text = %q", got)
	}
	if h.fm.prop("osd-border-style") != "background-box" {
		t.Errorf("osd-border-style = %v, want background-box", h.fm.prop("osd-border-style"))
	}
	if h.fm.prop("pause") != true {
		t.Error("menu should pause playback")
	}

	// Down to "Change Audio" (index 0 → wrapped selection) and open it.
	h.pl.Key("ok")
	if got := h.fm.lastText(); !strings.Contains(got, "Select Audio Track") {
		t.Fatalf("audio menu text = %q", got)
	}
	// Pick the second audio track (Jellyfin index 3 → mpv id 2).
	h.pl.Key("down")
	h.pl.Key("ok")
	if got := fmt.Sprint(h.fm.prop("audio")); got != "2" {
		t.Errorf("mpv audio = %v, want 2", got)
	}
	if got := h.fm.lastText(); !strings.Contains(got, "Main Menu") {
		t.Errorf("after select, expected the root menu, got %q", got)
	}

	// Back at the root, "back" closes and restores OSD.
	h.pl.Key("back")
	if h.pl.menu.Shown() {
		t.Error("menu still shown after 'back' from the root")
	}
	if h.fm.prop("osd-border-style") == "background-box" {
		t.Error("osd-border-style not restored")
	}
}

func TestMenuSubtitleExternal(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	h.pl.Key("menu")
	h.pl.Key("down") // Change Subtitles
	h.pl.Key("ok")
	if got := h.fm.lastText(); !strings.Contains(got, "Select Subtitle Track") {
		t.Fatalf("subtitle menu text = %q", got)
	}
	// Entries: None, embedded (2), external (4).
	h.pl.Key("down")
	h.pl.Key("down")
	h.pl.Key("ok")
	if got, _ := h.fm.prop("sub-add").(string); !strings.Contains(got, "Subtitles/4/Stream.vtt") {
		t.Errorf("sub-add = %v, want the external subtitle URL", got)
	}
}

func TestKeyFallbackSeekWhenMenuClosed(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("time-pos", 10.0)

	before := h.fm.numCmds()
	h.pl.Key("left")  // menu closed → -5 s
	h.pl.Key("right") // +5 s
	h.pl.Key("up")    // +60 s
	if h.fm.numCmds() != before+3 {
		t.Fatalf("expected 3 seek commands, got %d", h.fm.numCmds()-before)
	}
	h.fm.mu.Lock()
	got := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	// mpv's "relative" is relative to the current position, so the *amount*
	// is what we send — never a computed absolute position.
	if want := "seek-5relative|seek5relative|seek60relative"; got != want {
		t.Errorf("seek commands = %q, want %q", got, want)
	}
}

func TestIntroSkipOnce(t *testing.T) {
	// "Ask to skip" (skip_intro): only near the end of the segment.
	h := setup(t)
	c := cfg()
	c.SkipIntro = true
	playOne(t, h, c)

	h.fm.SetProperty("time-pos", 5.0) // intro is 0-30 s
	before := h.fm.numCmds()
	h.pl.Tick()
	h.fm.mu.Lock()
	cmds := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if strings.Contains(cmds, "absolute") {
		t.Errorf("skipped too early in ask mode: %q", cmds)
	}
	if txt := h.fm.lastText(); txt != "Seek to Skip Intro" {
		t.Errorf("OSD text = %q, want the prompt", txt)
	}

	// Within the window it skips for real, once.
	h.fm.SetProperty("time-pos", 20.0)
	before = h.fm.numCmds()
	h.pl.Tick()
	h.fm.mu.Lock()
	got := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if !strings.Contains(got, "30") || !strings.Contains(got, "absolute") {
		t.Errorf("expected a seek to the intro end (30), got %q", got)
	}
	if txt := h.fm.lastText(); txt != "Skipped Intro" {
		t.Errorf("OSD text = %q, want %q", txt, "Skipped Intro")
	}
	before = h.fm.numCmds()
	h.fm.SetProperty("time-pos", 21.0)
	h.pl.Tick()
	if h.fm.numCmds() != before {
		t.Error("intro skip repeated after HasTriggered")
	}
}

// "Always skip" (skip_intro_always) jumps as soon as the segment starts.
func TestIntroSkipAlways(t *testing.T) {
	h := setup(t)
	c := cfg()
	c.SkipIntro = true // the segments are fetched; the *option* decides how
	playOne(t, h, c)
	o := h.pl.Options()
	o.SkipIntro, o.SkipIntroAlways = false, true // "always skip", no prompt
	h.pl.SetOptions(o)

	h.fm.SetProperty("time-pos", 1.0)
	before := h.fm.numCmds()
	h.pl.Tick()
	h.fm.mu.Lock()
	got := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if !strings.Contains(got, "30") {
		t.Errorf("always-skip did not seek to the intro end: %q", got)
	}
}

// The mpv keybindings speak through client-message; check the routing.
func TestClientMessageOpensMenu(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.fire("client-message", []string{"shim-menu", "menu"})
	waitFor(t, "menu open", func() bool { return h.pl.menu.Shown() })
	if h.fm.prop("osd-border-style") != "background-box" {
		t.Error("menu did not apply the OSD box style")
	}
	h.fm.fire("client-message", []string{"other-script", "menu"})
	if !h.pl.menu.Shown() {
		t.Error("unrelated client-message closed/ignored the menu unexpectedly")
	}
}

// --- M5: idle stop, watched/unwatched, screenshot, volume step ---

func TestIdleStopAfterDelay(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.pl.SetIdleStop(50 * time.Millisecond)

	// Playing: the idle timer keeps resetting.
	h.fm.SetProperty("time-pos", 1.0)
	time.Sleep(120 * time.Millisecond)
	h.pl.Tick()
	if !h.pl.HasVideo() {
		t.Fatal("idle stop fired during playback")
	}

	// Paused counts as idle upstream (idle_when_paused); abort the playback
	// instead — the "no media" case must also stop.
	h.pl.Stop()
	if h.pl.HasVideo() {
		t.Fatal("Stop did not clear the media")
	}
	h.pl.Tick() // resets the timer (stopLocked is activity)
	_, _, stopped, _ := h.recs.snapshot()
	n := len(stopped)
	time.Sleep(120 * time.Millisecond)
	h.pl.Tick()
	_, _, stopped2, _ := h.recs.snapshot()
	if len(stopped2) != n {
		t.Errorf("idle stop reported another stop with no media: %d → %d", n, len(stopped2))
	}
}

func TestIdleStopPausedStopsPlayback(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.pl.SetIdleStop(30 * time.Millisecond)
	h.pl.SetPaused(true)
	time.Sleep(80 * time.Millisecond)
	h.pl.Tick()
	if h.pl.HasVideo() {
		t.Error("paused playback was not stopped after the idle delay")
	}
	_, _, stopped, _ := h.recs.snapshot()
	if len(stopped) == 0 {
		t.Error("idle stop did not report a stop")
	}
}

func TestWatchedSkipAndUnwatchedQuit(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	// `w` marks watched (recorded by the fake server) and advances.
	h.pl.Key("watched")
	waitFor(t, "watched", func() bool {
		_, _, _, w := h.recs.snapshot()
		return len(w) >= 1 && w[0] == "a"
	})

	// `u` stops and marks unwatched.
	h.pl.Key("unwatched")
	waitFor(t, "unwatched", func() bool {
		_, _, _, w := h.recs.snapshot()
		return len(w) >= 2 && w[1] == "a"
	})
	if h.pl.HasVideo() {
		t.Error("unwatched quit did not stop playback")
	}
}

func TestStepVolumeToggleMuteScreenshot(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("volume", 50.0)
	h.fm.SetProperty("mute", false)

	h.pl.StepVolume(10)
	if got := h.pl.GetVolume(); got != 60 {
		t.Errorf("volume = %d, want 60", got)
	}
	h.pl.StepVolume(-20)
	if got := h.pl.GetVolume(); got != 40 {
		t.Errorf("volume = %d, want 40", got)
	}
	h.pl.ToggleMute()
	if h.fm.prop("mute") != true {
		t.Error("ToggleMute did not mute")
	}
	h.pl.ToggleMute()
	if h.fm.prop("mute") != false {
		t.Error("ToggleMute did not unmute")
	}
	h.pl.ScreenshotDir = "/tmp/shots"
	h.pl.Screenshot()
	h.fm.mu.Lock()
	shots := append([]string(nil), h.fm.shots...)
	h.fm.mu.Unlock()
	if len(shots) != 1 || shots[0] != "/tmp/shots" {
		t.Errorf("screenshot dirs = %v", shots)
	}
}

// Simultaneous remote commands while playing must not deadlock or race
// (PLAN M5 hardening: "simultaneous remote commands").
func TestConcurrentRemoteCommands(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	ops := []func(){
		func() { h.pl.TogglePause() },
		func() { h.pl.SetVolume(30 + int(time.Now().Unix())%50) },
		func() { h.pl.SetMute(true) },
		func() { h.pl.SetMute(false) },
		func() { h.pl.Seek(30, true) },
		func() { h.pl.Key("menu") },
		func() { h.pl.Key("back") },
		func() { h.pl.Status() },
		func() { h.pl.Tick() },
		func() { h.pl.Next() },
		func() { h.pl.Prev() },
	}
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			for n := 0; n < 20; n++ {
				ops[(i+n)%len(ops)]()
			}
		}(i)
	}
	for i := 0; i < 8; i++ {
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatal("concurrent remote commands deadlocked")
		}
	}
}

// A pause that did not come from us (mpv's own OSC/keymap) must still reach
// the web UI — upstream does this with a `pause` property observer.
func TestExternalPauseIsReported(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.pl.Tick() // baseline report
	_, pr0, _, _ := h.recs.snapshot()
	n := len(pr0)

	h.fm.SetProperty("pause", true) // as if mpv paused itself
	h.pl.Tick()
	_, pr1, _, _ := h.recs.snapshot()
	if len(pr1) != n+1 {
		t.Fatalf("external pause not reported (%d → %d reports)", n, len(pr1))
	}
	if !pr1[len(pr1)-1].IsPaused {
		t.Error("report does not carry IsPaused")
	}
	// No repeat reports while it stays paused.
	h.pl.Tick()
	_, pr2, _, _ := h.recs.snapshot()
	if len(pr2) != len(pr1) {
		t.Errorf("repeated reports while paused: %d → %d", len(pr1), len(pr2))
	}
}

// M5 follow-up: property observers are the immediate feedback path, so a
// local pause/seek/volume change must reach the server without waiting for the
// 5 s timeline tick.
func TestPropertyObserversReportImmediately(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	progress := func() []jfin.SessionInfo {
		_, pr, _, _ := h.recs.snapshot()
		return pr
	}
	base := len(progress())

	// A pause we did not ask for (the web UI or mpv's own OSC).
	h.fm.changeProp("pause", true)
	waitFor(t, "pause report", func() bool { return len(progress()) > base })
	if got := progress()[len(progress())-1]; !got.IsPaused {
		t.Error("pause change not reported as paused")
	}

	// Echo suppression: the same value again must not report.
	n := len(progress())
	h.fm.changeProp("pause", true)
	time.Sleep(50 * time.Millisecond)
	if len(progress()) != n {
		t.Error("duplicate pause change reported")
	}

	// A finished seek reports the new position right away.
	h.fm.changeProp("time-pos", 80.0)
	h.fm.changeProp("seeking", true) // drag start: no report // drag start: no report
	if len(progress()) != n {
		t.Error("seeking=true reported (drag in progress)")
	}
	h.fm.changeProp("seeking", false)
	waitFor(t, "seek report", func() bool { return len(progress()) > n })
	if got := progress()[len(progress())-1]; got.PositionTicks != 80*1e7 {
		t.Errorf("position after seek = %d, want %d (reports: %+v)", got.PositionTicks, int64(80*1e7), progress())
	}

	// Volume change reports immediately too.
	n = len(progress())
	h.fm.changeProp("volume", 42.0)
	waitFor(t, "volume report", func() bool { return len(progress()) > n })
	if got := progress()[len(progress())-1].VolumeLevel; got != 42 {
		t.Errorf("reported volume = %d, want 42", got)
	}
}

// time-pos fires many times a second; position reports are throttled.
func TestPositionReportsAreThrottled(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	_, pr0, _, _ := h.recs.snapshot()
	base := len(pr0)
	for i := 0; i < 40; i++ { // ~40 changes in a few ms
		h.fm.changeProp("time-pos", float64(i))
	}
	time.Sleep(100 * time.Millisecond)
	_, pr1, _, _ := h.recs.snapshot()
	if got := len(pr1) - base; got > 1 {
		t.Errorf("time-pos produced %d reports, want at most 1 (throttled)", got)
	}
}

// The observers are (re)subscribed on every mpv incarnation.
func TestObserversSubscribedAfterSpawn(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg()) // this spawned mpv
	deadline := time.Now().Add(3 * time.Second)
	for {
		h.fm.mu.Lock()
		got := append([]string(nil), h.fm.observed...)
		h.fm.mu.Unlock()
		if len(got) >= len(observedProps) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observed %v, want %v", got, observedProps)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Regression: closing the mpv window (or pressing stop) must NOT auto-advance
// the queue — that used to spawn a new mpv for every following episode.
func TestEndFileStopDoesNotAdvanceQueue(t *testing.T) {
	for _, reason := range []string{"stop", "quit", ""} {
		t.Run("reason="+reason, func(t *testing.T) {
			h := setup(t)
			playOne(t, h, cfg())
			h.pl.InsertQueue([]string{"b", "c"}, false) // queue: a, b, c
			loads := h.fm.numLoads()

			h.fm.endFile(reason)
			time.Sleep(200 * time.Millisecond)

			if h.fm.numLoads() != loads {
				t.Errorf("reason %q loaded another item (%d → %d loads)", reason, loads, h.fm.numLoads())
			}
			if h.pl.HasVideo() {
				t.Error("reason " + reason + ": still has an active video")
			}
			_, _, stopped, _ := h.recs.snapshot()
			if len(stopped) == 0 {
				t.Error("no stop report after " + reason)
			}
		})
	}
}

// A crash loop (mpv dies again and again without playing) must give up.
func TestCrashLoopGivesUp(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	// Each crash resumes at lastPos, which never advances, so the guard trips.
	for i := 0; i < maxCrashRestarts+1; i++ {
		h.fm.crash()
		waitFor(t, "restart handled", func() bool { return !h.pl.HasVideo() || h.fm.numLoads() > i+1 })
		if !h.pl.HasVideo() {
			break
		}
	}
	if h.pl.HasVideo() {
		t.Errorf("crash loop did not give up after %d restarts", maxCrashRestarts)
	}
}

// A track switch made inside mpv (OSC/keys) reaches the web UI.
func TestLocalTrackSwitchIsReported(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	progress := func() []jfin.SessionInfo {
		_, pr, _, _ := h.recs.snapshot()
		return pr
	}
	n := len(progress())

	// The fixture has audio at Jellyfin index 1 and 3 → mpv ids 1 and 2.
	h.fm.changeProp("aid", 2)
	waitFor(t, "audio switch report", func() bool { return len(progress()) > n })
	if got := progress()[len(progress())-1].AudioStreamIndex; got != 3 {
		t.Errorf("reported audio index = %d, want 3", got)
	}

	// Subtitles off in mpv ("no") maps to Jellyfin -1.
	n = len(progress())
	h.fm.changeProp("sid", "no")
	waitFor(t, "subtitle off report", func() bool { return len(progress()) > n })
	if got := progress()[len(progress())-1].SubtitleStreamIndex; got != -1 {
		t.Errorf("reported subtitle index = %d, want -1", got)
	}
}

// mpv echoes each property's current value when we subscribe; that must not be
// mistaken for a user action (an auto-selected track would otherwise be pushed
// to the web UI as the user's choice).
func TestSubscribeEchoIsIgnored(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	// The subscribe echo for sid arrives with mpv's current track.
	h.pl.mu.Lock()
	h.pl.initialEcho["sid"] = true
	h.pl.mu.Unlock()
	h.fm.changeProp("sid", 1) // echo: ignored
	_, pr0, _, _ := h.recs.snapshot()
	n := len(pr0)
	h.fm.changeProp("sid", 1) // a real user switch now (mpv id 1 = Jellyfin 2)
	waitFor(t, "real switch reported", func() bool {
		_, pr, _, _ := h.recs.snapshot()
		return len(pr) > n
	})
	_, pr1, _, _ := h.recs.snapshot()
	if got := pr1[len(pr1)-1].SubtitleStreamIndex; got != 2 {
		t.Errorf("reported subtitle index = %d, want 2", got)
	}
}

// --- bug-hunt regressions ---

// A relative seek moves *by* the amount; lastPos must be the real position,
// not the amount we sent.
func TestRelativeSeekKeepsRealPosition(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("time-pos", 100.0)
	h.pl.Seek(-5, false) // relative
	h.fm.mu.Lock()
	cmds := strings.Join(h.fm.cmds, "|")
	h.fm.mu.Unlock()
	if !strings.Contains(cmds, "seek-5relative") {
		t.Errorf("relative seek command = %q", cmds)
	}
	// The fake applies relative seeks, so our lastPos must match mpv.
	h.pl.mu.Lock()
	got := h.pl.lastPos
	h.pl.mu.Unlock()
	if got != 95 {
		t.Errorf("lastPos = %v, want 95 (the position after the seek)", got)
	}
}

// ESC outside the menu leaves fullscreen (upstream kb_menu_esc).
func TestEscOutsideMenuLeavesFullscreen(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("fullscreen", true)
	h.pl.Key("back")
	if h.fm.prop("fullscreen") != false {
		t.Errorf("fullscreen = %v, want false", h.fm.prop("fullscreen"))
	}
}

// Volume stepping must not clobber the volume when it cannot be read.
func TestStepVolumeIgnoresUnknownVolume(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.mu.Lock()
	delete(h.fm.props, "volume") // mpv not answering
	h.fm.mu.Unlock()
	h.pl.StepVolume(10)
	h.fm.mu.Lock()
	_, set := h.fm.props["volume"]
	h.fm.mu.Unlock()
	if set {
		t.Error("volume was written from an unknown current value")
	}
}

// ESC/arrows must not act when there is no media (a stopped player).
func TestKeysWithoutMediaAreNoops(t *testing.T) {
	h := setup(t)
	before := h.fm.numCmds()
	for _, k := range []string{"left", "right", "up", "down", "back", "pause", "fullscreen"} {
		h.pl.Key(k)
	}
	if h.fm.numCmds() != before {
		t.Errorf("keys acted on an empty player: %d new commands", h.fm.numCmds()-before)
	}
	if h.pl.HasVideo() {
		t.Error("a key press started playback")
	}
}

// A load failure must still close the session: the server must not keep
// showing us as playing, and our state must be clean afterwards.
func TestLoadFailureStopsSession(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	_, _, stopped, _ := h.recs.snapshot()
	n := len(stopped)

	// mpv reports playback-abort (a failed load) and then end-file/error.
	h.fm.SetProperty("playback-abort", true)
	h.pl.Stop()

	if h.pl.HasVideo() {
		t.Error("media still set after a failed-load stop")
	}
	_, _, stopped2, _ := h.recs.snapshot()
	if len(stopped2) != n+1 {
		t.Errorf("no stop report after a failed load (%d → %d)", n, len(stopped2))
	}
	// And no key press may act on the dead session.
	cmds := h.fm.numCmds()
	h.pl.Key("left")
	if h.fm.numCmds() != cmds {
		t.Error("a key press acted on a dead session")
	}
}

// External subtitles are re-added whenever configureStreams runs (play,
// restart, track switch). mpv's "cached" flag makes that a re-select instead
// of stacking a new copy of the same file, so the flag must be sent.
func TestExternalSubtitleUsesCachedFlag(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	sid := 4 // the fixture's external subtitle
	before := h.fm.numCmds()
	for i := 0; i < 3; i++ {
		h.pl.SetStreams(nil, &sid)
	}
	h.fm.mu.Lock()
	cmds := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if !strings.Contains(cmds, "sub-add") {
		t.Fatalf("no sub-add issued: %q", cmds)
	}
	if !strings.Contains(cmds, "cached") {
		t.Errorf("sub-add without the cached flag (would stack duplicates): %q", cmds)
	}
}

// Menu labels come from the server's DisplayTitle when present, so a track is
// recognisable instead of a bare language code.
func TestMenuLabelsUseDisplayTitle(t *testing.T) {
	cases := []struct {
		s    jfin.MediaStream
		want string
	}{
		{jfin.MediaStream{DisplayTitle: "English - ASS", Language: "eng"}, "English - ASS"},
		{jfin.MediaStream{Title: "SDH", Language: "eng"}, "SDH"},
		{jfin.MediaStream{Language: "hrv", IsExternal: true}, "hrv (external)"},
		{jfin.MediaStream{Type: "Audio", Index: 7}, "Audio 7"},
	}
	for _, c := range cases {
		if got := streamLabel(c.s); got != c.want {
			t.Errorf("streamLabel(%+v) = %q, want %q", c.s, got, c.want)
		}
	}
}

// A volume change from the remote must actually change the volume and never
// leave the player muted (the "slider does nothing, it just mutes" symptom).
func TestSetVolumeUnmutesAndChangesVolume(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("volume", 100.0)
	h.fm.SetProperty("mute", false)

	// Muted first, then move the slider: the player must become audible.
	h.pl.SetMute(true)
	h.pl.SetVolume(60)
	if h.fm.prop("mute") != false {
		t.Error("SetVolume did not unmute")
	}
	if got := h.pl.GetVolume(); got != 60 {
		t.Errorf("volume = %d, want 60", got)
	}

	// Volume 0 keeps the mute state (that is an explicit "silence" request).
	h.pl.SetMute(true)
	h.pl.SetVolume(0)
	if h.fm.prop("mute") != true {
		t.Error("SetVolume(0) unmuted the player")
	}
}

// The reported level must stay inside the 0-100 range the remote sliders use,
// even when mpv allows more.
func TestReportedVolumeIsClamped(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.fm.SetProperty("volume", 130.0) // mpv's default volume-max
	h.pl.Tick()
	_, pr, _, _ := h.recs.snapshot()
	if got := pr[len(pr)-1].VolumeLevel; got != 100 {
		t.Errorf("reported VolumeLevel = %d, want 100 (clamped)", got)
	}
}

// --- settings: rebinding, seek steps, prefs menus ---------------------------

// The keybinding table is the default set plus the user's overrides, and an
// empty action unbinds a key.
func TestKeyBindingOverrides(t *testing.T) {
	o := DefaultOptions()
	if _, ok := o.keyBindings()["c"]; !ok {
		t.Fatal("default menu key missing")
	}
	o.Keys = map[string]string{"c": "fullscreen", "x": "menu", "q": ""}
	got := o.keyBindings()
	if got["c"] != "fullscreen" {
		t.Errorf("c = %q, want fullscreen", got["c"])
	}
	if got["x"] != "menu" {
		t.Errorf("x = %q, want menu", got["x"])
	}
	if _, ok := got["q"]; ok {
		t.Error("q was explicitly unbound but is still bound")
	}
}

// The arrow keys seek by the configured steps, and exact seeks ask mpv for a
// keyframe-accurate jump.
func TestConfigurableSeekSteps(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	o := h.pl.Options()
	o.SeekLeft, o.SeekRight, o.SeekUp = -15, 30, 120
	h.pl.SetOptions(o)

	before := h.fm.numCmds()
	h.pl.Key("right")
	h.pl.Key("up")
	h.fm.mu.Lock()
	cmds := strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if !strings.Contains(cmds, "seek30relative") || !strings.Contains(cmds, "seek120relative") {
		t.Errorf("configured steps not used: %q", cmds)
	}

	o.SeekHExact = true
	h.pl.SetOptions(o)
	before = h.fm.numCmds()
	h.pl.Key("right")
	h.fm.mu.Lock()
	cmds = strings.Join(h.fm.cmds[before:], "|")
	h.fm.mu.Unlock()
	if !strings.Contains(cmds, "relative+exact") {
		t.Errorf("exact seek flag missing: %q", cmds)
	}
}

// The preferences menus render the current settings and a selection changes
// them, applies where relevant, and persists.
func TestPrefsMenusChangeSettings(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	var saved int
	// The real callback shape: read the options it is handed and write them
	// back. It must not call back into the player — that deadlocked the whole
	// app when a preference was changed from the menu.
	h.pl.SetSaveFunc(func(o Options) {
		saved++
		if o.TranscodeHDR {
			saved += 100
		}
	})

	// Drive the whole flow (open → Video Preferences → toggle → back out) from
	// a goroutine: a deadlock here is the bug this test is about.
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.pl.Key("menu")
		moveTo(h.pl, h.fm, videoPrefsTitle)
		h.pl.Key("ok")
		moveTo(h.pl, h.fm, "Transcode HDR")
		h.pl.Key("ok")
		h.pl.Key("back")
		h.pl.Key("back")
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("changing a preference deadlocked (the app froze)")
	}

	if !h.pl.Options().TranscodeHDR {
		t.Error("Transcode HDR toggle did not take")
	}
	if saved != 101 {
		t.Errorf("preference change not persisted with the new value (save called %d)", saved)
	}
}

// The root menu carries both preference menus, and re-rendering a preference
// menu shows the new state.
func TestPrefsMenuRerenders(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.pl.SetSaveFunc(func(Options) {})
	h.pl.Key("menu")
	root := h.fm.lastText()
	if !strings.Contains(root, videoPrefsTitle) || !strings.Contains(root, playerPrefsTitle) {
		t.Fatalf("prefs rows missing from the root menu:\n%s", root)
	}
	moveTo(h.pl, h.fm, videoPrefsTitle)
	h.pl.Key("ok")
	prefs := h.fm.lastText()
	if !strings.Contains(prefs, "Subtitle Size") || !strings.Contains(prefs, "Transcode HDR") {
		t.Fatalf("video prefs = %q", prefs)
	}
	moveTo(h.pl, h.fm, "Transcode HDR")
	h.pl.Key("ok")
	if got := h.fm.lastText(); !strings.Contains(got, "✔ Transcode HDR") {
		t.Errorf("prefs menu not re-rendered with the new state:\n%s", got)
	}
	// Player preferences has the behaviour toggles.
	for i := 0; i < 2; i++ { // back to the root menu
		h.pl.Key("back")
		if strings.Contains(h.fm.lastText(), "Main Menu") {
			break
		}
	}
	if !strings.Contains(h.fm.lastText(), "Main Menu") {
		t.Fatalf("did not get back to the root menu:\n%s", h.fm.lastText())
	}
	moveTo(h.pl, h.fm, playerPrefsTitle)
	h.pl.Key("ok")
	if p := h.fm.lastText(); !strings.Contains(p, "Auto Play") || !strings.Contains(p, "Enable OSC") {
		t.Errorf("player prefs = %q", p)
	}
}

// The subtitle submenu changes mpv's sub-* properties straight away.
func TestSubtitleSizeMenuAppliesToMpv(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())
	h.pl.Key("menu")
	moveTo(h.pl, h.fm, videoPrefsTitle)
	h.pl.Key("ok") // Video Preferences
	moveTo(h.pl, h.fm, "Subtitle Size")
	h.pl.Key("ok") // Subtitle Size
	if got := h.fm.lastText(); !strings.Contains(got, "Select Subtitle Size") {
		t.Fatalf("subtitle size menu = %q", got)
	}
	moveTo(h.pl, h.fm, "Huge")
	h.pl.Key("ok")
	if got := h.pl.Options().SubSize; got != 200 {
		t.Errorf("SubSize = %d, want 200", got)
	}
	if got := h.fm.prop("sub-scale"); got != "2.00" {
		t.Errorf("mpv sub-scale = %v, want 2.00", got)
	}
}

// moveTo selects a menu row by label, wrapping like the menu itself.
func moveTo(pl *Player, fm *fakeMpv, label string) {
	rows, sel := menuRows(fm.lastText())
	target := -1
	for i, r := range rows {
		if strings.Contains(r, label) {
			target = i
			break
		}
	}
	if target < 0 || sel == target {
		return
	}
	steps := (target - sel + len(rows)) % len(rows)
	key := "down"
	if steps > len(rows)/2 { // go the short way
		steps = len(rows) - steps
		key = "up"
	}
	for i := 0; i < steps; i++ {
		pl.Key(key)
	}
}

// menuRows parses the rendered menu: the entry labels and the selected index
// (the menu marks it with ** … **).
func menuRows(text string) (rows []string, selected int) {
	for i, line := range strings.Split(text, "\n") {
		if i == 0 {
			continue // the title
		}
		rows = append(rows, line)
		if strings.Contains(line, "**") {
			selected = len(rows) - 1
		}
	}
	return rows, selected
}

// indexOfRow returns the menu row index of a label (0-based, as rendered).
func indexOfRow(text, label string) int {
	rows := 0
	for i, line := range strings.Split(text, "\n") {
		if i == 0 {
			continue // the title
		}
		if strings.Contains(line, label) {
			return rows
		}
		rows++
	}
	return -1
}

// compareVersions orders dotted versions; the update check uses it.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3", "1.2.3", 0},
		{"1.3.0", "1.2.9", 1},
		{"1.2.0", "1.10.0", -1},
		{"2.0", "1.9.9", 1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestUpdateCheckFindsNewerRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"tag_name": "v9.9.9", "html_url": "https://example/releases/v9.9.9",
		})
	}))
	defer srv.Close()

	h := setup(t)
	h.pl.SetVersion("0.1.0")
	h.pl.SetUpdateURL(srv.URL)
	h.pl.SetUpdateEnabled(true)

	deadline := time.Now().Add(3 * time.Second)
	for !h.pl.HasUpdate() {
		if time.Now().After(deadline) {
			t.Fatal("update check did not report the newer release")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.pl.UpdateVersion(); got != "v9.9.9" {
		t.Errorf("UpdateVersion = %q, want v9.9.9", got)
	}
}

// The update check must read *our* release feed, and stay quiet when the repo
// has no releases yet (a 404 is the normal state for a fresh repo).
func TestUpdateCheckQuietWhenNoReleases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	h := setup(t)
	h.pl.SetVersion("0.1.0")
	h.pl.SetUpdateURL(srv.URL)
	h.pl.SetUpdateEnabled(true)
	time.Sleep(300 * time.Millisecond) // one request, no retries
	if h.pl.HasUpdate() {
		t.Errorf("update announced with no releases: %q", h.pl.UpdateVersion())
	}
}

// A tag-only feed (no release objects) is understood too.
func TestUpdateCheckFallsBackToTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]string{{"name": "v2.0.0"}})
	}))
	defer srv.Close()
	h := setup(t)
	h.pl.SetVersion("0.1.0")
	h.pl.SetUpdateURL(srv.URL + "/api/v1/repos/x/y/releases/latest")
	h.pl.SetUpdateEnabled(true)
	deadline := time.Now().Add(3 * time.Second)
	for !h.pl.HasUpdate() {
		if time.Now().After(deadline) {
			t.Fatal("tag-only feed not picked up")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := h.pl.UpdateVersion(); got != "v2.0.0" {
		t.Errorf("UpdateVersion = %q, want v2.0.0", got)
	}
}

// An older release must not be announced.
func TestUpdateCheckIgnoresOlderRelease(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v3.0.0", "html_url": "u"})
	}))
	defer srv.Close()
	h := setup(t)
	h.pl.SetVersion("9.9.9")
	h.pl.SetUpdateURL(srv.URL)
	h.pl.SetUpdateEnabled(true)
	time.Sleep(300 * time.Millisecond)
	if h.pl.HasUpdate() {
		t.Error("an older release was announced as an update")
	}
}

// The save callback runs while the player's lock is held, so it must receive
// the options rather than reading them back (that was the freeze: the callback
// re-entered Options() → self-deadlock → the whole app hung).
func TestSaveCallbackGetsOptionsWithoutReentering(t *testing.T) {
	h := setup(t)
	playOne(t, h, cfg())

	var got Options
	var before time.Time
	h.pl.SetSaveFunc(func(o Options) {
		got = o
		before = time.Now()
	})
	o := h.pl.Options()
	o.SeekRight = 42
	h.pl.SetOptions(o)
	h.pl.Key("menu")
	moveTo(h.pl, h.fm, videoPrefsTitle)
	h.pl.Key("ok")
	moveTo(h.pl, h.fm, "Subtitle Size")
	h.pl.Key("ok")
	h.pl.Key("down")
	h.pl.Key("ok") // pick a size
	if before.IsZero() {
		t.Fatal("save callback never ran")
	}
	if got.SubSize == 0 {
		t.Error("save callback received zero options")
	}
	// The values we set before opening the menu must still be there: the
	// callback copies, it does not reset.
	if got.SeekRight != 42 {
		t.Errorf("save callback lost unrelated settings: seek_right = %v", got.SeekRight)
	}
	if got.SubSize == 0 || got.SubColor == "" {
		t.Errorf("save callback got a half-filled Options: %+v", got)
	}
}

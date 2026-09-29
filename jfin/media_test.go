package jfin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }

// TestDeviceProfileGolden pins the profile JSON against the upstream
// (v2.10) static structure, with the kbps/codec knobs applied.
func TestDeviceProfileGolden(t *testing.T) {
	p, err := DeviceProfile(ProfileOpts{IsRemote: false, LocalKbps: 10000, RemoteKbps: 3000})
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{
  "Name": "Jellyfin MPV Shim",
  "MaxStreamingBitrate": 10000000,
  "MaxStaticBitrate": 10000000,
  "MusicStreamingTranscodingBitrate": 1280000,
  "TimelineOffsetSeconds": 5,
  "TranscodingProfiles": [
    {"Type": "Audio"},
    {"Container": "ts", "Type": "Video", "Protocol": "hls",
     "AudioCodec": "aac,mp3,ac3,opus,flac,vorbis",
     "VideoCodec": "h264,mpeg4,mpeg2video", "MaxAudioChannels": "6"},
    {"Container": "jpeg", "Type": "Photo"}
  ],
  "DirectPlayProfiles": [{"Type": "Video"}, {"Type": "Audio"}, {"Type": "Photo"}],
  "ResponseProfiles": [],
  "ContainerProfiles": [],
  "CodecProfiles": [],
  "SubtitleProfiles": [
    {"Format": "srt", "Method": "External"}, {"Format": "srt", "Method": "Embed"},
    {"Format": "ass", "Method": "External"}, {"Format": "ass", "Method": "Embed"},
    {"Format": "sub", "Method": "Embed"}, {"Format": "sub", "Method": "External"},
    {"Format": "ssa", "Method": "Embed"}, {"Format": "ssa", "Method": "External"},
    {"Format": "smi", "Method": "Embed"}, {"Format": "smi", "Method": "External"},
    {"Format": "pgssub", "Method": "Embed"},
    {"Format": "dvdsub", "Method": "Embed"},
    {"Format": "dvbsub", "Method": "Embed"},
    {"Format": "pgs", "Method": "Embed"}
  ]
}`
	var got, wantAny any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantAny); err != nil {
		t.Fatal(err)
	}
	gb, _ := json.Marshal(got)
	wb, _ := json.Marshal(wantAny)
	if string(gb) != string(wb) {
		t.Errorf("profile mismatch:\ngot  %s\nwant %s", gb, wb)
	}
}

func TestDeviceProfileRemoteAndH265(t *testing.T) {
	p, _ := DeviceProfile(ProfileOpts{IsRemote: true, LocalKbps: 10000, RemoteKbps: 3000})
	if p.MaxStreamingBitrate != 3000000 {
		t.Errorf("remote bitrate = %d, want 3000000", p.MaxStreamingBitrate)
	}
	p2, _ := DeviceProfile(ProfileOpts{IsRemote: false, LocalKbps: 10000, RemoteKbps: 3000, TranscodeH265: true})
	if p2.TranscodingProfiles[1].VideoCodec != "h264,h265,hevc,mpeg4,mpeg2video" {
		t.Errorf("h265 video codecs = %q", p2.TranscodingProfiles[1].VideoCodec)
	}
	p3, _ := DeviceProfile(ProfileOpts{IsRemote: false, LocalKbps: 10000, RemoteKbps: 3000,
		TranscodeH265: true, ForceH264: true})
	if p3.TranscodingProfiles[1].VideoCodec != "h264,mpeg4,mpeg2video" {
		t.Errorf("force_h264 should drop h265, got %q", p3.TranscodingProfiles[1].VideoCodec)
	}
}

// fakeServer serves the two REST endpoints the media pipeline hits.
type fakeServer struct {
	*httptest.Server
	item         Item
	sources      []MediaSource
	playInfoHits int
}

func (f *fakeServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/Items/"+f.item.ID):
			json.NewEncoder(w).Encode(f.item)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/PlaybackInfo"):
			f.playInfoHits++
			json.NewEncoder(w).Encode(PlaybackInfo{
				MediaSources:  f.sources,
				PlaySessionId: "sess-1",
			})
		default:
			http.NotFound(w, r)
		}
	}
}

func (f *fakeServer) client() *Client {
	return &Client{Base: f.Server.URL, UserID: "u1", DeviceID: "dev1", http: http.DefaultClient}
}

func newTestMedia(t *testing.T, c *Client, itemID string, local bool) *Media {
	t.Helper()
	m := &Media{
		C:     c,
		Cfg:   MediaConfig{LocalKbps: 10000, RemoteKbps: 3000},
		Queue: []PlaylistItem{{PlaylistItemId: "p1", ID: itemID}},
		Seq:   0, UserID: "u1", IsLocal: local,
	}
	m.Video = &Video{M: m, ID: itemID, Item: &Item{ID: itemID, Name: "Movie", Type: "Movie"}}
	return m
}

func mustURL(t *testing.T, v *Video) string {
	t.Helper()
	u, err := v.PlaybackURL(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestURLDirectStream(t *testing.T) {
	f := &fakeServer{item: Item{ID: "i1"}}
	f.sources = []MediaSource{{ID: "src1", Protocol: "File", Path: "/nope/nothere.mkv", SupportsDirectStream: true, Bitrate: 8000000}}
	f.Server = httptest.NewServer(f.handler())
	defer f.Server.Close()
	m := newTestMedia(t, f.client(), "i1", false)
	u := mustURL(t, m.Video)
	want := f.Server.URL + "/Videos/i1/stream?MediaSourceId=src1&static=true"
	if u != want {
		t.Errorf("url = %q, want %q", u, want)
	}
	if m.Video.IsTranscode {
		t.Error("direct stream marked as transcode")
	}
}

func TestURLTranscode(t *testing.T) {
	f := &fakeServer{item: Item{ID: "i1"}}
	f.sources = []MediaSource{{ID: "src1", Protocol: "Http-Hls", SupportsTranscoding: true, Bitrate: 5000000,
		TranscodingUrl: "/Videos/i1/stream.hls?Static=true&MediaSourceId=src1&ProfileId=VideoHls"}}
	f.Server = httptest.NewServer(f.handler())
	defer f.Server.Close()
	m := newTestMedia(t, f.client(), "i1", false)
	u := mustURL(t, m.Video)
	want := f.Server.URL + "/Videos/i1/stream.hls?Static=true&MediaSourceId=src1&ProfileId=VideoHls"
	if u != want {
		t.Errorf("url = %q, want %q", u, want)
	}
	if !m.Video.IsTranscode {
		t.Error("hls source not marked transcode")
	}
}

func TestURLDirectFileLocal(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "movie.mkv")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{item: Item{ID: "i1"}}
	f.sources = []MediaSource{{ID: "src1", Protocol: "File", Path: file, SupportsDirectPlay: true, Bitrate: 8000000}}
	f.Server = httptest.NewServer(f.handler())
	defer f.Server.Close()
	m := newTestMedia(t, f.client(), "i1", true)
	u := mustURL(t, m.Video)
	if u != file {
		t.Errorf("url = %q, want %q", u, file)
	}
}

func TestURLFallbackLoop(t *testing.T) {
	f := &fakeServer{item: Item{ID: "i1"}}
	f.sources = []MediaSource{
		{ID: "src1", Protocol: "File", Path: "/nope/absent.mkv", Bitrate: 8000000}, // unplayable
		{ID: "src2", Protocol: "File", Path: "/nope/absent.mkv", SupportsDirectStream: true, Bitrate: 8000000},
	}
	f.Server = httptest.NewServer(f.handler())
	defer f.Server.Close()
	m := newTestMedia(t, f.client(), "i1", false)
	u := mustURL(t, m.Video)
	want := f.Server.URL + "/Videos/i1/stream?MediaSourceId=src2&static=true"
	if u != want {
		t.Errorf("url = %q, want %q", u, want)
	}
	if m.Video.MediaSource.ID != "src2" {
		t.Errorf("source = %q, want src2", m.Video.MediaSource.ID)
	}
}

func TestPickMediaSource(t *testing.T) {
	sources := []MediaSource{
		{ID: "a", Bitrate: 5000000},
		{ID: "b", Bitrate: 2000000, SupportsDirectPlay: true},
		{ID: "c", Bitrate: 9000000},
	}
	got, err := PickMediaSource(sources, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != "b" { // direct play bonus beats higher bitrate
		t.Errorf("picked %q, want b", got.ID)
	}
	got, _ = PickMediaSource(sources, "c")
	if got.ID != "c" {
		t.Errorf("preferred = %q, want c", got.ID)
	}
	if _, err := PickMediaSource(nil, ""); err == nil {
		t.Error("expected error for empty sources")
	}
}

func TestMapStreams(t *testing.T) {
	c := &Client{Base: "http://s:8096", UserID: "u1", DeviceID: "dev"}
	m := newTestMedia(t, c, "i1", false)
	m.Video.MediaSource = &MediaSource{
		ID: "src1", Protocol: "File",
		MediaStreams: []MediaStream{
			{Index: 1, Type: "Audio"},
			{Index: 2, Type: "Audio", IsExternal: true},
			{Index: 3, Type: "Subtitle", DeliveryMethod: "Embed"},
			{Index: 4, Type: "Subtitle", DeliveryMethod: "External", DeliveryUrl: "/sub/4.srt"},
			{Index: 5, Type: "Subtitle", DeliveryMethod: "Encode"},
		},
	}
	m.Video.MapStreams()
	// External audio takes the *next embedded* slot (upstream semantics).
	if m.Video.AudioSeq[1] != 1 || m.Video.AudioSeq[2] != 2 {
		t.Errorf("audioSeq = %v", m.Video.AudioSeq)
	}
	if m.Video.SubtitleSeq[3] != 1 {
		t.Errorf("subtitleSeq[3] = %v", m.Video.SubtitleSeq)
	}
	if got := m.Video.SubtitleURL[4]; got != "http://s:8096/sub/4.srt" {
		t.Errorf("subtitleURL = %q", got)
	}
	if _, ok := m.Video.SubtitleEnc[5]; !ok {
		t.Error("subtitle 5 should be Encode")
	}
	// Defaults applied when unset.
	d := 7
	m.Video.MediaSource.DefaultSubtitleStreamIndex = &d
	m.Video.MapStreams()
	if m.Video.Sid == nil || *m.Video.Sid != 7 {
		t.Errorf("sid default = %v, want 7", m.Video.Sid)
	}
}

func TestQueueTransitions(t *testing.T) {
	c := &Client{Base: "http://s:8096", UserID: "u1", DeviceID: "dev"}
	m := newTestMedia(t, c, "i1", false)
	m.Queue = []PlaylistItem{{"p1", "i1"}, {"p2", "i2"}, {"p3", "i3"}}
	m.Seq = 0
	if !m.HasNext() || m.HasPrev() {
		t.Fatal("initial position flags wrong")
	}
	m.Insert([]string{"i4"}, false)      // PlayNext: after current
	m.Insert([]string{"i5", "i6"}, true) // PlayLast: at end
	want := []string{"i1", "i4", "i2", "i3", "i5", "i6"}
	var got []string
	for _, q := range m.Queue {
		got = append(got, q.ID)
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("queue = %v, want %v", got, want)
	}
}

func TestProperTitle(t *testing.T) {
	c := &Client{Base: "http://s:8096", UserID: "u1", DeviceID: "dev"}
	m := newTestMedia(t, c, "i1", false)
	y := 2020
	m.Video.Item.ProductionYear = &y
	if got := m.Video.ProperTitle(); got != "Movie (2020)" {
		t.Errorf("movie title = %q", got)
	}
	m.Video.IsTV = true
	m.Video.Item.SeriesName = "Show"
	e1, e2 := 2, 10
	m.Video.Item.IndexNumber, m.Video.Item.ParentIndexNumber = &e1, &e2
	if got := m.Video.ProperTitle(); got != "Show - s10e02 - Movie" {
		t.Errorf("episode title = %q", got)
	}
	m.Video.IsTranscode = true
	if got := m.Video.ProperTitle(); got != "Show - s10e02 - Movie (Transcode)" {
		t.Errorf("episode title transcode = %q", got)
	}
}

func TestHasScheme(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"C:\\videos\\x.mkv", false},
		{"/mnt/nas/x.mkv", false},
		{"smb://nas/share/x.mkv", true},
		{"file:///mnt/x.mkv", true},
		{"http://192.168.1.2:8096/x.mkv", true},
	}
	for _, tc := range cases {
		if got := hasScheme(tc.s); got != tc.want {
			t.Errorf("hasScheme(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

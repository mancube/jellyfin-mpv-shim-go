package jfin

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync/atomic"
)

// MediaConfig carries the playback-relevant settings the media pipeline
// needs. main maps Settings → MediaConfig at startup.
type MediaConfig struct {
	LocalKbps     int
	RemoteKbps    int
	TranscodeH265 bool
	ForceH264     bool
	SkipIntro     bool
	SkipCredits   bool

	// Device-profile codec knobs (upstream transcode_*/force_*_codec).
	AlwaysTranscode      bool
	TranscodeHi10p       bool
	TranscodeHDR         bool
	TranscodeDolbyVision bool
	TranscodeHEVC        bool
	TranscodeAV1         bool
	Transcode4K          bool
	ForceVideoCodec      string
	ForceAudioCodec      string

	// DirectPaths serves a local file for a remote server;
	// PathSubstitutions maps a server path prefix to a local one.
	DirectPaths       bool
	RemoteDirectPaths bool
	PathSubstitutions map[string]string

	// Language selection: LangFilterAudio/LangFilterSub are comma lists
	// ("und,eng,jpn"), LanguageRules is the ordered upstream rule list.
	LangFilterAudio string
	LangFilterSub   string
	LanguageRules   []LanguageRule
}

// LanguageRule is one ordered auto-track rule (upstream language_config).
type LanguageRule struct {
	AudioLang string
	SubLang   string
	AudioNone bool
	SubNone   bool
	Enabled   bool
	Priority  int
	Note      string
}

// playlistItemID mints unique PlaylistItemIds (upstream get_seq).
var seqNum atomic.Int64

func playlistItemID() string { return fmt.Sprintf("playlistItem%d", seqNum.Add(1)) }

// Media is one playback session: the playlist (raw queue) plus the position
// within it. Port of upstream media.Media.
type Media struct {
	C       *Client
	Cfg     MediaConfig
	Queue   []PlaylistItem
	Seq     int
	UserID  string
	Video   *Video
	IsLocal bool
}

// NewMedia builds a playlist from raw item ids, starting at seq (clamped).
// The current item is fetched from the server. Port of upstream Media.
func NewMedia(ctx context.Context, c *Client, cfg MediaConfig, itemIDs []string, seq int, userID string, aid, sid *int, srcID *string) (*Media, error) {
	if len(itemIDs) == 0 {
		return nil, errors.New("jfin: empty queue")
	}
	queue := make([]PlaylistItem, len(itemIDs))
	for i, id := range itemIDs {
		queue[i] = PlaylistItem{PlaylistItemId: playlistItemID(), ID: id}
	}
	if seq < 0 {
		seq = 0
	}
	if seq >= len(queue) {
		seq = len(queue) - 1
	}
	m := &Media{C: c, Cfg: cfg, Queue: queue, Seq: seq, UserID: userID, IsLocal: c.IsLocal(ctx)}
	v, err := NewVideo(ctx, m, queue[seq].ID, aid, sid, srcID)
	if err != nil {
		return nil, err
	}
	m.Video = v
	return m, nil
}

func (m *Media) HasNext() bool { return m.Seq < len(m.Queue)-1 }
func (m *Media) HasPrev() bool { return m.Seq > 0 }

// At returns the queue entry at seq (its Video is fetched on demand).
func (m *Media) At(ctx context.Context, seq int) (*Media, error) {
	if seq < 0 || seq >= len(m.Queue) {
		return nil, nil
	}
	return m.at(ctx, seq)
}

func (m *Media) at(ctx context.Context, seq int) (*Media, error) {
	v, err := NewVideo(ctx, m, m.Queue[seq].ID, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	return &Media{C: m.C, Cfg: m.Cfg, Queue: m.Queue, Seq: seq, UserID: m.UserID, Video: v, IsLocal: m.IsLocal}, nil
}

// Next/Prev return a sibling Media for the following/preceding queue entry
// (its Video is fetched on demand, like upstream get_next/get_prev).
func (m *Media) Next(ctx context.Context) (*Media, error) {
	if !m.HasNext() {
		return nil, nil
	}
	return m.at(ctx, m.Seq+1)
}

func (m *Media) Prev(ctx context.Context) (*Media, error) {
	if !m.HasPrev() {
		return nil, nil
	}
	return m.at(ctx, m.Seq-1)
}

// GetFromKey locates a queue entry by item id (remote "PlayMediaSource").
func (m *Media) GetFromKey(ctx context.Context, id string) (*Media, error) {
	for i := range m.Queue {
		if m.Queue[i].ID == id {
			return m.at(ctx, i)
		}
	}
	return nil, nil
}

// Insert adds ids to the queue: right after the current position
// (atEnd=false, upstream "PlayNext") or at the end (atEnd=true, "PlayLast").
// Port of upstream insert_items.
func (m *Media) Insert(ids []string, atEnd bool) {
	items := make([]PlaylistItem, len(ids))
	for i, id := range ids {
		items[i] = PlaylistItem{PlaylistItemId: playlistItemID(), ID: id}
	}
	if atEnd {
		m.Queue = append(m.Queue, items...)
		return
	}
	n := make([]PlaylistItem, 0, len(m.Queue)+len(items))
	n = append(n, m.Queue[:m.Seq+1]...)
	n = append(n, items...)
	n = append(n, m.Queue[m.Seq+1:]...)
	m.Queue = n
}

// Video is one item: fetched metadata + the playback decision state
// (media source, URL kind, stream maps). Port of upstream media.Video.
type Video struct {
	M            *Media
	ID           string
	Aid          *int // requested/active audio stream (Jellyfin index), nil = default
	Sid          *int // requested/active subtitle stream (Jellyfin index)
	SrcID        *string
	Item         *Item
	IsTV         bool
	IsTranscode  bool
	PlaybackInfo *PlaybackInfo
	MediaSource  *MediaSource
	// Track maps: Jellyfin stream index → mpv track index (and back).
	AudioSeq    map[int]int
	AudioUid    map[int]int
	SubtitleSeq map[int]int
	SubtitleUid map[int]int
	SubtitleURL map[int]string
	SubtitleEnc map[int]struct{}
	// Intros fetched from MediaSegments, used to skip intro/credits.
	Intros []Intro
	// Chapters fetched lazily (upstream get_chapters), used by the OSD menu.
	Chapters   []Chapter
	introTried bool
}

func NewVideo(ctx context.Context, m *Media, id string, aid, sid *int, srcID *string) (*Video, error) {
	item, err := m.C.GetItem(ctx, id)
	if err != nil {
		return nil, err
	}
	return &Video{
		M: m, ID: id, Aid: aid, Sid: sid, SrcID: srcID,
		Item: item, IsTV: item.Type == "Episode",
		AudioSeq: map[int]int{}, AudioUid: map[int]int{},
		SubtitleSeq: map[int]int{}, SubtitleUid: map[int]int{},
		SubtitleURL: map[int]string{}, SubtitleEnc: map[int]struct{}{},
	}, nil
}

func (v *Video) GetDuration() float64 {
	if v.Item.RunTimeTicks != nil {
		return float64(*v.Item.RunTimeTicks) / 1e7
	}
	return 0
}

// ProperTitle is the display title (force-media-title). Port of upstream
// get_proper_title, sans i18n.
func (v *Video) ProperTitle() string {
	title := v.Item.Name
	if v.IsTV && v.Item.IndexNumber != nil && v.Item.ParentIndexNumber != nil {
		title = fmt.Sprintf("%s - s%de%.2d - %s", v.Item.SeriesName, *v.Item.ParentIndexNumber, *v.Item.IndexNumber, v.Item.Name)
	} else if v.Item.Type == "Movie" && v.Item.ProductionYear != nil {
		title = fmt.Sprintf("%s (%d)", v.Item.Name, *v.Item.ProductionYear)
	}
	if v.IsTranscode {
		title += " (Transcode)"
	}
	return title
}

// PlaybackURL resolves the URL to load into mpv: PlaybackInfo request, media
// source pick, URL building (3 cases), and the fallback loop over remaining
// sources. Port of upstream get_playback_url.
func (v *Video) PlaybackURL(ctx context.Context) (string, error) {
	v.TerminateTranscode(ctx)
	m := v.M
	profile, err := DeviceProfile(ProfileOpts{
		IsRemote:             !m.IsLocal,
		VideoBitrate:         nil, // no per-session bitrate override
		LocalKbps:            m.Cfg.LocalKbps,
		RemoteKbps:           m.Cfg.RemoteKbps,
		TranscodeH265:        m.Cfg.TranscodeH265,
		ForceH264:            m.Cfg.ForceH264,
		AlwaysTranscode:      m.Cfg.AlwaysTranscode,
		TranscodeHi10p:       m.Cfg.TranscodeHi10p,
		TranscodeHDR:         m.Cfg.TranscodeHDR,
		TranscodeDolbyVision: m.Cfg.TranscodeDolbyVision,
		TranscodeHEVC:        m.Cfg.TranscodeHEVC,
		TranscodeAV1:         m.Cfg.TranscodeAV1,
		Transcode4K:          m.Cfg.Transcode4K,
		ForceVideoCodec:      m.Cfg.ForceVideoCodec,
		ForceAudioCodec:      m.Cfg.ForceAudioCodec,
	})
	if err != nil {
		return "", err
	}
	req := &PlaybackRequest{
		UserID:             m.C.UserID,
		DeviceProfile:      profile,
		AutoOpenLiveStream: true,
		IsPlayback:         true,
	}
	// Upstream sends the indices when truthy (0 means "not set").
	if v.Aid != nil && *v.Aid != 0 {
		req.AudioStreamIndex = v.Aid
	}
	if v.Sid != nil && *v.Sid != 0 {
		req.SubtitleStreamIndex = v.Sid
	}
	if v.SrcID != nil {
		req.MediaSourceID = v.SrcID
	}
	pi, err := m.C.GetPlaybackInfo(ctx, v.ID, req)
	if err != nil {
		return "", err
	}
	v.PlaybackInfo = pi
	ms, err := PickMediaSource(pi.MediaSources, srcStr(v.SrcID))
	if err != nil {
		return "", err
	}
	v.MediaSource = &ms
	if m.Cfg.SkipIntro || m.Cfg.SkipCredits {
		v.GetIntro(ctx, v.MediaSource.ID)
	}
	v.MapStreams()
	v.applyLanguageRules()
	url := v.urlFromSource()
	// If the picked source is unplayable, try the rest (upstream fallback
	// loop).
	if url == "" && len(pi.MediaSources) > 1 {
		for i := range pi.MediaSources {
			if v.SrcID != nil && pi.MediaSources[i].ID == *v.SrcID {
				continue
			}
			ms := pi.MediaSources[i]
			v.MediaSource = &ms
			v.MapStreams()
			if u := v.urlFromSource(); u != "" {
				url = u
				break
			}
		}
	}
	return url, nil
}

func srcStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// PickMediaSource picks the media source: the preferred one when present,
// else the highest SupportsDirectPlay*50000 + Bitrate/1000. Port of upstream
// get_best_media_source.
func PickMediaSource(sources []MediaSource, preferred string) (MediaSource, error) {
	if len(sources) == 0 {
		return MediaSource{}, errors.New("jfin: no media sources")
	}
	var (
		preferredMS *MediaSource
		selected    *MediaSource
		weight      float64
	)
	for i := range sources {
		ms := &sources[i]
		if preferred != "" && ms.ID == preferred {
			preferredMS = ms
		}
		w := ms.Bitrate / 1000
		if ms.SupportsDirectPlay {
			w += 50000
		}
		if w > weight {
			weight, selected = w, ms
		}
	}
	if preferredMS != nil {
		return *preferredMS, nil
	}
	if selected == nil { // degenerate: every source had zero weight (upstream returns None here)
		return MediaSource{}, errors.New("jfin: no playable media source")
	}
	return *selected, nil
}

// urlFromSource builds the load URL from the current media source. Port of
// upstream _get_url_from_source. Direct file paths are only considered for
// LAN servers (upstream's direct_paths/remote_direct_paths default off; the
// LAN case is this port's main target).
func (v *Video) urlFromSource() string {
	ms := v.MediaSource
	base := v.M.C.Base
	if ms == nil {
		return ""
	}
	// Local file paths: always for a LAN server, and for a remote one when
	// direct_paths/remote_direct_paths is on (upstream direct_paths +
	// path_substitutions).
	if ms.Path != "" {
		local := v.M.IsLocal
		substituted := v.substitutePath(ms.Path)
		if !local && substituted != ms.Path {
			local = v.M.Cfg.DirectPaths || v.M.Cfg.RemoteDirectPaths
		}
		if local {
			if p, ok := directPath(substituted); ok {
				v.IsTranscode = false
				return p
			}
		}
	}
	if ms.SupportsDirectStream {
		v.IsTranscode = false
		q := url.Values{}
		q.Set("static", "true")
		q.Set("MediaSourceId", ms.ID)
		if ms.LiveStreamId != "" {
			q.Set("LiveStreamId", ms.LiveStreamId)
		}
		return fmt.Sprintf("%s/Videos/%s/stream?%s", base, v.ID, q.Encode())
	}
	if ms.SupportsTranscoding && ms.TranscodingUrl != "" {
		v.IsTranscode = true
		return base + ms.TranscodingUrl
	}
	return ""
}

// applyLanguageRules picks aid/sid from the ordered language rules, then falls
// back to the first stream whose language passes the filters. Port of upstream
// language_config + lang_filter_audio/sub.
func (v *Video) applyLanguageRules() {
	ms := v.MediaSource
	if ms == nil {
		return
	}
	// 1) explicit rules, highest priority first, first match wins.
	rules := append([]LanguageRule(nil), v.M.Cfg.LanguageRules...)
	sort.SliceStable(rules, func(i, j int) bool { return rules[i].Priority > rules[j].Priority })
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		matched := false
		if r.AudioLang != "" || r.AudioNone {
			if v.pickLanguage(r.AudioLang, r.AudioNone, "Audio") {
				matched = true
			}
		}
		if r.SubLang != "" || r.SubNone {
			if v.pickLanguage(r.SubLang, r.SubNone, "Subtitle") {
				matched = true
			}
		}
		if matched {
			return
		}
	}
	// 2) no rule matched: apply the language filters, if configured. The list
	// is a *filter* ("which languages may be used"), so we keep the first
	// stream in file order whose language is in it.
	if list := v.M.Cfg.LangFilterAudio; list != "" {
		v.pickLanguage(list, false, "Audio")
	}
	if list := v.M.Cfg.LangFilterSub; list != "" {
		if v.pickLanguage(list, false, "Subtitle") {
			return
		}
		// Nothing matched: turn subtitles off, as upstream does.
		off := -1
		v.Sid = &off
	}
}

// pickLanguage sets aid/sid to the first playable stream of the given type
// whose language is in the list (or the "und" fallback). Returns whether it
// found one. kind is "Audio" or "Subtitle".
func (v *Video) pickLanguage(lang string, none bool, kind string) bool {
	ms := v.MediaSource
	if ms == nil {
		return false
	}
	// "und" matches streams with no language tag.
	var wanted []string
	if lang != "" {
		for _, l := range strings.Split(lang, ",") {
			l = strings.TrimSpace(strings.ToLower(l))
			if l != "" {
				wanted = append(wanted, l)
			}
		}
	}
	if none && len(wanted) == 0 {
		if kind == "Subtitle" {
			off := -1
			v.Sid = &off
		}
		return true
	}
	for _, s := range ms.MediaStreams {
		if s.Type != kind {
			continue
		}
		l := strings.ToLower(s.Language)
		for _, w := range wanted {
			if w == l || (w == "und" && l == "") {
				idx := s.Index
				if kind == "Audio" {
					v.Aid = &idx
				} else {
					v.Sid = &idx
				}
				return true
			}
		}
	}
	return false
}

// substitutePath rewrites a server path with the configured
// path_substitutions map (longest prefix wins), returning "" when nothing
// matched. Port of upstream _apply_path_substitutions.
func (v *Video) substitutePath(path string) string {
	if path == "" {
		return path
	}
	if len(v.M.Cfg.PathSubstitutions) == 0 {
		return path // no rules: unchanged
	}
	best := ""
	original := path
	for from, to := range v.M.Cfg.PathSubstitutions {
		if from == "" || !strings.HasPrefix(original, from) {
			continue
		}
		if len(from) > len(best) {
			best, path = from, strings.Replace(original, from, to, 1)
		}
	}
	return path
}

// directPath applies the upstream local-path rules: a path with a URI scheme
// is used as-is, a bare path must exist as a local file.
func directPath(p string) (string, bool) {
	if hasScheme(p) {
		return p, true
	}
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
		return p, true
	}
	return "", false
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isAlnum(c byte) bool {
	return isAlpha(c) || (c >= '0' && c <= '9')
}

// hasScheme mirrors urllib.parse.urlparse scheme detection, minus the
// single-character Windows drive letters ("C:\..." is not a scheme).
func hasScheme(p string) bool {
	i := strings.IndexByte(p, ':')
	if i < 2 || i > 32 {
		return false
	}
	scheme := p[:i]
	if !isAlpha(scheme[0]) {
		return false
	}
	for j := 1; j < len(scheme); j++ {
		c := scheme[j]
		if !isAlnum(c) && c != '+' && c != '-' && c != '.' {
			return false
		}
	}
	return true
}

// MapStreams builds the Jellyfin ↔ mpv track index maps. Port of upstream
// map_streams (the language_config override is out of scope here).
func (v *Video) MapStreams() {
	v.AudioSeq = map[int]int{}
	v.AudioUid = map[int]int{}
	v.SubtitleSeq = map[int]int{}
	v.SubtitleUid = map[int]int{}
	v.SubtitleURL = map[int]string{}
	v.SubtitleEnc = map[int]struct{}{}
	ms := v.MediaSource
	if ms == nil || ms.Protocol != "File" {
		return
	}
	index := 1
	for _, s := range ms.MediaStreams {
		if s.Type != "Audio" {
			continue
		}
		v.AudioUid[index] = s.Index
		v.AudioSeq[s.Index] = index
		if !s.IsExternal {
			index++
		}
	}
	index = 1
	for _, s := range ms.MediaStreams {
		if s.Type != "Subtitle" {
			continue
		}
		switch s.DeliveryMethod {
		case "Embed":
			v.SubtitleUid[index] = s.Index
			v.SubtitleSeq[s.Index] = index
		case "External":
			u := s.DeliveryUrl
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				u = v.M.C.Base + u
			}
			v.SubtitleURL[s.Index] = u
		case "Encode":
			v.SubtitleEnc[s.Index] = struct{}{}
		}
		if !s.IsExternal {
			index++
		}
	}
	if ms.DefaultAudioStreamIndex != nil && v.Aid == nil {
		v.Aid = ms.DefaultAudioStreamIndex
	}
	if ms.DefaultSubtitleStreamIndex != nil && v.Sid == nil {
		v.Sid = ms.DefaultSubtitleStreamIndex
	}
}

// SetStreams updates the requested aid/sid (either may be nil = unchanged)
// and reports whether playback must restart: a transcode audio switch, or an
// Encode-method subtitle. Port of upstream set_streams.
func (v *Video) SetStreams(aid, sid *int) bool {
	need := false
	if aid != nil && (v.Aid == nil || *v.Aid != *aid) {
		v.Aid = aid
		if v.IsTranscode {
			need = true
		}
	}
	if sid != nil && (v.Sid == nil || *v.Sid != *sid) {
		v.Sid = sid
		if _, ok := v.SubtitleEnc[*sid]; ok {
			need = true
		}
	}
	return need
}

// TerminateTranscode tears down any transcode session tied to this video
// (DELETE ActiveEncodings, or close the live stream). Port of upstream
// terminate_transcode.
func (v *Video) TerminateTranscode(ctx context.Context) {
	if !v.IsTranscode {
		return
	}
	if v.MediaSource != nil && v.MediaSource.LiveStreamId != "" {
		_ = v.M.C.CloseLiveStream(ctx, v.MediaSource.LiveStreamId)
		return
	}
	if v.PlaybackInfo != nil && v.PlaybackInfo.PlaySessionId != "" {
		_ = v.M.C.CloseTranscode(ctx, v.PlaybackInfo.PlaySessionId)
	}
}

// GetIntro fetches intro/outro segments once. Port of upstream get_intro.
func (v *Video) GetIntro(ctx context.Context, sourceID string) error {
	if v.introTried {
		return nil
	}
	v.introTried = true
	segs, err := v.M.C.MediaSegments(ctx, sourceID)
	if err != nil {
		return err
	}
	for _, s := range segs {
		v.Intros = append(v.Intros, Intro{
			Type:  s.Type,
			Start: float64(s.StartTicks) / 1e7,
			End:   float64(s.EndTicks) / 1e7,
		})
	}
	return nil
}

// GetItem fetches one item's metadata (server default field set).
func (c *Client) GetItem(ctx context.Context, id string) (*Item, error) {
	var item Item
	err := c.Get(ctx, "/Users/"+c.UserID+"/Items/"+url.PathEscape(id), &item)
	return &item, err
}

// GetItemChapters fetches the chapter markers for an item (upstream
// get_chapters). They are not in the default field set.
func (c *Client) GetItemChapters(ctx context.Context, id string) ([]Chapter, error) {
	var item Item
	path := "/Users/" + c.UserID + "/Items/" + url.PathEscape(id) + "?Fields=Chapters"
	if err := c.Get(ctx, path, &item); err != nil {
		return nil, err
	}
	out := make([]Chapter, 0, len(item.Chapters))
	for _, ch := range item.Chapters {
		if ch.ImageTag == "" {
			continue // no preview image: upstream skips these too
		}
		out = append(out, Chapter{Name: ch.Name, StartTicks: ch.StartPositionTicks, ImageTag: ch.ImageTag})
	}
	return out, nil
}

// PlaybackRequest is the POST /Items/{id}/PlaybackInfo body.
type PlaybackRequest struct {
	UserID              string   `json:"UserId"`
	DeviceProfile       *Profile `json:"DeviceProfile"`
	AudioStreamIndex    *int     `json:"AudioStreamIndex,omitempty"`
	SubtitleStreamIndex *int     `json:"SubtitleStreamIndex,omitempty"`
	MediaSourceID       *string  `json:"MediaSourceId,omitempty"`
	StartTimeTicks      *int64   `json:"StartTimeTicks,omitempty"`
	AutoOpenLiveStream  bool     `json:"AutoOpenLiveStream"`
	IsPlayback          bool     `json:"IsPlayback"`
}

// GetPlaybackInfo asks the server how to play the item for our profile.
func (c *Client) GetPlaybackInfo(ctx context.Context, itemID string, req *PlaybackRequest) (*PlaybackInfo, error) {
	var pi PlaybackInfo
	err := c.Post(ctx, "/Items/"+url.PathEscape(itemID)+"/PlaybackInfo", req, &pi)
	return &pi, err
}

// MediaSegments fetches intro/outro windows for a media source
// (skip-intro plugin; 404/501 when the plugin is absent).
func (c *Client) MediaSegments(ctx context.Context, sourceID string) ([]MediaSegment, error) {
	var res struct {
		Items []MediaSegment `json:"Items"`
	}
	path := "/MediaSegments/" + url.PathEscape(sourceID) +
		"?includeSegmentTypes=Intro&includeSegmentTypes=Outro"
	if err := c.Get(ctx, path, &res); err != nil {
		return nil, err
	}
	return res.Items, nil
}

// IsLocal reports whether the server resolves to a private (LAN) address, so
// bitrate caps can be relaxed. Port of the core of upstream is_local_domain
// (the WAN-hairpin/IPv6 heuristics are out of scope).
func (c *Client) IsLocal(ctx context.Context) bool {
	host := c.Base
	if i := strings.Index(host, "://"); i >= 0 {
		host = host[i+3:]
	}
	if i := strings.IndexAny(host, "/?"); i >= 0 {
		host = host[:i]
	}
	if i := strings.LastIndexByte(host, ':'); i >= 0 && !strings.Contains(host[i+1:], "]") {
		host = host[:i] // strip :port (not IPv6-in-brackets)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if a.IP.IsPrivate() || a.IP.IsLoopback() || a.IP.IsLinkLocalUnicast() {
			return true
		}
	}
	return false
}

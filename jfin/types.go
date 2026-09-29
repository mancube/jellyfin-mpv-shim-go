package jfin

// User is the subset of the user object we use.
type User struct {
	ID   string `json:"Id"`
	Name string `json:"Name"`
}

// LoginResponse is the /Users/AuthenticateByName payload.
type LoginResponse struct {
	AccessToken string `json:"AccessToken"`
	User        User   `json:"User"`
	ServerName  string `json:"ServerName"`
}

// Item is the subset of the item DTO used by the media pipeline. All fields
// are in the server's default item field set, so no Fields= is needed.
type Item struct {
	ID                string `json:"Id"`
	Name              string `json:"Name"`
	Type              string `json:"Type"`
	IndexNumber       *int   `json:"IndexNumber"`
	ParentIndexNumber *int   `json:"ParentIndexNumber"`
	SeriesName        string `json:"SeriesName"`
	ProductionYear    *int   `json:"ProductionYear"`
	RunTimeTicks      *int64 `json:"RunTimeTicks"`
}

// MediaStream is one audio/subtitle stream of a MediaSource.
type MediaStream struct {
	Index           int    `json:"Index"`
	Type            string `json:"Type"` // "Audio" | "Subtitle"
	Title           string `json:"Title"`
	Language        string `json:"Language"`
	IsExternal      bool   `json:"IsExternal"`
	DeliveryMethod  string `json:"DeliveryMethod"` // "Embed" | "External" | "Encode"
	DeliveryUrl     string `json:"DeliveryUrl"`
}

// MediaSource is one playable source of an item (a container on disk, or a
// transcode).
type MediaSource struct {
	ID                           string        `json:"Id"`
	Protocol                     string        `json:"Protocol"` // "File" | "Http" | "Http-Hls"
	Path                         string        `json:"Path"`
	SupportsDirectPlay           bool          `json:"SupportsDirectPlay"`
	SupportsDirectStream         bool          `json:"SupportsDirectStream"`
	SupportsTranscoding          bool          `json:"SupportsTranscoding"`
	TranscodingUrl               string        `json:"TranscodingUrl"`
	Bitrate                      float64       `json:"Bitrate"`
	MediaStreams                 []MediaStream `json:"MediaStreams"`
	LiveStreamId                 string        `json:"LiveStreamId"`
	DefaultAudioStreamIndex      *int          `json:"DefaultAudioStreamIndex"`
	DefaultSubtitleStreamIndex   *int          `json:"DefaultSubtitleStreamIndex"`
}

// PlaybackInfo is the POST /Items/{id}/PlaybackInfo response.
type PlaybackInfo struct {
	MediaSources  []MediaSource `json:"MediaSources"`
	PlaySessionId string        `json:"PlaySessionId"`
	LiveStreamId  string        `json:"LiveStreamId"`
}

// PlaylistItem is one entry of the NowPlayingQueue.
type PlaylistItem struct {
	PlaylistItemId string `json:"PlaylistItemId"`
	ID             string `json:"Id"`
}

// MediaSegment is one intro/outro window (GET /MediaSegments/{sourceId}).
type MediaSegment struct {
	Type       string `json:"Type"` // "Intro" | "Outro"
	StartTicks int64  `json:"StartTicks"`
	EndTicks   int64  `json:"EndTicks"`
}

// Intro is a media segment in seconds (port of upstream media.Intro).
type Intro struct {
	Type        string  `json:"Type"`
	Start       float64 `json:"Start"`
	End         float64 `json:"End"`
	HasTriggered bool   `json:"-"`
}

// PlayRequest is the WS "Play" event payload.
type PlayRequest struct {
	PlayCommand        string   `json:"PlayCommand"` // "PlayNow" | "PlayNext" | "PlayLast"
	ItemIDs            []string `json:"ItemIds"`
	StartIndex         *int     `json:"StartIndex"`
	ControllingUserID  string   `json:"ControllingUserId"`
	AudioStreamIndex   *int     `json:"AudioStreamIndex"`
	SubtitleStreamIndex *int    `json:"SubtitleStreamIndex"`
	MediaSourceID      *string  `json:"MediaSourceId"`
	StartPositionTicks *int64   `json:"StartPositionTicks"`
}

// BufferedRange is one element of SessionInfo.BufferedRanges.
type BufferedRange struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// SessionInfo is the payload for POST /Sessions/Playing,
// /Sessions/Playing/Progress and /Sessions/Playing/Stopped — one struct, as
// upstream builds it with a single options dict.
type SessionInfo struct {
	ItemID                   string         `json:"ItemId"`
	MediaSourceID            string         `json:"MediaSourceId"`
	PlaySessionID            string         `json:"PlaySessionId"`
	PlaylistItemID           string         `json:"PlaylistItemId,omitempty"`
	PlayMethod               string         `json:"PlayMethod"` // "DirectPlay" | "Transcode"
	LiveStreamID             string         `json:"LiveStreamId,omitempty"`
	CanSeek                  bool           `json:"CanSeek"`
	IsPaused                 bool           `json:"IsPaused"`
	IsMuted                  bool           `json:"IsMuted"`
	VolumeLevel              int            `json:"VolumeLevel"`
	PositionTicks            int64          `json:"PositionTicks"`
	PlaybackStartTimeTicks   int64          `json:"PlaybackStartTimeTicks"`
	SubtitleStreamIndex      int            `json:"SubtitleStreamIndex"`
	AudioStreamIndex         int            `json:"AudioStreamIndex"`
	RepeatMode               string         `json:"RepeatMode"`
	BufferedRanges           []BufferedRange `json:"BufferedRanges,omitempty"`
	NowPlayingQueue          []PlaylistItem `json:"NowPlayingQueue,omitempty"`
}

package jfin

import "errors"

// ProfileOpts are the knobs the device profile varies on. Bitrate is in
// kbps, matching upstream settings local_kbps/remote_kbps.
type ProfileOpts struct {
	IsRemote       bool
	VideoBitrate   *int // kbps; nil → LocalKbps/RemoteKbps
	LocalKbps      int
	RemoteKbps     int
	ForceTranscode bool
	TranscodeH265  bool // allow h265/hevc as transcode targets
	ForceH264      bool // force h264 output
}

// Profile is the DeviceProfile sent with PlaybackInfo requests. Port of
// upstream utils.get_profile (the static JSON, with our two codec knobs).
type Profile struct {
	Name                             string          `json:"Name"`
	MaxStreamingBitrate              int             `json:"MaxStreamingBitrate"`
	MaxStaticBitrate                 int             `json:"MaxStaticBitrate"`
	MusicStreamingTranscodingBitrate int             `json:"MusicStreamingTranscodingBitrate"`
	TimelineOffsetSeconds            int             `json:"TimelineOffsetSeconds"`
	TranscodingProfiles              []Transcoding   `json:"TranscodingProfiles"`
	DirectPlayProfiles               []Transcoding   `json:"DirectPlayProfiles"`
	ResponseProfiles                 []any           `json:"ResponseProfiles"`
	ContainerProfiles                []any           `json:"ContainerProfiles"`
	CodecProfiles                    []any           `json:"CodecProfiles"`
	SubtitleProfiles                 []SubtitleProfile `json:"SubtitleProfiles"`
}

type Transcoding struct {
	Container        string `json:"Container,omitempty"`
	Type             string `json:"Type"`
	Protocol         string `json:"Protocol,omitempty"`
	AudioCodec       string `json:"AudioCodec,omitempty"`
	VideoCodec       string `json:"VideoCodec,omitempty"`
	MaxAudioChannels string `json:"MaxAudioChannels,omitempty"`
}

type SubtitleProfile struct {
	Format string `json:"Format"`
	Method string `json:"Method"`
}

// DeviceProfile builds the profile JSON (port of utils.get_profile; the
// transcode-hi10p/DV/HDR/HEVC/AV1 CodecProfiles are all off by default
// upstream and have no setting here, so the list stays empty).
func DeviceProfile(o ProfileOpts) (*Profile, error) {
	if o.LocalKbps <= 0 || o.RemoteKbps <= 0 {
		return nil, errors.New("jfin: kbps must be > 0")
	}
	kbps := o.LocalKbps
	if o.IsRemote {
		kbps = o.RemoteKbps
	}
	if o.VideoBitrate != nil {
		kbps = *o.VideoBitrate
	}
	// Upstream picks the transcode video codec list from its h265 trio of
	// settings; with our two knobs it collapses to: h265 allowed iff
	// transcode_h265 is set (and force_h264 is not).
	var videoCodecs string
	if o.TranscodeH265 && !o.ForceH264 {
		videoCodecs = "h264,h265,hevc,mpeg4,mpeg2video"
	} else {
		videoCodecs = "h264,mpeg4,mpeg2video"
	}
	p := &Profile{
		Name:                             ClientName,
		MaxStreamingBitrate:              kbps * 1000,
		MaxStaticBitrate:                 kbps * 1000,
		MusicStreamingTranscodingBitrate: 1280000,
		TimelineOffsetSeconds:            5,
		TranscodingProfiles: []Transcoding{
			{Type: "Audio"},
			{Container: "ts", Type: "Video", Protocol: "hls",
				AudioCodec: "aac,mp3,ac3,opus,flac,vorbis",
				VideoCodec: videoCodecs, MaxAudioChannels: "6"},
			{Container: "jpeg", Type: "Photo"},
		},
		DirectPlayProfiles: []Transcoding{
			{Type: "Video"},
			{Type: "Audio"},
			{Type: "Photo"},
		},
		ResponseProfiles:    []any{},
		ContainerProfiles:   []any{},
		CodecProfiles:       []any{},
		SubtitleProfiles: []SubtitleProfile{
			{"srt", "External"}, {"srt", "Embed"},
			{"ass", "External"}, {"ass", "Embed"},
			{"sub", "Embed"}, {"sub", "External"},
			{"ssa", "Embed"}, {"ssa", "External"},
			{"smi", "Embed"}, {"smi", "External"},
			// Jellyfin refuses to serve these as external.
			{"pgssub", "Embed"},
			{"dvdsub", "Embed"},
			{"dvbsub", "Embed"},
			{"pgs", "Embed"},
		},
	}
	return p, nil
}

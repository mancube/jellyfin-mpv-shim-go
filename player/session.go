package player

import "mpv-shim/jfin"

// Session reports to Jellyfin. Port of upstream get_timeline_options and
// the send_timeline* methods.

// timelineOptions builds the Playing/Progress/Stopped payload. Caller holds
// p.mu and p.media must be set.
func (p *Player) timelineOptions(finished bool) *jfin.SessionInfo {
	v := p.media.Video
	m := p.media

	volume := 100.0
	if x, err := p.mpv.GetProperty("volume"); err == nil {
		if f, ok := x.(float64); ok {
			volume = f
		}
	}
	mute := false
	if x, err := p.mpv.GetProperty("mute"); err == nil {
		mute, _ = x.(bool)
	}
	pause := false
	if x, err := p.mpv.GetProperty("pause"); err == nil {
		pause, _ = x.(bool)
	}
	var duration *float64
	if x, err := p.mpv.GetProperty("duration"); err == nil {
		if f, ok := x.(float64); ok {
			duration = &f
		}
	}
	var playbackTime *float64
	if x, err := p.mpv.GetProperty("time-pos"); err == nil {
		if f, ok := x.(float64); ok {
			playbackTime = &f
		}
	}
	cacheBuffering := 0.0
	if x, err := p.mpv.GetProperty("cache-buffering-state"); err == nil {
		if f, ok := x.(float64); ok {
			cacheBuffering = f
		}
	}
	if playbackTime != nil {
		p.lastPos = *playbackTime
	}
	p.lastPause = pause
	p.lastMute = mute

	var safePos float64
	if finished {
		// When mpv has already exited we don't actually know if the video
		// finished — fall back to the last known position.
		if playbackTime == nil {
			safePos = p.lastPos
		} else {
			safePos = v.GetDuration()
		}
	} else {
		if playbackTime != nil {
			safePos = *playbackTime
		}
	}
	p.lastSeek = safePos
	p.pauseIgnore = pause

	info := &jfin.SessionInfo{
		ItemID:                 v.ID,
		MediaSourceID:          v.MediaSource.ID,
		PlaySessionID:          v.PlaybackInfo.PlaySessionId,
		PlaylistItemID:         m.Queue[m.Seq].PlaylistItemId,
		PlayMethod:             "DirectPlay",
		CanSeek:                true,
		IsPaused:               pause,
		IsMuted:                mute,
		VolumeLevel:            int(volume),
		PositionTicks:          int64(safePos * 1e7),
		PlaybackStartTimeTicks: p.start.Unix() * 1e7,
		SubtitleStreamIndex:    -1,
		AudioStreamIndex:       -1,
		RepeatMode:             "RepeatNone",
		NowPlayingQueue:        m.Queue,
	}
	if v.IsTranscode {
		info.PlayMethod = "Transcode"
	}
	if v.MediaSource.LiveStreamId != "" {
		info.LiveStreamID = v.MediaSource.LiveStreamId
	}
	if v.Aid != nil {
		info.AudioStreamIndex = *v.Aid
	}
	if v.Sid != nil {
		info.SubtitleStreamIndex = *v.Sid
	}
	if duration != nil {
		info.BufferedRanges = []jfin.BufferedRange{{
			Start: int64(safePos * 1e7),
			End:   int64((*duration - safePos*cacheBuffering/100 + safePos) * 1e7),
		}}
	}
	return info
}

func (p *Player) sendStart() {
	if p.media == nil || p.ctx == nil {
		return
	}
	opts := p.timelineOptions(false)
	if err := p.media.C.SessionPlaying(p.ctx, opts); err != nil {
		p.log.Printf("session playing: %v", err)
	}
}

func (p *Player) sendStopped(finished bool) {
	if p.media == nil {
		return
	}
	p.shouldSendTimeline = false
	opts := p.timelineOptions(finished)
	c := p.media.C
	if err := c.SessionStopped(p.ctx, opts); err != nil {
		p.log.Printf("session stopped: %v", err)
	}
}

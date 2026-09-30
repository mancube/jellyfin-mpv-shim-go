package player

// Property observers: the immediate feedback path. Upstream registers
// property_observer("pause"/"seeking"/…) so the web UI hears about a change the
// moment it happens; we subscribe with `observe_property` and do the same from
// the event loop. Without this the server only learns about local changes on
// the next 5 s timeline tick, which makes the remote panel feel laggy.

import (
	"encoding/json"
	"time"
)

// positionReportInterval throttles the position-only reports driven by
// time-pos (which fires several times a second). State changes (pause, mute,
// volume, seek finished) always report immediately.
const positionReportInterval = 1500 * time.Millisecond

// trackChangeLocked updates the video's aid/sid from an mpv-side track switch
// and reports it. False = nothing to report (unchanged, or a track we cannot
// map, e.g. an external subtitle added by the user).
func (p *Player) trackChangeLocked(prop string, data json.RawMessage) bool {
	v := p.media.Video
	// mpv track ids are the mapped sequence values; "no"/-1 means off.
	var id any
	if err := json.Unmarshal(data, &id); err != nil {
		return false
	}
	if s, ok := id.(string); ok {
		if s == "no" || s == "auto" {
			id = float64(-1)
		} else {
			return false // "auto": let mpv pick, nothing to sync
		}
	}
	f, ok := id.(float64)
	if !ok {
		return false
	}
	seq := map[int]int{} // mpv id → Jellyfin index
	if prop == "aid" {
		for jellyfinIdx, mpvID := range v.AudioSeq {
			seq[mpvID] = jellyfinIdx
		}
	} else {
		for jellyfinIdx, mpvID := range v.SubtitleSeq {
			seq[mpvID] = jellyfinIdx
		}
		if f == -1 { // subtitles off
			v.Sid = &offIndex
			return true
		}
		if u, ok := v.SubtitleURL[seq[int(f)]]; ok {
			// An external subtitle: mpv has it loaded, the web UI wants the
			// Jellyfin index. We cannot know which one it is, so log and skip.
			p.log.Printf("external subtitle switched in mpv (%s); sync it from the web UI", u)
			return false
		}
	}
	idx, ok := seq[int(f)]
	if !ok {
		return false
	}
	if prop == "aid" {
		if v.Aid != nil && *v.Aid == idx {
			return false
		}
		v.Aid = &idx
	} else {
		if v.Sid != nil && *v.Sid == idx {
			return false
		}
		v.Sid = &idx
	}
	p.log.Printf("track switched in mpv: %s=%d", prop, idx)
	return true
}

// offIndex is the Jellyfin "no subtitles" index.
var offIndex = -1

// onPropertyChange handles one property-change event.
func (p *Player) onPropertyChange(prop string, data json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	// mpv sends the property's current value as soon as we subscribe. That
	// echo is not a user action, so drop the first one per property (per mpv
	// incarnation) — otherwise an auto-selected track would be adopted as the
	// user's choice and pushed to the web UI.
	if p.initialEcho[prop] {
		delete(p.initialEcho, prop)
		return
	}
	now := time.Now()
	// time-pos fires several times a second: skip the extra IPC round-trips
	// (aborted()) for those and let the cheap paths run.
	if prop == "time-pos" {
		if p.media == nil || !p.shouldSendTimeline || p.lastPause {
			return
		}
	} else if p.media == nil || !p.shouldSendTimeline || p.aborted() {
		return
	}

	// A state change we caused ourselves was already reported by the op that
	// set it (SetPaused/SetMute/SetVolume/Seek) — echo suppression, port of
	// upstream's pause_ignore / last_seek.
	switch prop {
	case "pause":
		var v bool
		if json.Unmarshal(data, &v) != nil {
			return
		}
		if v == p.lastPause {
			return
		}
		p.lastPause, p.pauseIgnore = v, v
	case "mute":
		var v bool
		if json.Unmarshal(data, &v) != nil {
			return
		}
		if v == p.lastMute {
			return
		}
		p.lastMute = v
		recordVolume(int(p.repVolume), v)
	case "volume":
		var v float64
		if json.Unmarshal(data, &v) != nil {
			return
		}
		if int(v) == int(p.repVolume) {
			return
		}
		recordVolume(int(v), p.lastMute)
	case "seeking":
		// Report when the seek finished (value false) — that is the new
		// position the UI should show, not the drag in progress.
		var v bool
		if json.Unmarshal(data, &v) != nil {
			return
		}
		if v {
			return
		}
	case "aid", "sid":
		// A track switch done inside mpv (OSC, the `a`/`s` keys, a client
		// like MPRIS) must reach the web UI too: map the mpv track id back to
		// the Jellyfin stream index and report it.
		if !p.trackChangeLocked(prop, data) {
			return
		}
	case "time-pos":
		var v float64
		if json.Unmarshal(data, &v) != nil {
			return
		}
		p.lastPos = v
		if now.Sub(p.lastReport) < positionReportInterval {
			return
		}
	default:
		return
	}
	p.lastReport = now
	p.reportLocked()
}

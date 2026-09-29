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

// onPropertyChange handles one property-change event.
func (p *Player) onPropertyChange(prop string, data json.RawMessage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.media == nil || !p.shouldSendTimeline || p.aborted() {
		return
	}
	now := time.Now()

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
	case "volume":
		var v float64
		if json.Unmarshal(data, &v) != nil {
			return
		}
		if int(v) == int(p.repVolume) {
			return
		}
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

package player

// Volume memory: restore the volume *and* the mute state from the previous
// session when playback starts, and hand new values back to the app so it can
// persist them. The player knows nothing about config files — main installs the
// callbacks.
//
// Disk writes are deliberately rare: `Record` only updates the in-memory value
// and `Commit` (at the end of playback, when mpv goes away, and at app exit)
// is what actually writes.

import "sync"

// VolumeState is the remembered output state.
type VolumeState struct {
	Volume int // 0 = nothing remembered
	Mute   bool
}

// VolumeMemory is the app-side hook for the volume memory.
type VolumeMemory struct {
	// Get returns the state to restore (Volume 0 = none).
	Get func() VolumeState
	// Record is called on every change; it must not touch the disk.
	Record func(VolumeState)
	// Commit persists the recorded state. Called on the logical events.
	Commit func()
}

var (
	volMemMu sync.Mutex
	volMem   VolumeMemory
)

// SetVolumeMemory installs the callbacks.
func (p *Player) SetVolumeMemory(m VolumeMemory) {
	volMemMu.Lock()
	volMem = m
	volMemMu.Unlock()
}

// rememberedState returns the state to restore at the start of playback.
func rememberedState() VolumeState {
	volMemMu.Lock()
	get := volMem.Get
	volMemMu.Unlock()
	if get == nil {
		return VolumeState{}
	}
	return get()
}

// recordVolume notes a new volume/mute without writing anything: the app
// keeps it in memory until it decides to persist.
func recordVolume(vol int, mute bool) {
	volMemMu.Lock()
	record := volMem.Record
	volMemMu.Unlock()
	if record != nil {
		record(VolumeState{Volume: vol, Mute: mute})
	}
}

// commitVolume persists the remembered state — only called on the logical
// events: playback ended, mpv quitting, app exiting.
func commitVolume() {
	volMemMu.Lock()
	commit := volMem.Commit
	volMemMu.Unlock()
	if commit != nil {
		commit()
	}
}

// restoreVolume applies the remembered volume and mute state to a freshly
// loaded file. Called with p.mu held.
func (p *Player) restoreVolumeLocked() {
	if !p.opt.RememberVolume {
		return
	}
	st := rememberedState()
	if st.Volume > 0 {
		p.mpv.SetProperty("volume", st.Volume)
	}
	if st.Mute {
		p.mpv.SetProperty("mute", true)
	}
}

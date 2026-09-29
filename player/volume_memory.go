package player

// Volume memory: restore the volume from the previous session when playback
// starts, and hand every new volume back to the app so it can be persisted.
// The player does not know about config files — main installs two callbacks.

import "sync"

// volumeMemory bridges the player and the settings store.
type volumeMemory struct {
	mu  sync.Mutex
	get func() int       // remembered volume, 0 = none
	set func(volume int) // called when the volume changes
}

var volMem volumeMemory

// SetVolumeMemory installs the callbacks: get returns the remembered volume
// (0 when there is none or the feature is off), set is called whenever the
// volume changes.
func (p *Player) SetVolumeMemory(get func() int, set func(int)) {
	volMem.mu.Lock()
	volMem.get, volMem.set = get, set
	volMem.mu.Unlock()
}

// rememberedVolume returns the volume to restore, or 0.
func rememberedVolume() int {
	volMem.mu.Lock()
	get := volMem.get
	volMem.mu.Unlock()
	if get == nil {
		return 0
	}
	return get()
}

// rememberVolume reports a new volume to the app (best effort, never blocks
// playback).
func rememberVolume(vol int) {
	volMem.mu.Lock()
	set := volMem.set
	volMem.mu.Unlock()
	if set != nil {
		set(vol)
	}
}

// restoreVolume applies the remembered volume to a freshly loaded file. Called
// with p.mu held.
func (p *Player) restoreVolumeLocked() {
	if !p.opt.RememberVolume {
		return
	}
	if vol := rememberedVolume(); vol > 0 {
		p.mpv.SetProperty("volume", vol)
	}
}

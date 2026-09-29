package ui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"

	"mpv-shim/jfin"
)

func decodeIcon(t *testing.T, state int32) image.Image {
	t.Helper()
	b := trayIcon(state)
	if len(b) == 0 {
		t.Fatalf("trayIcon(%d) returned nothing", state)
	}
	img, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("decode icon: %v", err)
	}
	return img
}

func at(img image.Image, x, y int) (r, g, b, a uint32) {
	rr, gg, bb, aa := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
	return rr >> 8, gg >> 8, bb >> 8, aa >> 8
}

// The icon must be high resolution: KDE panels are 22–32 px (44 on HiDPI) and
// a 16 px source is what looked like an 8-bit blur.
func TestTrayIconIsHighResolution(t *testing.T) {
	for _, state := range []int32{jfin.StateOffline, jfin.StateReconnecting, jfin.StateConnected} {
		img := decodeIcon(t, state)
		w := img.Bounds().Dx()
		if w < 64 {
			t.Errorf("icon width = %d, want >= 64 (panel-size independent)", w)
		}
		if w != img.Bounds().Dy() {
			t.Errorf("icon is not square: %v", img.Bounds())
		}
	}
}

// The artwork is the project's own icon, untouched; only the status dot is
// drawn on top, and it is big enough to see after the panel downscales it.
func TestTrayIconArtworkPlusStatusDot(t *testing.T) {
	base := baseIcon()
	if base == nil {
		t.Fatal("no embedded artwork")
	}
	online := decodeIcon(t, jfin.StateConnected)
	offline := decodeIcon(t, jfin.StateOffline)
	reconnecting := decodeIcon(t, jfin.StateReconnecting)

	// The dot is a circle of ~1/9 of the width, sitting in the corner; sample
	// its centre and require the surrounding artwork to be untouched.
	b := base.Bounds()
	size := b.Dx()
	dotR := size / 9
	cx, cy := b.Max.X-dotR-2, b.Max.Y-dotR-2

	// Inside the dot: opaque and one of our colours.
	r, g, bl, a := at(online, cx, cy)
	if a < 250 {
		t.Errorf("dot centre is not opaque: alpha=%d", a)
	}
	if g <= r || g <= bl {
		t.Errorf("connected dot is not green at centre: rgb(%d,%d,%d)", r, g, bl)
	}

	// Far from the dot: identical to the base artwork.
	for _, p := range [][2]int{{0, 0}, {size / 2, 0}, {0, size / 2}, {size / 2, size / 2}, {size / 3, size / 3}} {
		br, bg, bb, ba := at(base, p[0], p[1])
		ir, ig, ib, ia := at(online, p[0], p[1])
		if br != ir || bg != ig || bb != ib || ba != ia {
			t.Errorf("artwork pixel %v was recoloured: base rgba(%d,%d,%d,%d) icon rgba(%d,%d,%d,%d)",
				p, br, bg, bb, ba, ir, ig, ib, ia)
		}
	}

	// Distinct colours per state.
	col := func(img image.Image) [3]uint32 {
		r, g, bb, _ := at(img, cx, cy)
		return [3]uint32{r, g, bb}
	}
	cOn, cOff, cRecon := col(online), col(offline), col(reconnecting)
	if cOn == cOff || cOn == cRecon || cOff == cRecon {
		t.Errorf("dot colours are not distinct: on=%v off=%v reconnecting=%v", cOn, cOff, cRecon)
	}
	if r, g, b := cRecon[0], cRecon[1], cRecon[2]; !(r > g && g > b) { // amber
		t.Errorf("reconnecting dot is not amber: %v", cRecon)
	}
	if cOff[0] != cOff[1] || cOff[1] != cOff[2] { // grey
		t.Errorf("offline dot is not grey: %v", cOff)
	}
}

func TestPNGToICO(t *testing.T) {
	ico := pngToICO(trayIcon(jfin.StateConnected))
	if len(ico) < 22 {
		t.Fatalf("ICO too short: %d bytes", len(ico))
	}
	if r := binary.LittleEndian.Uint16(ico[0:2]); r != 0 {
		t.Errorf("ICONDIR reserved = %d, want 0", r)
	}
	if typ := binary.LittleEndian.Uint16(ico[2:4]); typ != 1 {
		t.Errorf("ICONDIR type = %d, want 1 (icon)", typ)
	}
	if n := binary.LittleEndian.Uint16(ico[4:6]); n != 1 {
		t.Errorf("ICONDIR image count = %d, want 1", n)
	}
	if size := binary.LittleEndian.Uint32(ico[14:18]); int(size) != len(ico)-22 {
		t.Errorf("entry size = %d, want %d", size, len(ico)-22)
	}
	if _, err := png.Decode(bytes.NewReader(ico[22:])); err != nil {
		t.Errorf("ICO payload is not a PNG: %v", err)
	}
}

func TestConnLabel(t *testing.T) {
	cases := map[int32]string{
		jfin.StateConnected:    "online",
		jfin.StateReconnecting: "reconnecting",
		jfin.StateOffline:      "offline",
	}
	for state, want := range cases {
		if got := connLabel(state); got != want {
			t.Errorf("connLabel(%d) = %q, want %q", state, got, want)
		}
	}
}

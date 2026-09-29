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
	if got := img.Bounds().Dx(); got != 16 {
		t.Errorf("icon width = %d, want 16 (upstream's systray.png)", got)
	}
	return img
}

func at(img image.Image, x, y int) (r, g, b, a uint32) {
	rr, gg, bb, aa := img.At(img.Bounds().Min.X+x, img.Bounds().Min.Y+y).RGBA()
	return rr >> 8, gg >> 8, bb >> 8, aa >> 8
}

// The icon is upstream's artwork untouched; only the status dot in the bottom
// -right corner changes, and it has one colour per connection state.
func TestTrayIconIsUpstreamArtworkPlusStatusDot(t *testing.T) {
	upstream, err := png.Decode(bytes.NewReader(upstreamIcon))
	if err != nil {
		t.Fatal(err)
	}
	online := decodeIcon(t, jfin.StateConnected)
	offline := decodeIcon(t, jfin.StateOffline)
	reconnecting := decodeIcon(t, jfin.StateReconnecting)

	// Everything outside the dot is byte-identical to the original artwork…
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if x >= 13 && y >= 13 {
				continue // the dot
			}
			ur, ug, ub, ua := at(upstream, x, y)
			ir, ig, ib, ia := at(online, x, y)
			if ur != ir || ug != ig || ub != ib || ua != ia {
				t.Fatalf("pixel (%d,%d) was recoloured: upstream rgba(%d,%d,%d,%d), icon rgba(%d,%d,%d,%d)",
					x, y, ur, ug, ub, ua, ir, ig, ib, ia)
			}
		}
	}

	// …and the dot differs per state: green / amber / grey.
	col := func(img image.Image) [3]uint32 {
		r, g, b, _ := at(img, 15, 15)
		return [3]uint32{r, g, b}
	}
	cOn, cOff, cRecon := col(online), col(offline), col(reconnecting)
	if cOn == cOff || cOn == cRecon || cOff == cRecon {
		t.Errorf("dot colours are not distinct: on=%v off=%v reconnecting=%v", cOn, cOff, cRecon)
	}
	if g := cOn[1]; g <= cOn[0] || g <= cOn[2] {
		t.Errorf("connected dot is not green: %v", cOn)
	}
	if r, g, b := cRecon[0], cRecon[1], cRecon[2]; !(r > g && g > b) { // amber: red > green > blue
		t.Errorf("reconnecting dot is not amber: %v", cRecon)
	}
	if cOff[0] != cOff[1] || cOff[1] != cOff[2] {
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

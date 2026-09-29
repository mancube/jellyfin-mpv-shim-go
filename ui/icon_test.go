package ui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"testing"
)

func decodeIcon(t *testing.T, online bool) image.Image {
	t.Helper()
	b := trayIcon(online)
	if len(b) == 0 {
		t.Fatalf("trayIcon(%v) returned nothing", online)
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

// The icon is upstream's glyph in our colours, and the status dot is the only
// thing that changes between states.
func TestTrayIconTintAndStatusDot(t *testing.T) {
	on := decodeIcon(t, true)
	off := decodeIcon(t, false)

	// Two-tone tint: the glyph must not be a single flat colour.
	seen := map[[3]uint32]bool{}
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			r, g, b, a := at(on, x, y)
			if a < 32 {
				continue
			}
			seen[[3]uint32{r, g, b}] = true
		}
	}
	if len(seen) < 2 {
		t.Errorf("icon is a single flat colour: %v", seen)
	}

	// The dot lives in the bottom-right corner and switches colour with state.
	r1, g1, b1, _ := at(on, 15, 15)
	r2, g2, b2, _ := at(off, 15, 15)
	if r1 == r2 && g1 == g2 && b1 == b2 {
		t.Errorf("status dot does not change with the connection state: %v", r1)
	}
	if g1 <= r1 || g1 <= b1 {
		t.Errorf("online dot is not green: rgb(%d,%d,%d)", r1, g1, b1)
	}
	if r2 != g2 || g2 != b2 {
		t.Errorf("offline dot is not grey: rgb(%d,%d,%d)", r2, g2, b2)
	}

	// Everything else must be identical between the two states.
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			if x >= 13 && y >= 13 {
				continue // the dot
			}
			ar, ag, ab, aa := at(on, x, y)
			br, bg, bb, ba := at(off, x, y)
			if ar != br || ag != bg || ab != bb || aa != ba {
				t.Errorf("pixel (%d,%d) differs outside the status dot", x, y)
			}
		}
	}
}

func TestPNGToICO(t *testing.T) {
	ico := pngToICO(trayIcon(true))
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
	size := binary.LittleEndian.Uint32(ico[14:18])
	if int(size) != len(ico)-22 {
		t.Errorf("entry size = %d, want %d", size, len(ico)-22)
	}
	if _, err := png.Decode(bytes.NewReader(ico[22:])); err != nil {
		t.Errorf("ICO payload is not a PNG: %v", err)
	}
}

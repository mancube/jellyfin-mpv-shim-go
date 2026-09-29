package ui

// The tray icon: upstream's jellyfin-mpv-shim `systray.png` (16×16) used
// as-is, plus a small status dot in the bottom-right corner so the icon says
// whether we are connected. Windows gets a generated .ico (one PNG-compressed
// entry, supported since Vista); Linux/macOS get the PNG.

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"runtime"

	"mpv-shim/jfin"
)

//go:embed assets/systray.png
var upstreamIcon []byte

// Status dot colours, one per connection state.
var (
	dotConnected    = color.NRGBA{R: 0x2e, G: 0xd4, B: 0x5a, A: 0xff} // green
	dotReconnecting = color.NRGBA{R: 0xf2, G: 0xc0, B: 0x3d, A: 0xff} // amber
	dotOffline      = color.NRGBA{R: 0x8a, G: 0x8a, B: 0x8a, A: 0xff} // grey
)

// trayIcon renders the tray icon for a connection state. Returns
// platform-appropriate bytes (PNG, or an ICO on Windows).
func trayIcon(state int32) []byte {
	src, err := png.Decode(bytes.NewReader(upstreamIcon))
	if err != nil {
		return nil
	}
	img := image.NewNRGBA(src.Bounds())
	draw.Draw(img, src.Bounds(), src, src.Bounds().Min, draw.Src)
	drawStatusDot(img, state)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return pngToICO(buf.Bytes())
	}
	return buf.Bytes()
}

// drawStatusDot puts a small dot in the bottom-right corner: green while the
// socket is up, amber while reconnecting, grey while offline.
func drawStatusDot(img *image.NRGBA, state int32) {
	c := dotOffline
	switch state {
	case jfin.StateConnected:
		c = dotConnected
	case jfin.StateReconnecting:
		c = dotReconnecting
	}
	b := img.Bounds()
	x0, y0 := b.Max.X-3, b.Max.Y-3 // 3×3 dot
	for y := y0; y < b.Max.Y; y++ {
		for x := x0; x < b.Max.X; x++ {
			// Skip one corner pixel so the dot reads as a circle.
			if x == x0 && y == b.Max.Y-1 {
				continue
			}
			img.SetNRGBA(x, y, c)
		}
	}
}

// pngToICO wraps a PNG in a single-entry ICO container (Vista+ supports
// PNG-compressed entries), so Windows shows the same icon + dot.
func pngToICO(pngData []byte) []byte {
	const (
		headerSize = 6
		entrySize  = 16
	)
	var buf bytes.Buffer
	// ICONDIR
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // one image
	// ICONDIRENTRY: a 16×16 icon is stored as 0 (256).
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint8(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))  // colour planes
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32)) // bits per pixel
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(pngData)))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(headerSize+entrySize))
	buf.Write(pngData)
	return buf.Bytes()
}

package ui

// The tray icon. Upstream's tray icon is `integration/jellyfin-16.png` (the
// same 16×16 art as systray.png), which is far too small for a KDE panel
// (22–32 px, 44 px on HiDPI) — upscaling it looks like an 8-bit blur. So we
// embed the project's high-resolution artwork from the same folder
// (jellyfin-256.png, jellyfin-128.png as a lighter alternative) and draw a
// status dot sized from the artwork, so the icon stays crisp at any panel size
// and the dot stays visible.

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"runtime"
	"sync"

	"mpv-shim/jfin"
)

//go:embed assets/jellyfin-256.png
var icon256 []byte

//go:embed assets/jellyfin-128.png
var icon128 []byte

// Status dot colours, one per connection state.
var (
	dotConnected    = color.NRGBA{R: 0x2e, G: 0xd4, B: 0x5a, A: 0xff} // green
	dotReconnecting = color.NRGBA{R: 0xf2, G: 0xc0, B: 0x3d, A: 0xff} // amber
	dotOffline      = color.NRGBA{R: 0x9a, G: 0x9a, B: 0x9a, A: 0xff} // grey
	dotRing         = color.NRGBA{R: 0x10, G: 0x10, B: 0x10, A: 0xd0} // dark outline
)

var (
	baseOnce sync.Once
	baseImg  image.Image
	baseSize int // 0 = the largest embedded artwork
)

// trayIcon renders the tray icon for a connection state: PNG on Linux/macOS,
// a generated single-entry ICO on Windows.
func trayIcon(state int32) []byte {
	return trayIconSize(state, 0)
}

// trayIconSize is trayIcon with a chosen source resolution (0 = largest).
func trayIconSize(state int32, size int) []byte {
	base := baseIcon(size)
	if base == nil {
		return nil
	}
	img := image.NewNRGBA(base.Bounds())
	draw.Draw(img, base.Bounds(), base, base.Bounds().Min, draw.Src)
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

// baseIcon decodes the embedded artwork once. trayIconSize picks the source
// resolution: 0 = the largest available.
func baseIcon(size int) image.Image {
	baseOnce.Do(func() {
		for _, b := range [][]byte{icon256, icon128} {
			if img, err := png.Decode(bytes.NewReader(b)); err == nil {
				baseImg, baseSize = img, img.Bounds().Dx()
				break
			}
		}
	})
	if baseImg == nil {
		return nil
	}
	if size == 0 || size >= baseSize {
		return baseImg
	}
	// Downscale once; cached per requested size.
	key := fmt.Sprintf("icon:%d", size)
	scalesOnce.Do(func() { scales = map[string]image.Image{} })
	if img, ok := scales[key]; ok {
		return img
	}
	small := image.NewNRGBA(image.Rect(0, 0, size, size))
	// Box-average downscale: for icon work it beats bilinear on hard edges and
	// needs no extra dependency (golang.org/x/image is not in the graph).
	averageScale(small, baseImg)
	scales[key] = small
	return small
}

var (
	scalesOnce sync.Once
	scales     map[string]image.Image
)

// averageScale is a plain box filter over the source pixels.
func averageScale(dst *image.NRGBA, src image.Image) {
	sw, sh := src.Bounds().Dx(), src.Bounds().Dy()
	dw, dh := dst.Bounds().Dx(), dst.Bounds().Dy()
	for y := 0; y < dh; y++ {
		y0, y1 := y*sh/dh, (y+1)*sh/dh
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < dw; x++ {
			x0, x1 := x*sw/dw, (x+1)*sw/dw
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var r, g, b, a, n uint64
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					// premultiplied average, then un-premultiply
					r += uint64(cr) * uint64(ca) / 0xffff
					g += uint64(cg) * uint64(ca) / 0xffff
					b += uint64(cb) * uint64(ca) / 0xffff
					a += uint64(ca)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			avgA := a / n
			if avgA == 0 {
				dst.SetNRGBA(x, y, color.NRGBA{})
				continue
			}
			dst.SetNRGBA(x, y, color.NRGBA{
				R: uint8((r / n) * 0xffff / avgA),
				G: uint8((g / n) * 0xffff / avgA),
				B: uint8((b / n) * 0xffff / avgA),
				A: uint8(avgA >> 8),
			})
		}
	}
}

// drawStatusDot puts a filled circle with a dark outline in the bottom-right
// corner: green connected, amber reconnecting, grey offline. It is sized from
// the artwork (≈1/5 of the width) so that after a panel downscales the icon to
// 22–44 px the dot is still ~5–9 px across and clearly readable.
func drawStatusDot(img *image.NRGBA, state int32) {
	c := dotOffline
	switch state {
	case jfin.StateConnected:
		c = dotConnected
	case jfin.StateReconnecting:
		c = dotReconnecting
	}
	b := img.Bounds()
	size := b.Dx()
	// Radius, clamped so tiny icons still get a visible dot.
	r := float64(size) / 9.0
	if r < 1.5 {
		r = 1.5
	}
	if r > float64(size)/3 {
		r = float64(size) / 3
	}
	ring := math.Max(1, r/6) // outline thickness
	cx := float64(b.Max.X) - r - ring
	cy := float64(b.Max.Y) - r - ring

	minX, minY := int(cx-r-ring-1), int(cy-r-ring-1)
	for y := minY; y < b.Max.Y; y++ {
		for x := minX; x < b.Max.X; x++ {
			if x < b.Min.X || y < b.Min.Y {
				continue
			}
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d := math.Hypot(dx, dy)
			switch {
			case d <= r:
				img.SetNRGBA(x, y, c)
			case d <= r+ring:
				img.SetNRGBA(x, y, dotRing)
			}
		}
	}
}

// pngToICO wraps a PNG in a single-entry ICO container (Vista+ supports
// PNG-compressed entries), so Windows shows the same image.
func pngToICO(pngData []byte) []byte {
	const (
		headerSize = 6
		entrySize  = 16
	)
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1)) // one image
	// ICONDIRENTRY: width/height 0 = 256.
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

package ui

// The tray icon: upstream's jellyfin-mpv-shim `systray.png` (16×16), tinted
// into the shim's own colours and given a live status dot, so the icon says
// "connected" at a glance. Windows gets a generated .ico (one PNG-compressed
// entry, which Windows has supported since Vista); Linux/macOS get the PNG.

import (
	"bytes"
	_ "embed"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"runtime"
)

//go:embed assets/systray.png
var upstreamIcon []byte

// The shim's two-tone palette: the ring in Jellyfin's cyan, the mark inside
// lighter so the 16 px glyph keeps its detail.
var (
	iconRing  = color.NRGBA{R: 0x00, G: 0xa4, B: 0xdc, A: 0xff}
	iconMark  = color.NRGBA{R: 0xc8, G: 0xec, B: 0xff, A: 0xff}
	dotOnline = color.NRGBA{R: 0x2e, G: 0xd4, B: 0x5a, A: 0xff}
	dotAway   = color.NRGBA{R: 0x8a, G: 0x8a, B: 0x8a, A: 0xff}
)

// trayIcon renders the tray icon for the current connection state. Returns
// platform-appropriate bytes (PNG, or an ICO on Windows).
func trayIcon(online bool) []byte {
	src, err := png.Decode(bytes.NewReader(upstreamIcon))
	if err != nil {
		return nil
	}
	img := tintIcon(src)
	drawStatusDot(img, online)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	if runtime.GOOS == "windows" {
		return pngToICO(buf.Bytes())
	}
	return buf.Bytes()
}

// tintIcon maps the upstream glyph's luminance onto our two colours: the
// brighter half (the ring's top/outer pixels) becomes the mark colour, the
// rest the ring colour — this keeps the shape while making it ours.
func tintIcon(src image.Image) *image.NRGBA {
	b := src.Bounds()
	out := image.NewNRGBA(b)
	draw.Draw(out, b, src, b.Min, draw.Src)
	// Split the glyph's own luminance range rather than a fixed threshold: the
	// upstream icon is a mid-tone purple, so an absolute cut would flatten it
	// to a single colour.
	lo, hi := 255, 0
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if p := out.NRGBAAt(x, y); p.A > 32 {
				lum := luminance(p)
				if lum < lo {
					lo = lum
				}
				if lum > hi {
					hi = lum
				}
			}
		}
	}
	cut := lo + 3*(hi-lo)/4
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			p := out.NRGBAAt(x, y)
			if p.A == 0 {
				continue
			}
			c := iconRing
			if luminance(p) > cut {
				c = iconMark
			}
			// Keep a hint of the original shading so the glyph stays legible.
			p.R = mix(p.R, c.R)
			p.G = mix(p.G, c.G)
			p.B = mix(p.B, c.B)
			out.SetNRGBA(x, y, p)
		}
	}
	return out
}

// luminance is the usual 0-255 perceived brightness.
func luminance(c color.NRGBA) int {
	return (299*int(c.R) + 587*int(c.G) + 114*int(c.B)) / 1000
}

func mix(orig, tint uint8) uint8 {
	// 75% tint, 25% original.
	return uint8((int(orig)*25 + int(tint)*75) / 100)
}

// drawStatusDot puts a small dot in the bottom-right corner: green while the
// socket is up, grey when it is not (reconnecting, server down).
func drawStatusDot(img *image.NRGBA, online bool) {
	c := dotAway
	if online {
		c = dotOnline
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
// PNG-compressed entries), so Windows shows the same tinted icon + dot.
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

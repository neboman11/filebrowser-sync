package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"math"
	"runtime"
)

type iconKind int

const (
	iconIdle iconKind = iota
	iconSyncing
	iconPaused
	iconError
)

const (
	iconSize   = 64
	spinFrames = 8
)

var iconColors = map[iconKind]color.NRGBA{
	iconIdle:    {0x2e, 0x9e, 0x5b, 0xff}, // green
	iconSyncing: {0x2f, 0x7f, 0xd8, 0xff}, // blue
	iconPaused:  {0x80, 0x80, 0x80, 0xff}, // grey
	iconError:   {0xd0, 0x45, 0x3d, 0xff}, // red
}

// renderIcon draws a coloured disc with a white glyph: rotating sync arrows
// (idle/syncing; rot in radians), pause bars, or an exclamation mark.
func renderIcon(kind iconKind, rot float64) []byte {
	const ss = 4 // supersampling per axis
	img := image.NewNRGBA(image.Rect(0, 0, iconSize, iconSize))
	bg := iconColors[kind]
	c := iconSize / 2.0
	for py := range iconSize {
		for px := range iconSize {
			var disc, glyph int
			for sy := range ss {
				for sx := range ss {
					x := float64(px) + (float64(sx)+0.5)/ss - c
					y := float64(py) + (float64(sy)+0.5)/ss - c
					if math.Hypot(x, y) > 30 {
						continue
					}
					disc++
					if inGlyph(kind, x, y, rot) {
						glyph++
					}
				}
			}
			if disc == 0 {
				continue
			}
			t := float64(glyph) / float64(disc)
			mix := func(a uint8) uint8 { return uint8(float64(a)*(1-t) + 255*t + 0.5) }
			img.SetNRGBA(px, py, color.NRGBA{mix(bg.R), mix(bg.G), mix(bg.B), uint8(255 * disc / (ss * ss))})
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	if runtime.GOOS == "windows" {
		return wrapICO(buf.Bytes())
	}
	return buf.Bytes()
}

func inGlyph(kind iconKind, x, y, rot float64) bool {
	switch kind {
	case iconPaused:
		return math.Abs(y) < 13 && (math.Abs(x+7) < 4 || math.Abs(x-7) < 4)
	case iconError:
		return (math.Abs(x) < 4 && y > -16 && y < 5) || math.Hypot(x, y-12) < 4.2
	}
	// Two arcs with arrowheads, rotated by rot.
	s, co := math.Sin(-rot), math.Cos(-rot)
	x, y = x*co-y*s, x*s+y*co
	r, a := math.Hypot(x, y), math.Atan2(y, x)
	for _, start := range []float64{0, math.Pi} {
		// arc spans [start+0.35, start+2.35] radians
		d := math.Mod(a-start+2*math.Pi, 2*math.Pi)
		if r > 13 && r < 19 && d > 0.35 && d < 2.35 {
			return true
		}
		// arrowhead at the arc's leading end, pointing along the circle
		ea := start + 2.35
		tip := [2]float64{22 * math.Cos(ea+0.55) * 0.75, 22 * math.Sin(ea+0.55) * 0.75}
		in := [2]float64{10 * math.Cos(ea), 10 * math.Sin(ea)}
		out := [2]float64{23 * math.Cos(ea), 23 * math.Sin(ea)}
		if inTriangle(x, y, tip, in, out) {
			return true
		}
	}
	return false
}

func inTriangle(x, y float64, a, b, c [2]float64) bool {
	sign := func(p1, p2 [2]float64) float64 {
		return (x-p2[0])*(p1[1]-p2[1]) - (p1[0]-p2[0])*(y-p2[1])
	}
	d1, d2, d3 := sign(a, b), sign(b, c), sign(c, a)
	neg := d1 < 0 || d2 < 0 || d3 < 0
	pos := d1 > 0 || d2 > 0 || d3 > 0
	return !(neg && pos)
}

// wrapICO packs a PNG into a single-image .ico (supported since Windows Vista).
func wrapICO(pngData []byte) []byte {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, [3]uint16{0, 1, 1})
	b.Write([]byte{iconSize, iconSize, 0, 0})
	binary.Write(&b, binary.LittleEndian, [2]uint16{1, 32})
	binary.Write(&b, binary.LittleEndian, [2]uint32{uint32(len(pngData)), 22})
	b.Write(pngData)
	return b.Bytes()
}

// icons holds pre-rendered images: one per static state plus spin frames.
type icons struct {
	idle, paused, err []byte
	spin              [spinFrames][]byte
}

func renderIcons() *icons {
	ic := &icons{
		idle:   renderIcon(iconIdle, 0),
		paused: renderIcon(iconPaused, 0),
		err:    renderIcon(iconError, 0),
	}
	for i := range ic.spin {
		ic.spin[i] = renderIcon(iconSyncing, float64(i)*math.Pi/spinFrames)
	}
	return ic
}

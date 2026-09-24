//go:build ignore

// gen_favicon.go — one-shot generator for internal/web/static/favicon.png.
// Rasterizes the TeleDoc document icon (rounded tile with three slots, same
// shape as "assets/logo transparent.svg") in the brand purple #7c5cff.
// Run with: go run gen_favicon.go
package main

import (
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
)

const S = 256

var purple = color.NRGBA{R: 0x7c, G: 0x5c, B: 0xff, A: 0xff} // #7c5cff
var transparent = color.NRGBA{A: 0}

func main() {
	dst := image.NewNRGBA(image.Rect(0, 0, S, S))
	draw.Draw(dst, dst.Bounds(), &image.Uniform{transparent}, image.Point{}, draw.Src)

	// Supersample 4x for smooth edges (SVG-like anti-aliasing).
	SS := 4
	buf := image.NewNRGBA(image.Rect(0, 0, S*SS, S*SS))
	draw.Draw(buf, buf.Bounds(), &image.Uniform{transparent}, image.Point{}, draw.Src)

	// Shape geometry in a 24x24 viewBox, scaled by SS then downsampled.
	// Rounded rect: x 3..21, y 2..22, r 5 (from the SVG path's arc radii).
	tile := rect{minX: 3, minY: 2, maxX: 21, maxY: 22, r: 5}
	// Slots (lines): y = 8, 12, 16; x 8..16, 8..16, 8..13; half-height 0.75
	slots := []rect{
		{minX: 8 - 0.6, minY: 8 - 0.75, maxX: 16 + 0.6, maxY: 8 + 0.75, r: 0.75},
		{minX: 8 - 0.6, minY: 12 - 0.75, maxX: 16 + 0.6, maxY: 12 + 0.75, r: 0.75},
		{minX: 8 - 0.6, minY: 16 - 0.75, maxX: 13 + 0.6, maxY: 16 + 0.75, r: 0.75},
	}

	// Icons look better with slightly rounder corners at small sizes.
	tile.r = 5.5

	scale := float64(S * SS) / 24.0

	for py := 0; py < S*SS; py++ {
		for px := 0; px < S*SS; px++ {
			x := (float64(px) + 0.5) / scale
			y := (float64(py) + 0.5) / scale
			if roundedRectContains(tile, x, y) && !anySlotContains(slots, x, y) {
				buf.SetNRGBA(px, py, purple)
			}
		}
	}

	// Downsample SS x SS -> 1 px (box filter).
	for py := 0; py < S; py++ {
		for px := 0; px < S; px++ {
			var r, g, b, a uint32
			for sy := 0; sy < SS; sy++ {
				for sx := 0; sx < SS; sx++ {
					c := buf.NRGBAAt(px*SS+sx, py*SS+sy)
					r += uint32(c.R)
					g += uint32(c.G)
					b += uint32(c.B)
					a += uint32(c.A)
				}
			}
			n := uint32(SS * SS)
			dst.SetNRGBA(px, py, color.NRGBA{
				R: uint8(r / n), G: uint8(g / n), B: uint8(b / n), A: uint8(a / n),
			})
		}
	}

	f, err := os.Create("internal/web/static/favicon.png")
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, dst); err != nil {
		panic(err)
	}
	println("wrote internal/web/static/favicon.png (" + itoa(S) + "x" + itoa(S) + ")")
}

type rect struct{ minX, minY, maxX, maxY, r float64 }

// roundedRectContains reports whether the point is inside the rounded
// rectangle with corner radius r, via the standard signed-distance test:
// inside iff length(max(q,0)) <= r where q is the point's distance from the
// inner rect (shrunk by r on each side).
func roundedRectContains(rc rect, x, y float64) bool {
	cx, cy := (rc.minX+rc.maxX)/2, (rc.minY+rc.maxY)/2
	hx, hy := (rc.maxX-rc.minX)/2, (rc.maxY-rc.minY)/2
	qx := math.Abs(x-cx) - (hx - rc.r)
	qy := math.Abs(y-cy) - (hy - rc.r)
	ox := math.Max(qx, 0)
	oy := math.Max(qy, 0)
	return ox*ox+oy*oy <= rc.r*rc.r
}

func anySlotContains(slots []rect, x, y float64) bool {
	for _, s := range slots {
		if roundedRectContains(s, x, y) {
			return true
		}
	}
	return false
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := ""
	for n > 0 {
		digits = string(rune('0'+n%10)) + digits
		n /= 10
	}
	return digits
}

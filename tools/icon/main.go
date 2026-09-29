// Command icon draws Kinotape's app icon as a 1024 px PNG: a deep indigo squircle with a rounded play mark
// over a resume bar, the "pick up where you left off" idea the app is built around.
//
// Usage: go run ./tools/icon out.png
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
)

const size = 1024

type rgb [3]float64

var (
	topTone    = rgb{0x2E, 0x4F, 0xAE}
	bottomTone = rgb{0x19, 0x2C, 0x6E}
	white      = rgb{0xF7, 0xF8, 0xFB}
	saffron    = rgb{0xF6, 0xB6, 0x2E}
)

// main writes the icon to the path given on the command line.
func main() {
	if len(os.Args) != 2 {
		log.Fatal("usage: icon out.png")
	}
	file, err := os.Create(os.Args[1])
	if err != nil {
		log.Fatal(err)
	}
	defer file.Close()
	if err := png.Encode(file, draw()); err != nil {
		log.Fatal(err)
	}
}

// draw renders every layer with anti-aliased edges computed from signed distances.
func draw() *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	triangle := [3][2]float64{{414, 300}, {414, 612}, {690, 456}}
	const barLeft, barRight, barY, barHalf, played = 312.0, 712.0, 742.0, 13.0, 0.62
	knobX := barLeft + (barRight-barLeft)*played
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			var out rgb
			var alpha float64

			shadow := 0.30 * (1 - smoothstep(-6, 26, squircle(px, py-14, 404)))
			out, alpha = over(out, alpha, rgb{0, 0, 16}, shadow)

			if tile := coverage(squircle(px, py, 412)); tile > 0 {
				t := (py - 100) / 824
				bg := lerp(topTone, bottomTone, math.Min(1, math.Max(0, t)))
				out, alpha = over(out, alpha, bg, tile)
				out, alpha = over(out, alpha, white, coverage(roundedTriangle(px, py, triangle, 34)))
				track := capsule(px, py, barLeft, barRight, barY, barHalf)
				out, alpha = over(out, alpha, white, 0.26*coverage(track))
				out, alpha = over(out, alpha, saffron, coverage(capsule(px, py, barLeft, knobX, barY, barHalf)))
				knob := math.Hypot(px-knobX, py-barY)
				out, alpha = over(out, alpha, white, coverage(knob-30))
				out, alpha = over(out, alpha, saffron, coverage(knob-19))
			}
			if alpha > 0 {
				img.SetNRGBA(x, y, color.NRGBA{R: channel(out[0] / alpha), G: channel(out[1] / alpha), B: channel(out[2] / alpha), A: channel(alpha * 255)})
			}
		}
	}
	return img
}

// over composites a colour with the given coverage onto premultiplied colour and alpha.
func over(dst rgb, dstAlpha float64, src rgb, a float64) (rgb, float64) {
	if a <= 0 {
		return dst, dstAlpha
	}
	return rgb{src[0]*a + dst[0]*(1-a), src[1]*a + dst[1]*(1-a), src[2]*a + dst[2]*(1-a)}, a + dstAlpha*(1-a)
}

// squircle is the signed distance to the superellipse macOS uses for icon tiles, centred on the canvas.
func squircle(px, py, half float64) float64 {
	const n = 5.0
	x, y := math.Abs(px-512), math.Abs(py-512)
	r := math.Pow(math.Pow(x, n)+math.Pow(y, n), 1/n)
	if r == 0 {
		return -half
	}
	gx, gy := math.Pow(x/r, n-1), math.Pow(y/r, n-1)
	return (r - half) / math.Max(math.Hypot(gx, gy), 1e-6)
}

// roundedTriangle is the signed distance to a triangle whose corners are rounded by radius.
func roundedTriangle(px, py float64, t [3][2]float64, radius float64) float64 {
	best, positive, negative := math.Inf(1), 0, 0
	for i := 0; i < 3; i++ {
		a, b := t[i], t[(i+1)%3]
		ex, ey := b[0]-a[0], b[1]-a[1]
		vx, vy := px-a[0], py-a[1]
		h := math.Min(1, math.Max(0, (vx*ex+vy*ey)/(ex*ex+ey*ey)))
		best = math.Min(best, math.Hypot(vx-ex*h, vy-ey*h))
		if ex*vy-ey*vx < 0 {
			negative++
		} else {
			positive++
		}
	}
	if positive == 3 || negative == 3 {
		return -best - radius
	}
	return best - radius
}

// capsule is the signed distance to a horizontal bar with round ends.
func capsule(px, py, left, right, cy, half float64) float64 {
	cx := math.Min(right, math.Max(left, px))
	return math.Hypot(px-cx, py-cy) - half
}

// coverage turns a signed distance into anti-aliased pixel coverage.
func coverage(distance float64) float64 {
	return math.Min(1, math.Max(0, 0.5-distance))
}

// smoothstep eases from 0 to 1 between edge0 and edge1.
func smoothstep(edge0, edge1, x float64) float64 {
	t := math.Min(1, math.Max(0, (x-edge0)/(edge1-edge0)))
	return t * t * (3 - 2*t)
}

// lerp mixes two colours.
func lerp(a, b rgb, t float64) rgb {
	return rgb{a[0] + (b[0]-a[0])*t, a[1] + (b[1]-a[1])*t, a[2] + (b[2]-a[2])*t}
}

// channel rounds a colour value into a byte.
func channel(v float64) uint8 {
	return uint8(math.Round(math.Min(255, math.Max(0, v))))
}

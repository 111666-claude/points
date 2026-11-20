// Copyright Bjørn Borud 2019 Use of this source code is governed by
// the license found in the accompanying LICENSE file.
//
// CMYK halftone screening for points.
//
// The input image is separated into C, M, Y and K channels.  Each
// channel is screened with its own screen angle and line frequency
// (lines per inch at the given DPI), producing a grid of dots whose
// area is proportional to the channel coverage in each screen cell.
// The four layers can be exported as individual separation files or
// as a single composite preview (channels blended with multiply).
package main

import (
	"fmt"
	"image"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	svg "github.com/ajstarks/svgo"
)

// Channel indices into the CMYK arrays.
const (
	chC = 0
	chM = 1
	chY = 2
	chK = 3
)

var (
	channelNames  = [4]string{"C", "M", "Y", "K"}
	channelColors = [4]string{"#00ffff", "#ff00ff", "#ffff00", "#000000"}
)

// Resource limits.  These can be overridden through flags in main.go.
const (
	defaultMaxPixels = int64(100000000) // max source pixels
	defaultMaxCells  = int64(8000000)   // max screen cells per channel
	maxDPI           = 9600
)

// halftoneParams holds everything needed to render a CMYK halftone.
type halftoneParams struct {
	DPI       int        // output resolution, dots per inch
	LPI       int        // screen ruling, lines per inch
	Angles    [4]float64 // screen angles in degrees, C M Y K
	Scale     int        // output scale factor, same meaning as -s
	MaxPixels int64      // resource limit: max source pixels
	MaxCells  int64      // resource limit: max screen cells per channel
}

// cell returns the screen cell size in source pixels.
func (p halftoneParams) cell() float64 {
	return float64(p.DPI) / float64(p.LPI)
}

// validate checks the parameters and the image bounds, returning a
// descriptive error for invalid DPI/angle/LPI combinations, empty or
// too small images and canvases that exceed the resource limits.
func (p halftoneParams) validate(b image.Rectangle) error {
	if p.DPI < 1 || p.DPI > maxDPI {
		return fmt.Errorf("invalid DPI %d: must be between 1 and %d", p.DPI, maxDPI)
	}
	if p.LPI < 1 {
		return fmt.Errorf("invalid LPI %d: must be at least 1", p.LPI)
	}
	if p.LPI > p.DPI {
		return fmt.Errorf("invalid LPI %d: screen ruling cannot exceed DPI %d (cell would be smaller than one pixel)", p.LPI, p.DPI)
	}
	for i, a := range p.Angles {
		if math.IsNaN(a) || math.IsInf(a, 0) {
			return fmt.Errorf("invalid angle for channel %s: %v", channelNames[i], a)
		}
	}
	if p.Scale < 1 {
		return fmt.Errorf("invalid scale %d: must be at least 1", p.Scale)
	}

	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return fmt.Errorf("empty image: bounds %dx%d", w, h)
	}
	if p.MaxPixels > 0 && int64(w)*int64(h) > p.MaxPixels {
		return fmt.Errorf("image too large: %dx%d = %d pixels exceeds limit of %d", w, h, int64(w)*int64(h), p.MaxPixels)
	}

	cell := p.cell()
	if float64(w) < cell && float64(h) < cell {
		return fmt.Errorf("image too small: %dx%d cannot contain a single screen cell (%.2f px at %d DPI / %d LPI)", w, h, cell, p.DPI, p.LPI)
	}

	// Worst case cell count: the rotated grid can span the image
	// diagonal in both directions.
	diag := math.Hypot(float64(w), float64(h))
	perSide := int64(math.Ceil(diag/cell)) + 2
	if p.MaxCells > 0 && perSide*perSide > p.MaxCells {
		return fmt.Errorf("screen too fine: up to %d cells per channel exceeds limit of %d (increase LPI cell size or raise -maxcells)", perSide*perSide, p.MaxCells)
	}
	return nil
}

// parseAngles parses a comma separated list of exactly four screen
// angles in degrees (C, M, Y, K order) and normalizes them to [0,360).
func parseAngles(s string) ([4]float64, error) {
	var angles [4]float64
	parts := strings.Split(s, ",")
	if len(parts) != 4 {
		return angles, fmt.Errorf("need exactly 4 angles (C,M,Y,K), got %d in %q", len(parts), s)
	}
	for i, part := range parts {
		v, err := strconv.ParseFloat(strings.TrimSpace(part), 64)
		if err != nil {
			return angles, fmt.Errorf("invalid angle %q for channel %s: %v", part, channelNames[i], err)
		}
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return angles, fmt.Errorf("invalid angle %q for channel %s: not a finite number", part, channelNames[i])
		}
		v = math.Mod(v, 360.0)
		if v < 0 {
			v += 360.0
		}
		angles[i] = v
	}
	return angles, nil
}

// rgbToCMYK converts r, g, b in [0,1] to c, m, y, k in [0,1] using
// the standard naive separation.
func rgbToCMYK(r, g, b float64) (c, m, y, k float64) {
	k = 1.0 - math.Max(r, math.Max(g, b))
	if k >= 1.0 {
		return 0.0, 0.0, 0.0, 1.0
	}
	d := 1.0 - k
	c = (1.0 - r - k) / d
	m = (1.0 - g - k) / d
	y = (1.0 - b - k) / d
	return c, m, y, k
}

// compositeWhite returns the r, g, b of the pixel at (x, y)
// composited over an opaque white background, each in [0,1].
// Fully transparent pixels become white, i.e. zero coverage.
func compositeWhite(img image.Image, x, y int) (r, g, b float64) {
	rr, gg, bb, aa := img.At(x, y).RGBA()
	const max = 65535.0
	inv := max - float64(aa)
	return (float64(rr) + inv) / max, (float64(gg) + inv) / max, (float64(bb) + inv) / max
}

// cellID identifies one screen cell in the rotated screen grid.
type cellID struct {
	u, v int
}

// cellAcc accumulates channel coverage for one screen cell.
type cellAcc struct {
	sum float64
	n   int
}

// dot is one halftone dot in source pixel coordinates.
type dot struct {
	x, y, r float64
}

// screenChannel computes the halftone dots for one channel.  The
// source image is sampled through a grid rotated by angleDeg around
// the image center; every source pixel is assigned to exactly one
// cell, so edge cells of non-divisible images simply average fewer
// pixels.  Cells are returned sorted by (v, u) so output is
// deterministic regardless of map iteration order.
func screenChannel(img image.Image, ch int, angleDeg, cellSize float64) []dot {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	cx, cy := float64(w)/2.0, float64(h)/2.0

	rad := angleDeg * math.Pi / 180.0
	cos, sin := math.Cos(rad), math.Sin(rad)

	cells := make(map[cellID]cellAcc)
	for y := 0; y < h; y++ {
		fy := float64(y) + 0.5 - cy
		for x := 0; x < w; x++ {
			fx := float64(x) + 0.5 - cx
			// Rotate into screen space.
			u := fx*cos + fy*sin
			v := -fx*sin + fy*cos
			id := cellID{int(math.Floor(u / cellSize)), int(math.Floor(v / cellSize))}

			r, g, bl := compositeWhite(img, b.Min.X+x, b.Min.Y+y)
			c, m, yv, k := rgbToCMYK(r, g, bl)
			var val float64
			switch ch {
			case chC:
				val = c
			case chM:
				val = m
			case chY:
				val = yv
			case chK:
				val = k
			}

			a := cells[id]
			a.sum += val
			a.n++
			cells[id] = a
		}
	}

	ids := make([]cellID, 0, len(cells))
	for id := range cells {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		if ids[i].v != ids[j].v {
			return ids[i].v < ids[j].v
		}
		return ids[i].u < ids[j].u
	})

	dots := make([]dot, 0, len(ids))
	for _, id := range ids {
		a := cells[id]
		coverage := a.sum / float64(a.n)
		if coverage <= 0.0 {
			continue
		}
		if coverage > 1.0 {
			coverage = 1.0
		}
		// Dot area equals coverage * cell area.
		r := cellSize * math.Sqrt(coverage/math.Pi)

		// Cell center in screen space, rotated back to image space.
		uc := (float64(id.u) + 0.5) * cellSize
		vc := (float64(id.v) + 0.5) * cellSize
		dots = append(dots, dot{
			x: uc*cos - vc*sin + cx,
			y: uc*sin + vc*cos + cy,
			r: r,
		})
	}
	return dots
}

// computeDots validates the parameters and computes the halftone dots
// for all four channels.
func computeDots(img image.Image, p halftoneParams) ([4][]dot, error) {
	var dots [4][]dot
	if err := p.validate(img.Bounds()); err != nil {
		return dots, err
	}
	cell := p.cell()
	for ch := 0; ch < 4; ch++ {
		dots[ch] = screenChannel(img, ch, p.Angles[ch], cell)
	}
	return dots, nil
}

// drawDots draws one channel's dots onto the canvas, scaled.
func drawDots(canvas *svg.SVG, dots []dot, color string, scale int) {
	s := float64(scale)
	for _, d := range dots {
		r := int(math.Round(d.r * s))
		if r <= 0 {
			continue
		}
		canvas.Circle(int(math.Round(d.x*s)), int(math.Round(d.y*s)), r,
			fmt.Sprintf("fill:%s;stroke:none", color))
	}
}

// writeCompositeSVG writes all four channels into a single SVG
// preview: white background, each channel in a group blended with
// mix-blend-mode:multiply so overlapping inks darken like print.
func writeCompositeSVG(outputFileName string, img image.Image, p halftoneParams, dots [4][]dot) error {
	f, err := os.Create(outputFileName)
	if err != nil {
		return fmt.Errorf("unable to open svg file %s: %v", outputFileName, err)
	}
	defer f.Close()

	w := img.Bounds().Dx() * p.Scale
	h := img.Bounds().Dy() * p.Scale

	canvas := svg.New(f)
	canvas.Start(w, h)
	defer canvas.End()

	canvas.Rect(0, 0, w, h, "fill:white;stroke:none")
	for ch := 0; ch < 4; ch++ {
		canvas.Gstyle("mix-blend-mode:multiply")
		drawDots(canvas, dots[ch], channelColors[ch], p.Scale)
		canvas.Gend()
	}
	return nil
}

// writeSeparationsSVG writes one SVG per channel.  The output files
// are named <base>_C.svg, <base>_M.svg, <base>_Y.svg and <base>_K.svg
// where <base> is the output file name without its .svg suffix.
// Separations have a transparent background so they can be plated
// directly.
func writeSeparationsSVG(outputFileName string, img image.Image, p halftoneParams, dots [4][]dot) ([]string, error) {
	base := strings.TrimSuffix(outputFileName, ".svg")
	w := img.Bounds().Dx() * p.Scale
	h := img.Bounds().Dy() * p.Scale

	names := make([]string, 0, 4)
	for ch := 0; ch < 4; ch++ {
		name := fmt.Sprintf("%s_%s.svg", base, channelNames[ch])
		f, err := os.Create(name)
		if err != nil {
			return names, fmt.Errorf("unable to open svg file %s: %v", name, err)
		}
		canvas := svg.New(f)
		canvas.Start(w, h)
		drawDots(canvas, dots[ch], channelColors[ch], p.Scale)
		canvas.End()
		f.Close()
		names = append(names, name)
	}
	return names, nil
}

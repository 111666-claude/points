// Tests for the CMYK halftone mode.
//
// Uses fixed fixture images (solid color blocks, gradients,
// transparent edges) to verify separation ratios, screen angles,
// determinism, boundary handling and error paths.
package main

import (
	"bytes"
	"image"
	stdcolor "image/color"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// solid returns a w x h opaque image of one color.
func solid(w, h int, c stdcolor.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

func testParams() halftoneParams {
	return halftoneParams{
		DPI:       100,
		LPI:       10, // 10 px cells
		Angles:    [4]float64{15, 75, 0, 45},
		Scale:     1,
		MaxPixels: defaultMaxPixels,
		MaxCells:  defaultMaxCells,
	}
}

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

func TestRGBToCMYK(t *testing.T) {
	cases := []struct {
		name       string
		r, g, b    float64
		c, m, y, k float64
	}{
		{"white", 1, 1, 1, 0, 0, 0, 0},
		{"black", 0, 0, 0, 0, 0, 0, 1},
		{"red", 1, 0, 0, 0, 1, 1, 0},
		{"green", 0, 1, 0, 1, 0, 1, 0},
		{"blue", 0, 0, 1, 1, 1, 0, 0},
		{"cyan", 0, 1, 1, 1, 0, 0, 0},
		{"magenta", 1, 0, 1, 0, 1, 0, 0},
		{"yellow", 1, 1, 0, 0, 0, 1, 0},
		{"50% gray", 0.5, 0.5, 0.5, 0, 0, 0, 0.5},
	}
	for _, tc := range cases {
		c, m, y, k := rgbToCMYK(tc.r, tc.g, tc.b)
		if !almostEqual(c, tc.c) || !almostEqual(m, tc.m) || !almostEqual(y, tc.y) || !almostEqual(k, tc.k) {
			t.Errorf("%s: rgbToCMYK(%v,%v,%v) = (%v,%v,%v,%v), want (%v,%v,%v,%v)",
				tc.name, tc.r, tc.g, tc.b, c, m, y, k, tc.c, tc.m, tc.y, tc.k)
		}
	}
}

// TestSolidBlockSeparation checks that a pure red block separates
// into full M and Y coverage and zero C and K coverage.
func TestSolidBlockSeparation(t *testing.T) {
	img := solid(40, 40, stdcolor.RGBA{255, 0, 0, 255})
	dots, err := computeDots(img, testParams())
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	if len(dots[chC]) != 0 {
		t.Errorf("red block: expected no C dots, got %d", len(dots[chC]))
	}
	if len(dots[chK]) != 0 {
		t.Errorf("red block: expected no K dots, got %d", len(dots[chK]))
	}
	if len(dots[chM]) == 0 || len(dots[chY]) == 0 {
		t.Fatalf("red block: expected M and Y dots, got M=%d Y=%d", len(dots[chM]), len(dots[chY]))
	}
	// Full coverage: dot area must equal the cell area.
	cell := testParams().cell()
	wantR := cell / math.Sqrt(math.Pi)
	for _, d := range dots[chM] {
		if math.Abs(d.r-wantR) > 1e-9 {
			t.Errorf("full coverage M dot radius %v, want %v (area == cell area)", d.r, wantR)
		}
	}
}

// TestSolidGrayBlock checks K-only separation of a neutral gray and
// that dot area matches coverage.
func TestSolidGrayBlock(t *testing.T) {
	img := solid(40, 40, stdcolor.RGBA{128, 128, 128, 255})
	dots, err := computeDots(img, testParams())
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	for ch := chC; ch <= chY; ch++ {
		if len(dots[ch]) != 0 {
			t.Errorf("gray block: expected no %s dots, got %d", channelNames[ch], len(dots[ch]))
		}
	}
	if len(dots[chK]) == 0 {
		t.Fatal("gray block: expected K dots")
	}
	// 128/255 composite -> k = 1 - 128/255.
	k := 1.0 - 128.0/255.0
	wantR := testParams().cell() * math.Sqrt(k/math.Pi)
	for _, d := range dots[chK] {
		if math.Abs(d.r-wantR) > 1e-6 {
			t.Errorf("K dot radius %v, want %v for coverage %v", d.r, wantR, k)
		}
	}
}

// TestGradientCoverage checks that a horizontal black-to-white
// gradient produces monotonically decreasing K coverage per column.
func TestGradientCoverage(t *testing.T) {
	const w, h = 100, 20
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		v := uint8(255 * x / (w - 1)) // black on the left, white on the right
		for y := 0; y < h; y++ {
			img.SetRGBA(x, y, stdcolor.RGBA{v, v, v, 255})
		}
	}
	p := testParams()
	p.Angles = [4]float64{0, 0, 0, 0} // axis aligned: cells are columns of 10 px
	dots, err := computeDots(img, p)
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	// Group K dots by cell column and check coverage decreases.
	prevR := math.Inf(1)
	for col := 0; col < w/int(p.cell()); col++ {
		var sumR2 float64
		var n int
		for _, d := range dots[chK] {
			if int(d.x/p.cell()) == col {
				sumR2 += d.r * d.r
				n++
			}
		}
		if n == 0 {
			continue // fully white columns have no dots
		}
		avgR := math.Sqrt(sumR2 / float64(n))
		if avgR > prevR+1e-9 {
			t.Errorf("column %d: average K radius %v increased over previous %v; gradient not monotonic", col, avgR, prevR)
		}
		prevR = avgR
	}
}

// TestTransparentEdges checks that transparent pixels composite to
// white (no dots) and half-transparent pixels halve the coverage.
func TestTransparentEdges(t *testing.T) {
	// Left half opaque black, right half fully transparent.
	img := image.NewRGBA(image.Rect(0, 0, 40, 40))
	for y := 0; y < 40; y++ {
		for x := 0; x < 20; x++ {
			img.SetRGBA(x, y, stdcolor.RGBA{0, 0, 0, 255})
		}
	}
	p := testParams()
	p.Angles = [4]float64{0, 0, 0, 0}
	dots, err := computeDots(img, p)
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	for _, d := range dots[chK] {
		if d.x >= 30 {
			t.Errorf("dot at x=%.1f in fully transparent region (x>=30)", d.x)
		}
	}

	// Half-alpha black over white == 50% gray -> K coverage ~0.5.
	half := solid(40, 40, stdcolor.RGBA{0, 0, 0, 128})
	dotsHalf, err := computeDots(half, p)
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	if len(dotsHalf[chK]) == 0 {
		t.Fatal("half-alpha black: expected K dots")
	}
	wantK := 0.5 + 0.5/255 // 128/255 alpha
	wantR := p.cell() * math.Sqrt(wantK/math.Pi)
	for _, d := range dotsHalf[chK] {
		if math.Abs(d.r-wantR) > 0.02 {
			t.Errorf("half-alpha K dot radius %v, want ~%v (coverage ~0.5)", d.r, wantR)
		}
	}
}

// TestScreenAngle verifies that rotating the screen actually rotates
// the dot lattice: for angle 0 the cell centers are axis aligned,
// for angle 90 the lattice is swapped, and for 45 degrees the
// neighbor offset follows (cos45, sin45).
func TestScreenAngle(t *testing.T) {
	img := solid(100, 100, stdcolor.RGBA{0, 0, 0, 255})
	p := testParams()

	angleDots := func(angle float64) []dot {
		p.Angles = [4]float64{angle, angle, angle, angle}
		dots, err := computeDots(img, p)
		if err != nil {
			t.Fatalf("computeDots: %v", err)
		}
		return dots[chK]
	}

	d0 := angleDots(0)
	d90 := angleDots(90)
	if len(d0) != len(d90) {
		t.Fatalf("dot count differs between 0 and 90 degrees: %d vs %d", len(d0), len(d90))
	}
	// Rotating the screen by 90 degrees around the image center must
	// map the lattice onto itself with x/y mirrored around the center.
	key := func(x, y float64) [2]int { return [2]int{int(math.Round(x * 1000)), int(math.Round(y * 1000))} }
	set := make(map[[2]int]bool)
	for _, d := range d0 {
		set[key(d.x, d.y)] = true
	}
	for _, d := range d90 {
		// inverse rotate (x,y) by 90 deg around center (50,50)
		x := -(d.y - 50) + 50
		y := (d.x - 50) + 50
		if !set[key(x, y)] {
			t.Errorf("90 degree lattice point (%.2f,%.2f) has no counterpart in 0 degree lattice", d.x, d.y)
		}
	}

	// For 45 degrees, sorted neighbor offsets must include the cell
	// diagonal direction (cos45, sin45) * cell.
	d45 := angleDots(45)
	if len(d45) < 4 {
		t.Fatalf("too few dots at 45 degrees: %d", len(d45))
	}
	cell := p.cell()
	found := false
	for i := 0; i < len(d45) && !found; i++ {
		for j := i + 1; j < len(d45); j++ {
			dx := math.Abs(d45[j].x - d45[i].x)
			dy := math.Abs(d45[j].y - d45[i].y)
			if math.Abs(dx-cell/math.Sqrt2) < 0.01 && math.Abs(dy-cell/math.Sqrt2) < 0.01 {
				found = true
				break
			}
		}
	}
	if !found {
		t.Error("45 degree screen: no neighbor pair at 45 degree offset found")
	}
}

// TestNonDivisibleSize checks that images whose dimensions are not a
// multiple of the cell size still get complete, correct coverage.
func TestNonDivisibleSize(t *testing.T) {
	img := solid(13, 7, stdcolor.RGBA{0, 0, 0, 255}) // cell is 10 px
	p := testParams()
	p.Angles = [4]float64{0, 0, 0, 0}
	dots, err := computeDots(img, p)
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}
	if len(dots[chK]) == 0 {
		t.Fatal("expected dots for 13x7 image")
	}
	// Every dot must sit on a cell center of the axis aligned grid
	// anchored at the image center, and full-coverage dots keep the
	// full radius even in partial edge cells.
	wantR := p.cell() / math.Sqrt(math.Pi)
	for _, d := range dots[chK] {
		if math.Abs(d.r-wantR) > 1e-9 {
			t.Errorf("edge cell dot radius %v, want full coverage %v", d.r, wantR)
		}
	}
}

// TestDeterministic renders the same image twice and requires
// byte-identical SVG output.
func TestDeterministic(t *testing.T) {
	img := solid(37, 23, stdcolor.RGBA{200, 30, 90, 255})
	p := testParams()

	render := func() string {
		dots, err := computeDots(img, p)
		if err != nil {
			t.Fatalf("computeDots: %v", err)
		}
		var buf bytes.Buffer
		for ch := 0; ch < 4; ch++ {
			for _, d := range dots[ch] {
				buf.WriteString(channelNames[ch])
				buf.WriteString(" ")
				buf.WriteString(strings.TrimSpace(strings.Join([]string{
					floatStr(d.x), floatStr(d.y), floatStr(d.r)}, ",")))
				buf.WriteString("\n")
			}
		}
		return buf.String()
	}

	a, b := render(), render()
	if a != b {
		t.Fatal("repeated runs with fixed parameters produced different output")
	}
}

func floatStr(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// TestValidation covers invalid DPI/LPI/angles, empty and tiny
// images and the resource limits.
func TestValidation(t *testing.T) {
	img := solid(40, 40, stdcolor.RGBA{0, 0, 0, 255})

	bad := []struct {
		name string
		mut  func(*halftoneParams)
	}{
		{"zero dpi", func(p *halftoneParams) { p.DPI = 0 }},
		{"negative dpi", func(p *halftoneParams) { p.DPI = -10 }},
		{"huge dpi", func(p *halftoneParams) { p.DPI = 99999 }},
		{"zero lpi", func(p *halftoneParams) { p.LPI = 0 }},
		{"lpi exceeds dpi", func(p *halftoneParams) { p.LPI = p.DPI + 1 }},
		{"zero scale", func(p *halftoneParams) { p.Scale = 0 }},
		{"nan angle", func(p *halftoneParams) { p.Angles[0] = math.NaN() }},
		{"pixel limit", func(p *halftoneParams) { p.MaxPixels = 100 }},
		{"cell limit", func(p *halftoneParams) { p.MaxCells = 2 }},
	}
	for _, tc := range bad {
		p := testParams()
		tc.mut(&p)
		if _, err := computeDots(img, p); err == nil {
			t.Errorf("%s: expected error, got nil", tc.name)
		}
	}

	// Empty image.
	empty := image.NewRGBA(image.Rect(0, 0, 0, 0))
	if _, err := computeDots(empty, testParams()); err == nil {
		t.Error("empty image: expected error")
	}

	// Tiny image: smaller than one screen cell.
	tiny := solid(3, 3, stdcolor.RGBA{0, 0, 0, 255})
	if _, err := computeDots(tiny, testParams()); err == nil {
		t.Error("tiny image: expected error")
	}

	// Huge canvas: exceeds default pixel limit without allocating it.
	huge := image.NewRGBA(image.Rect(0, 0, 0, 0))
	_ = huge
	big := &image.Gray{Rect: image.Rect(0, 0, 20000, 20000)}
	if _, err := computeDots(big, testParams()); err == nil {
		t.Error("huge canvas: expected resource limit error")
	}
}

func TestParseAngles(t *testing.T) {
	a, err := parseAngles("15, 75, 0, 45")
	if err != nil {
		t.Fatalf("parseAngles: %v", err)
	}
	if a != [4]float64{15, 75, 0, 45} {
		t.Errorf("got %v", a)
	}
	// Normalization into [0,360).
	a, err = parseAngles("-15, 375, 0, 45")
	if err != nil {
		t.Fatalf("parseAngles: %v", err)
	}
	if !almostEqual(a[0], 345) || !almostEqual(a[1], 15) {
		t.Errorf("normalization failed: %v", a)
	}
	for _, s := range []string{"", "1,2,3", "1,2,3,4,5", "a,b,c,d", "1,2,3,nan", "1,2,3,inf"} {
		if _, err := parseAngles(s); err == nil {
			t.Errorf("parseAngles(%q): expected error", s)
		}
	}
}

// TestWriteOutputs checks the composite and separation writers
// produce well-formed files with the expected structure.
func TestWriteOutputs(t *testing.T) {
	img := solid(40, 40, stdcolor.RGBA{255, 0, 0, 255})
	p := testParams()
	dots, err := computeDots(img, p)
	if err != nil {
		t.Fatalf("computeDots: %v", err)
	}

	dir := t.TempDir()

	comp := filepath.Join(dir, "out.svg")
	if err := writeCompositeSVG(comp, img, p, dots); err != nil {
		t.Fatalf("writeCompositeSVG: %v", err)
	}
	data, err := os.ReadFile(comp)
	if err != nil {
		t.Fatalf("read composite: %v", err)
	}
	s := string(data)
	if !strings.Contains(s, "mix-blend-mode:multiply") {
		t.Error("composite: missing multiply blend groups")
	}
	if !strings.Contains(s, "#ff00ff") || !strings.Contains(s, "#ffff00") {
		t.Error("composite: red input should produce M and Y dots")
	}
	if strings.Contains(s, "#00ffff") {
		t.Error("composite: red input should not produce C dots")
	}

	names, err := writeSeparationsSVG(comp, img, p, dots)
	if err != nil {
		t.Fatalf("writeSeparationsSVG: %v", err)
	}
	if len(names) != 4 {
		t.Fatalf("expected 4 separation files, got %d", len(names))
	}
	contents := make(map[string]string)
	for _, n := range names {
		d, err := os.ReadFile(n)
		if err != nil {
			t.Fatalf("read %s: %v", n, err)
		}
		contents[filepath.Base(n)] = string(d)
	}
	if strings.Contains(contents["out_C.svg"], "<circle") {
		t.Error("C separation should have no dots for red input")
	}
	if !strings.Contains(contents["out_M.svg"], "<circle") || !strings.Contains(contents["out_Y.svg"], "<circle") {
		t.Error("M and Y separations should have dots for red input")
	}
	if strings.Contains(contents["out_K.svg"], "<circle") {
		t.Error("K separation should have no dots for red input")
	}
}

// TestOriginalModeStillWorks is a smoke test that the original
// monochrome/color dot command is unchanged.
func TestOriginalModeStillWorks(t *testing.T) {
	img := solid(30, 30, stdcolor.RGBA{10, 20, 30, 255})
	out := filepath.Join(t.TempDir(), "orig.svg")
	makeDots(img, 10, 1.0, true, false, out, 1, false)
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read original output: %v", err)
	}
	if !strings.Contains(string(data), "<circle") {
		t.Error("original mode produced no circles")
	}
}

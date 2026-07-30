package palette

import (
	"image"
	"image/color"
	"testing"
)

// solid builds a w×h image filled with one opaque colour.
func solid(w, h int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func TestExtractExactCountsAndOrder(t *testing.T) {
	// A 4×4 image: 10 red, 4 green, 2 blue pixels.
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	red := color.NRGBA{R: 0xff, A: 0xff}
	green := color.NRGBA{G: 0xff, A: 0xff}
	blue := color.NRGBA{B: 0xff, A: 0xff}
	fill := []color.NRGBA{
		red, red, red, red,
		red, red, red, red,
		red, red, green, green,
		green, green, blue, blue,
	}
	i := 0
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.Set(x, y, fill[i])
			i++
		}
	}

	p := Extract(img, Options{})
	if p.Kind != KindExact {
		t.Fatalf("Kind = %q, want exact", p.Kind)
	}
	if p.Visible != 16 {
		t.Errorf("Visible = %d, want 16", p.Visible)
	}
	if len(p.Swatches) != 3 {
		t.Fatalf("got %d swatches, want 3", len(p.Swatches))
	}
	// Most-used first: red (10), green (4), blue (2).
	want := []struct {
		hex   string
		count int
	}{{"#ff0000", 10}, {"#00ff00", 4}, {"#0000ff", 2}}
	for j, w := range want {
		if p.Swatches[j].Hex != w.hex || p.Swatches[j].Count != w.count {
			t.Errorf("swatch %d = %s/%d, want %s/%d",
				j, p.Swatches[j].Hex, p.Swatches[j].Count, w.hex, w.count)
		}
	}
	if got := p.Swatches[0].Ratio; got != 10.0/16.0 {
		t.Errorf("red ratio = %v, want %v", got, 10.0/16.0)
	}
}

func TestExtractExcludesFullyTransparent(t *testing.T) {
	// Half opaque red, half fully transparent. Only red is a visible colour, and
	// the transparent pixels must not dominate as "transparent black" (§8).
	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 0xff, A: 0xff})
	img.Set(1, 0, color.NRGBA{R: 0xff, A: 0xff})
	img.Set(0, 1, color.NRGBA{}) // transparent
	img.Set(1, 1, color.NRGBA{}) // transparent

	p := Extract(img, Options{})
	if p.Visible != 2 {
		t.Errorf("Visible = %d, want 2", p.Visible)
	}
	if len(p.Swatches) != 1 || p.Swatches[0].Hex != "#ff0000" {
		t.Fatalf("swatches = %+v, want a single #ff0000", p.Swatches)
	}
	if p.HasSemitransparent {
		t.Error("HasSemitransparent = true, want false")
	}
}

func TestExtractSemitransparentFlag(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 2, 1))
	img.Set(0, 0, color.NRGBA{R: 0xff, A: 0xff})
	img.Set(1, 0, color.NRGBA{R: 0xff, A: 0x80}) // partial alpha

	p := Extract(img, Options{})
	if !p.HasSemitransparent {
		t.Error("HasSemitransparent = false, want true")
	}
	if p.Visible != 2 {
		t.Errorf("Visible = %d, want 2 (semi-transparent still counts)", p.Visible)
	}
}

func TestExtractFullyTransparentImage(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4)) // all zero = transparent
	p := Extract(img, Options{})
	if p.Kind != KindExact {
		t.Errorf("Kind = %q, want exact", p.Kind)
	}
	if p.Visible != 0 || len(p.Swatches) != 0 {
		t.Errorf("want empty palette, got Visible=%d swatches=%d", p.Visible, len(p.Swatches))
	}
}

func TestExtractQuantizesManyColors(t *testing.T) {
	// A 64×64 image where every pixel is a distinct colour — far past the exact
	// threshold, so the quantized path must run.
	img := image.NewNRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.NRGBA{R: uint8(x * 4), G: uint8(y * 4), B: uint8((x + y) * 2), A: 0xff})
		}
	}

	p := Extract(img, Options{QuantizeN: 16})
	if p.Kind != KindQuantized {
		t.Fatalf("Kind = %q, want quantized", p.Kind)
	}
	if len(p.Swatches) == 0 || len(p.Swatches) > 16 {
		t.Fatalf("got %d swatches, want 1..16", len(p.Swatches))
	}
	if p.Visible != 64*64 {
		t.Errorf("Visible = %d, want %d", p.Visible, 64*64)
	}
	// Ratios of a partition sum to ~1.
	var sum float64
	for _, s := range p.Swatches {
		sum += s.Ratio
	}
	if sum < 0.99 || sum > 1.01 {
		t.Errorf("ratios sum to %v, want ~1", sum)
	}
}

func TestExtractExactThresholdBoundary(t *testing.T) {
	// Exactly MaxExactColors distinct colours stays exact; one more tips to quantized.
	opts := Options{MaxExactColors: 8, QuantizeN: 4}

	atLimit := manyColorImage(8)
	if p := Extract(atLimit, opts); p.Kind != KindExact {
		t.Errorf("8 colours with limit 8: Kind = %q, want exact", p.Kind)
	}

	over := manyColorImage(9)
	if p := Extract(over, opts); p.Kind != KindQuantized {
		t.Errorf("9 colours with limit 8: Kind = %q, want quantized", p.Kind)
	}
}

// manyColorImage returns a 1×n image with n distinct opaque colours.
func manyColorImage(n int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, n, 1))
	for i := 0; i < n; i++ {
		img.Set(i, 0, color.NRGBA{R: uint8(i), G: uint8(255 - i), B: uint8(i * 7), A: 0xff})
	}
	return img
}

func TestExtractDeterministic(t *testing.T) {
	img := manyColorImage(300) // forces the quantized path
	a := Extract(img, Options{})
	b := Extract(img, Options{})
	if len(a.Swatches) != len(b.Swatches) {
		t.Fatalf("swatch counts differ: %d vs %d", len(a.Swatches), len(b.Swatches))
	}
	for i := range a.Swatches {
		if a.Swatches[i] != b.Swatches[i] {
			t.Fatalf("swatch %d differs between runs: %+v vs %+v", i, a.Swatches[i], b.Swatches[i])
		}
	}
}

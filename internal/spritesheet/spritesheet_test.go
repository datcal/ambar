package spritesheet

import (
	"image"
	"image/color"
	"testing"
)

// gutterSheet builds a cols×rows grid of `cell`-px opaque cells whose interior
// seam lines (at each cell boundary) are one column/row of transparent pixels —
// the clean case detection is meant to nail.
func gutterSheet(cols, rows, cell int) *image.RGBA {
	w, h := cols*cell, rows*cell
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			onSeam := (x%cell == 0 && x > 0) || (y%cell == 0 && y > 0)
			if onSeam {
				img.Set(x, y, color.RGBA{}) // transparent gutter
			} else {
				img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x80, A: 0xff})
			}
		}
	}
	return img
}

func TestDetectGutterGrid(t *testing.T) {
	tests := []struct{ cols, rows, cell int }{
		{4, 3, 20},
		{8, 8, 16},
		{2, 5, 24},
	}
	for _, tc := range tests {
		img := gutterSheet(tc.cols, tc.rows, tc.cell)
		g, ok := Detect(img)
		if !ok {
			t.Errorf("%dx%d: no grid found", tc.cols, tc.rows)
			continue
		}
		if !g.Confident {
			t.Errorf("%dx%d: clean gutter grid not confident (score %.2f)", tc.cols, tc.rows, g.Score)
		}
		if g.Cols != tc.cols || g.Rows != tc.rows {
			t.Errorf("detected %dx%d, want %dx%d (frame %dx%d)", g.Cols, g.Rows, tc.cols, tc.rows, g.FrameW, g.FrameH)
		}
		if g.FrameCount != tc.cols*tc.rows {
			t.Errorf("frame count = %d, want %d", g.FrameCount, tc.cols*tc.rows)
		}
	}
}

func TestDetectSolidImageIsNotConfident(t *testing.T) {
	// A solid, gutterless image may yield a grid guess, but never a confident one.
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for i := range img.Pix {
		img.Pix[i] = 0xff
	}
	g, _ := Detect(img)
	if g.Confident {
		t.Errorf("a solid image was confidently gridded: %+v", g)
	}
}

func TestDetectTinyImage(t *testing.T) {
	if _, ok := Detect(image.NewRGBA(image.Rect(0, 0, 10, 10))); ok {
		t.Error("a 10x10 image should be too small to be a sheet")
	}
}

func TestIsCandidate(t *testing.T) {
	tests := []struct {
		name string
		w, h int
		want bool
	}{
		{"hero_sheet.png", 256, 64, true},
		{"explosion_atlas.png", 512, 512, true},
		{"run-anim.png", 128, 32, true},
		{"knight_8x8.png", 128, 128, true},
		{"portrait.png", 300, 300, true}, // dims divide into plausible cells
		{"tiny.png", 12, 12, false},
	}
	for _, tc := range tests {
		if got := IsCandidate(tc.name, tc.w, tc.h); got != tc.want {
			t.Errorf("IsCandidate(%q, %d, %d) = %v, want %v", tc.name, tc.w, tc.h, got, tc.want)
		}
	}
}

package derive

import (
	"image"
	"image/color"
	"testing"

	"github.com/oov/psd"
)

// The PSD flattener's rules, exercised without a .psd file.
//
// A real fixture would be better and is not available: every PSD in the library is vendor
// artwork under a licence that does not want it committed, and there is no pure-Go PSD
// *encoder* to build one with. So the layer structures below are transcribed from what the
// real files actually contain — dumped from
// craftpix-net-189780/PSD/Citizen1/Idle/Citizen1_Idle_front.psd, which is:
//
//	[0] "Background"  rect 1920x1080, visible, an image, opaque
//	[1] "Frame 1"     a folder, visible
//	[2] "Frame 2"..   folders, hidden
//
// Both of the shortcuts that look reasonable fail on that file: the backdrop's rect is
// enormous rather than canvas-sized, and it carries a transparency channel like every
// other PSD layer. Only measuring its opacity works.

// solid builds an image layer filled with one colour.
func solidLayer(name string, rect image.Rectangle, c color.NRGBA) psd.Layer {
	img := image.NewNRGBA(rect)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			img.SetNRGBA(x, y, c)
		}
	}
	return psd.Layer{
		Name:    name,
		Rect:    rect,
		Opacity: 0xff,
		Picker:  img,
		Channel: map[int]psd.Channel{-1: {}, 0: {}, 1: {}, 2: {}},
	}
}

func hide(l psd.Layer) psd.Layer {
	l.Flags |= 2
	return l
}

func folder(name string, children ...psd.Layer) psd.Layer {
	l := psd.Layer{Name: name, Layer: children}
	l.SectionDividerSetting.Type = 1
	return l
}

func TestFlattenPSDDropsTheBackdrop(t *testing.T) {
	canvas := image.Rect(0, 0, 32, 32)

	// The backdrop is deliberately much larger than the canvas, as in the real files.
	backdrop := solidLayer("Background", image.Rect(0, 0, 1920, 1080),
		color.NRGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff})
	sprite := solidLayer("body", image.Rect(8, 8, 24, 24),
		color.NRGBA{R: 0x4f, G: 0x7a, B: 0x3c, A: 0xff})

	flat := flattenPSD([]psd.Layer{
		backdrop,
		folder("Frame 1", sprite),
		hide(folder("Frame 2", solidLayer("body", canvas, color.NRGBA{R: 0xff, A: 0xff}))),
	}, canvas)

	if flat == nil {
		t.Fatal("nothing was drawn")
	}
	if got := flat.Bounds(); !got.Eq(canvas) {
		t.Errorf("bounds = %v, want the canvas %v", got, canvas)
	}

	// Outside the sprite the result must be transparent: that is the whole bug. The old
	// decoder read Photoshop's composite, which has the backdrop baked in, so a 32x32
	// sprite arrived as an opaque white square with a character in the middle.
	if _, _, _, a := flat.At(1, 1).RGBA(); a != 0 {
		t.Errorf("corner alpha = %d, want 0: the backdrop was not dropped", a)
	}
	// Inside it, the visible frame's colour and nothing from the hidden frames.
	r, g, b, a := flat.At(16, 16).RGBA()
	if a == 0 {
		t.Fatal("the sprite is missing")
	}
	if r>>8 != 0x4f || g>>8 != 0x7a || b>>8 != 0x3c {
		t.Errorf("centre = #%02x%02x%02x, want #4f7a3c (a hidden frame won)", r>>8, g>>8, b>>8)
	}
}

func TestFlattenPSDKeepsArtworkThatFillsTheCanvas(t *testing.T) {
	canvas := image.Rect(0, 0, 16, 16)

	// A single opaque layer covering the canvas *is* the artwork here — a flattened export,
	// or a texture. Dropping it would leave nothing, so flattenPSD must report that and let
	// decodePSD fall back to the composite.
	only := solidLayer("Background", canvas, color.NRGBA{R: 0x20, G: 0x40, B: 0x60, A: 0xff})

	if flat := flattenPSD([]psd.Layer{only}, canvas); flat != nil {
		t.Error("a lone canvas-filling layer was flattened; the composite fallback is the honest answer")
	}
}

func TestFlattenPSDKeepsASeeThroughBottomLayer(t *testing.T) {
	canvas := image.Rect(0, 0, 16, 16)

	// Covers the canvas but is see-through, so it is artwork rather than a backdrop.
	glass := solidLayer("wash", canvas, color.NRGBA{R: 0x10, G: 0x80, B: 0x40, A: 0x80})

	flat := flattenPSD([]psd.Layer{glass}, canvas)
	if flat == nil {
		t.Fatal("a translucent bottom layer was mistaken for a backdrop")
	}
	if _, _, _, a := flat.At(8, 8).RGBA(); a == 0 || a == 0xffff {
		t.Errorf("alpha = %d, want partial: the layer's own transparency was lost", a)
	}
}

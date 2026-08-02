// Package derive produces the thumbnails and image metadata of spec §6.
//
// The one line in the whole package that matters most is the resize filter choice.
// §6: "Nearest-neighbour resize when the image is pixel art. ... Bilinear-downscaling
// pixel art into mush is the single most annoying failure of every existing tool; get
// this right."
package derive

import (
	"image"
	"image/color"
)

// Analysis is what one decoded image tells us about itself.
type Analysis struct {
	Width  int
	Height int

	// HasAlpha is true if any pixel is not fully opaque.
	HasAlpha bool
	// HasSemitransparent counts partial alpha specifically. §8: in pixel art these
	// "are usually an authoring mistake and worth surfacing", which is why it is
	// tracked separately from HasAlpha.
	HasSemitransparent bool

	// ColorCount is the exact number of distinct visible colours, or ColorCountCap
	// when there are at least that many. Fully transparent pixels are excluded —
	// §8: otherwise "the palette is dominated by transparent black".
	ColorCount int
	// ColorCountExact is false when counting stopped at the cap.
	ColorCountExact bool

	// IsPixelArt drives both the resize filter (§6) and image-rendering: pixelated
	// in the viewer (§8).
	IsPixelArt bool

	// SoftTransitionRatio is, of the places where neighbouring pixels differ at all,
	// the fraction that differ only slightly.
	//
	// Measured against transitions rather than against every pair, which is the
	// distinction that makes it work. Both pixel art and a flat vector icon are
	// mostly uniform interior, so soft-pixels-over-all-pixels is near zero for both.
	// But *where* they change, pixel art jumps straight between palette entries while
	// antialiased art steps through intermediates — so this separates them cleanly.
	SoftTransitionRatio float64
	// TransitionSamples is how many differing pairs the ratio is based on. Below
	// minTransitions the ratio is noise and colour count decides alone.
	TransitionSamples int
}

// ColorCountCap bounds the distinct-colour set. A photograph has hundreds of
// thousands of colours and counting them all would allocate for no benefit: anything
// past the cap is emphatically not pixel art.
const ColorCountCap = 4096

// Pixel-art thresholds.
//
// §6 says to detect by "low dimensions (either axis under 256), low unique-colour
// count, and hard edges". Two of those three are used; dimensions deliberately are not,
// for two reasons:
//
//   - As a requirement it misclassifies the image that matters most. A 2048x2048
//     pixel-art tileset atlas is common in this library and is exactly what must not be
//     bilinear-downscaled, but it fails "either axis under 256".
//   - As a tie-breaker it only adds false positives. Once transitions are measured
//     properly, colour count and transition sharpness separate every case cleanly on
//     their own; a size-based relaxation just re-admits antialiased vector art, which
//     genuinely should be smoothly downscaled.
//
// Recorded as a deviation in docs/spec.md.
const (
	// Few enough colours to be a hand-picked palette rather than a photograph.
	pixelArtMaxColors = 256

	// Below this, colour count decides alone and the transition test is skipped.
	//
	// This exists because the transition test has a blind spot that measurement on a
	// real library exposed: it cannot tell a one-pixel step between two *adjacent*
	// palette entries from a one-pixel antialiasing blend. Shaded pixel art is full of
	// the former — a skin ramp steps by 15-40 per channel, well under hardEdgeThreshold
	// — so every shaded sprite scored as "soft" and was smoothly resized.
	//
	// Measured on the real library (see TestPixelArtDetection's shaded cases):
	//
	//	card-template-icon.png    5 colours   softRatio 0.676   unmistakably pixel art
	//	ui_top_background.png    15 colours   softRatio 0.459   unmistakably pixel art
	//	ui-right.aseprite        19 colours   softRatio 0.447   unmistakably pixel art
	//	npc12.png (sprite sheet) 28 colours   softRatio 0.643   unmistakably pixel art
	//	Win_loose.png            41 colours   softRatio 0.534   unmistakably pixel art
	//	TileSet_V2.png           65 colours   softRatio 0.597   unmistakably pixel art
	//	antialiased vector shape 120 colours  softRatio 0.542   genuinely smooth
	//
	// The threshold sits in the gap between the two populations. It is deliberately
	// nearer the sprites than the vector shape, because the two errors do not cost the
	// same: treating smooth art as pixel art gives a slightly aliased thumbnail, while
	// treating pixel art as smooth destroys the artwork — which §6 calls "the single
	// most annoying failure of every existing tool".
	pixelArtCertainColors = 96

	// Of the transitions in the image, fewer than this fraction may be gradual.
	// Measured: a flat-palette sprite scores about 0.0, an antialiased vector shape
	// about 0.54, and a photograph close to 1.0.
	pixelArtMaxSoftRatio = 0.40

	// A pair of neighbouring pixels differing by more than this on any channel is a
	// hard edge; differing by less (but not zero) is a gradient or antialiasing.
	hardEdgeThreshold = 48

	// Below this many differing pairs the ratio is noise, so colour count decides
	// alone. A 16x16 icon has too few transitions to judge — and it is also already
	// smaller than any thumbnail, so Fit returns it untouched and the answer does not
	// matter.
	minTransitions = 32
)

// Analyse inspects a decoded image.
//
// One pass for colours and alpha, one for edges. Both sample rather than read every
// pixel on large images: a 4096x4096 texture is 16 million pixels, and the answers
// this produces do not change with exhaustive counting.
func Analyse(img image.Image) Analysis {
	bounds := img.Bounds()
	a := Analysis{Width: bounds.Dx(), Height: bounds.Dy()}
	if a.Width <= 0 || a.Height <= 0 {
		return a
	}

	// Sampling stride. Colour counts stay exact for anything up to ~1 megapixel,
	// which covers every sprite and most textures.
	step := 1
	for (a.Width/step)*(a.Height/step) > 1<<20 {
		step++
	}

	colors := make(map[uint32]struct{}, 512)
	capped := false

	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			r, g, b, alpha := img.At(x, y).RGBA()

			switch {
			case alpha == 0:
				// Fully transparent: excluded from the palette entirely (§8).
				a.HasAlpha = true
				continue
			case alpha < 0xffff:
				a.HasAlpha = true
				a.HasSemitransparent = true
			}

			if !capped {
				// 8 bits per channel is the resolution artists actually work at, and
				// it keeps the key in one uint32.
				key := (r>>8)<<16 | (g>>8)<<8 | (b >> 8)
				colors[key] = struct{}{}
				if len(colors) >= ColorCountCap {
					capped = true
				}
			}
		}
	}

	a.ColorCount = len(colors)
	a.ColorCountExact = !capped

	a.SoftTransitionRatio, a.TransitionSamples = measureTransitions(img, step)

	switch {
	case a.ColorCount > pixelArtMaxColors:
		// A hand-picked palette this large is a photograph or a render.
		a.IsPixelArt = false

	case a.ColorCount <= pixelArtCertainColors:
		// Colour count alone is conclusive down here, and the transition test must not
		// get a veto: it cannot distinguish a one-pixel step between adjacent palette
		// entries from a one-pixel antialiasing blend, so it rejects shaded pixel art.
		// See the pixelArtCertainColors comment for the measurements.
		a.IsPixelArt = true

	case a.TransitionSamples < minTransitions:
		// Too few transitions to measure, so the count decides on its own. An image
		// this uniform is either tiny (returned untouched by Fit) or flat enough that
		// both resize filters agree.
		a.IsPixelArt = true

	default:
		// The band where the two populations overlap: enough colours that a smooth
		// gradient is plausible, so edge sharpness breaks the tie.
		a.IsPixelArt = a.SoftTransitionRatio < pixelArtMaxSoftRatio
	}
	return a
}

// measureTransitions looks at where neighbouring pixels differ, and asks how sharply.
//
// Pixel art jumps straight from one palette entry to another; antialiased and
// photographic content steps through intermediates. Flat runs are ignored entirely
// because both kinds of image are mostly flat — counting them would swamp the signal,
// which is what an earlier version of this got wrong.
func measureTransitions(img image.Image, step int) (softRatio float64, samples int) {
	bounds := img.Bounds()

	var hard, soft int

	compare := func(x1, y1, x2, y2 int) {
		r1, g1, b1, a1 := img.At(x1, y1).RGBA()
		r2, g2, b2, a2 := img.At(x2, y2).RGBA()
		// Transparent pixels have no meaningful colour, so a sprite's outline against
		// its transparent background must not count as an edge either way.
		if a1 == 0 || a2 == 0 {
			return
		}
		d := max(
			absDiff(r1>>8, r2>>8),
			absDiff(g1>>8, g2>>8),
			absDiff(b1>>8, b2>>8),
		)
		switch {
		case d == 0:
			// Flat: deliberately not counted. See the doc comment.
		case d > hardEdgeThreshold:
			hard++
		default:
			soft++
		}
	}

	for y := bounds.Min.Y; y < bounds.Max.Y; y += step {
		for x := bounds.Min.X; x < bounds.Max.X; x += step {
			if x+step < bounds.Max.X {
				compare(x, y, x+step, y)
			}
			if y+step < bounds.Max.Y {
				compare(x, y, x, y+step)
			}
		}
	}

	transitions := hard + soft
	if transitions == 0 {
		return 0, 0
	}
	return float64(soft) / float64(transitions), transitions
}

func absDiff(a, b uint32) uint32 {
	if a > b {
		return a - b
	}
	return b - a
}

// MidGrey is the background thumbnails are composited over.
//
// §6: "Composite thumbnails over mid-grey so alpha-heavy sprites are visible in a dark
// UI, with the transparent version also available." A white sprite on a dark theme and
// a black sprite on a light one are both invisible without this.
var MidGrey = color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0xff}

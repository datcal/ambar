// Package palette extracts the colour palette of a 2D image (spec §8 "Palette
// panel").
//
// The one thing this package must get right is the distinction the spec is
// emphatic about: "Extraction must be exact, not approximate." A 32×32 sprite has
// a hand-picked set of colours, and the user needs those exact hex values to match
// them — not a k-means approximation. So an image with few enough distinct colours
// is enumerated exactly (Kind "exact"); only a photographic texture with too many
// colours to list is reduced by median-cut (Kind "quantized"), and the UI labels
// that as approximate.
//
// Alpha handling follows §8: fully transparent pixels are excluded entirely (or the
// palette is dominated by transparent black), and semi-transparent pixels are
// counted separately because "in pixel art these are usually an authoring mistake
// and worth surfacing".
package palette

import (
	"fmt"
	"image"
	"sort"
)

// Swatch is one colour in a palette. The JSON tags are the on-disk shape stored in
// assets.palette_json (§4).
type Swatch struct {
	Hex   string  `json:"hex"`   // "#rrggbb"
	R     int     `json:"r"`     // 0–255
	G     int     `json:"g"`     // 0–255
	B     int     `json:"b"`     // 0–255
	Count int     `json:"count"` // visible pixels of this colour
	Ratio float64 `json:"ratio"` // Count / visible pixels, 0–1
}

// Kind labels how the palette was derived (§8).
const (
	// KindExact means every distinct visible colour is listed, and each hex is a
	// promise rather than an approximation.
	KindExact = "exact"
	// KindQuantized means the swatches are a median-cut summary of a photographic
	// image, and the UI must say so.
	KindQuantized = "quantized"
)

// Palette is the result of analysing one image.
type Palette struct {
	Kind     string   // KindExact or KindQuantized
	Swatches []Swatch // most-used first
	// Visible is the number of pixels that were not fully transparent — the
	// denominator behind every ratio, and what the panel's percentages are of.
	Visible int
	// HasSemitransparent is true when any visible pixel had partial alpha (§8).
	HasSemitransparent bool
}

// Options tunes extraction. The zero value uses the spec defaults.
type Options struct {
	// MaxExactColors is the boundary between the two paths (§8: "below a threshold,
	// around 256"). At or below this many distinct visible colours the palette is
	// exact; above it, quantized. 256 matches an indexed image's maximum, so every
	// indexed PNG enumerates exactly.
	MaxExactColors int
	// QuantizeN is the number of swatches produced on the quantized path (§8:
	// "top N with N configurable, default 16").
	QuantizeN int
	// SampleBudget bounds the pixels the median-cut path reads. Large enough that
	// the summary is stable, small enough not to sort sixteen million pixels.
	SampleBudget int
}

// Defaults, exported so callers and tests can reference them.
const (
	DefaultMaxExactColors = 256
	DefaultQuantizeN      = 16
	DefaultSampleBudget   = 1 << 16 // 65536 pixels
)

func (o Options) withDefaults() Options {
	if o.MaxExactColors <= 0 {
		o.MaxExactColors = DefaultMaxExactColors
	}
	if o.QuantizeN <= 0 {
		o.QuantizeN = DefaultQuantizeN
	}
	if o.SampleBudget <= 0 {
		o.SampleBudget = DefaultSampleBudget
	}
	return o
}

// rgb8 packs an 8-bit colour into a comparable key.
type rgb8 uint32

func packRGB(r, g, b uint8) rgb8 { return rgb8(uint32(r)<<16 | uint32(g)<<8 | uint32(b)) }
func (k rgb8) unpack() (r, g, b uint8) {
	return uint8(k >> 16), uint8(k >> 8), uint8(k)
}

// Extract analyses one decoded image and returns its palette.
//
// It always makes one full pass to count visible pixels and distinct colours,
// bailing out of the exact map once the count exceeds the threshold. If the image
// stayed within the threshold the counts are the palette; otherwise a second,
// sampled pass feeds median-cut. The double read only happens for photographic
// images, where exactness is meaningless anyway.
func Extract(img image.Image, opts Options) Palette {
	opts = opts.withDefaults()

	counts, visible, semi, exceeded := countColors(img, opts.MaxExactColors)

	p := Palette{Visible: visible, HasSemitransparent: semi}
	if visible == 0 {
		// A fully transparent image has no visible colours. An empty exact palette
		// is the honest answer, not a swatch of transparent black (§8).
		p.Kind = KindExact
		return p
	}

	if !exceeded {
		p.Kind = KindExact
		p.Swatches = exactSwatches(counts, visible)
		return p
	}

	p.Kind = KindQuantized
	p.Swatches = quantize(img, opts.QuantizeN, opts.SampleBudget, visible)
	return p
}

// countColors makes one pass, excluding fully transparent pixels (§8). It stops
// growing the map once distinct colours exceed maxExact — signalling the caller to
// take the quantized path — but keeps counting visible and semi-transparent pixels
// so the quantized path still has an accurate visible total for its ratios.
func countColors(img image.Image, maxExact int) (counts map[rgb8]int, visible int, semi, exceeded bool) {
	counts = make(map[rgb8]int, 256)
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			r, g, bl, a := img.At(x, y).RGBA()
			if a == 0 {
				continue // fully transparent: excluded entirely
			}
			visible++
			if a < 0xffff {
				semi = true
			}
			if exceeded {
				continue
			}
			key := packRGB(uint8(r>>8), uint8(g>>8), uint8(bl>>8))
			counts[key]++
			if len(counts) > maxExact {
				exceeded = true
			}
		}
	}
	return counts, visible, semi, exceeded
}

// exactSwatches turns the count map into swatches, most-used first. Ties break on
// the colour key so the ordering is deterministic — a rescan of identical bytes
// must produce byte-identical palette_json, or every derive would look like a
// change.
func exactSwatches(counts map[rgb8]int, visible int) []Swatch {
	swatches := make([]Swatch, 0, len(counts))
	for key, n := range counts {
		r, g, b := key.unpack()
		swatches = append(swatches, swatch(r, g, b, n, visible))
	}
	sortSwatches(swatches)
	return swatches
}

// sortSwatches orders by descending count, then by colour for a stable result.
func sortSwatches(s []Swatch) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].Count != s[j].Count {
			return s[i].Count > s[j].Count
		}
		ki := packRGB(uint8(s[i].R), uint8(s[i].G), uint8(s[i].B))
		kj := packRGB(uint8(s[j].R), uint8(s[j].G), uint8(s[j].B))
		return ki < kj
	})
}

func swatch(r, g, b uint8, count, visible int) Swatch {
	ratio := 0.0
	if visible > 0 {
		ratio = float64(count) / float64(visible)
	}
	return Swatch{
		Hex: fmt.Sprintf("#%02x%02x%02x", r, g, b),
		R:   int(r), G: int(g), B: int(b),
		Count: count,
		Ratio: ratio,
	}
}

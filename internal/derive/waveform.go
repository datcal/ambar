package derive

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"os"
	"path/filepath"

	"github.com/HugoSmits86/nativewebp"
	"github.com/datcal/ambar/internal/audio"
)

// A grid tile for a sound (M15).
//
// The grid showed audio as a bare extension chip, which makes a list of two hundred
// `.wav` files indistinguishable — you cannot tell an explosion from a footstep, a
// one-shot from a two-minute loop, or a normalised file from a quiet one. §8 already
// draws a canvas waveform on the detail page from the stored peaks; this draws the
// same shape as a static image so the *grid* can show it too.
//
// It reuses the peaks the audio analysis already computed, so it costs no extra
// decode: the same pass that writes peaks.json writes this.

// waveformThumb is the tile size. Wider than tall, because that is the shape of a
// waveform and of the grid cell it sits in.
const (
	waveformW = 512
	waveformH = 256
)

// writeWaveformThumb renders peaks as a waveform tile and writes it as WebP.
func writeWaveformThumb(path string, peaks audio.Peaks) error {
	img := image.NewRGBA(image.Rect(0, 0, waveformW, waveformH))

	// The same mid-grey the image thumbnails composite onto (§6), so a grid of mixed
	// kinds does not flicker between backgrounds.
	background := color.RGBA{R: 0x2a, G: 0x2e, B: 0x36, A: 0xff}
	draw.Draw(img, img.Bounds(), &image.Uniform{background}, image.Point{}, draw.Src)

	// The centre line, so a silent file still reads as audio rather than as an empty
	// tile.
	centre := waveformH / 2
	line := color.RGBA{R: 0x4a, G: 0x51, B: 0x5e, A: 0xff}
	for x := 0; x < waveformW; x++ {
		img.SetRGBA(x, centre, line)
	}

	buckets := len(peaks.Max)
	if buckets == 0 || len(peaks.Min) != buckets {
		return encodeWebP(path, img)
	}

	// The accent blue, matching the detail page's canvas waveform.
	wave := color.RGBA{R: 0x6e, G: 0xa8, B: 0xfe, A: 0xff}
	for x := 0; x < waveformW; x++ {
		// Map the column onto the peak buckets. There are normally more buckets than
		// columns, so take the extreme of the range rather than a single sample: a
		// transient must not disappear because it fell between two columns.
		lo := x * buckets / waveformW
		hi := (x + 1) * buckets / waveformW
		if hi <= lo {
			hi = lo + 1
		}
		if hi > buckets {
			hi = buckets
		}

		var minV, maxV float64
		for i := lo; i < hi; i++ {
			if float64(peaks.Min[i]) < minV {
				minV = float64(peaks.Min[i])
			}
			if float64(peaks.Max[i]) > maxV {
				maxV = float64(peaks.Max[i])
			}
		}

		top := centre - int(maxV*float64(centre-2))
		bottom := centre - int(minV*float64(centre-2))
		if top > bottom {
			top, bottom = bottom, top
		}
		// Always at least one pixel tall, so quiet passages stay visible.
		if bottom-top < 1 {
			top, bottom = centre-1, centre+1
		}
		for y := top; y <= bottom; y++ {
			if y >= 0 && y < waveformH {
				img.SetRGBA(x, y, wave)
			}
		}
	}

	return encodeWebP(path, img)
}

// encodeWebP writes an image as lossless WebP, the same encoder the image thumbnails
// use (§6: pure Go, no CGO).
func encodeWebP(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create %s: %w", filepath.Base(path), err)
	}
	if err := nativewebp.Encode(f, img, nil); err != nil {
		f.Close() //nolint:errcheck
		return fmt.Errorf("encode %s: %w", filepath.Base(path), err)
	}
	return f.Close()
}

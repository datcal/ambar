package derive

import (
	"bytes"
	"image"
	"os"
	"path/filepath"
	"testing"

	"github.com/datcal/ambar/internal/audio"
)

// TestWriteWaveformThumb: audio used to show in the grid as a bare extension chip, so
// two hundred .wav files were indistinguishable (M15). The tile is the same shape §8
// draws on the detail page, rendered from the peaks the analysis already produced.
func TestWriteWaveformThumb(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "thumb.webp")

	// A quiet start and a loud middle, so the drawing has something to say.
	peaks := audio.Peaks{Version: 1, Count: 64}
	for i := 0; i < 64; i++ {
		amp := float32(0.05)
		if i > 20 && i < 40 {
			amp = 0.9
		}
		peaks.Min = append(peaks.Min, -amp)
		peaks.Max = append(peaks.Max, amp)
	}

	if err := writeWaveformThumb(path, peaks); err != nil {
		t.Fatalf("write: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw[:16], []byte("WEBP")) {
		t.Fatalf("not a WebP: % x", raw[:16])
	}

	// The loud section must actually be taller than the quiet one, or the tile says
	// nothing about the sound.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		t.Skipf("no WebP decoder registered in this build: %v", err)
	}
	height := func(x int) int {
		n := 0
		b := img.Bounds()
		for y := b.Min.Y; y < b.Max.Y; y++ {
			r, g, bl, _ := img.At(x, y).RGBA()
			// The wave colour is the accent blue; the background is grey.
			if bl>>8 > 0xc0 && r>>8 < 0x90 && g>>8 > 0x80 {
				n++
			}
		}
		return n
	}
	quiet, loud := height(20), height(img.Bounds().Dx()/2)
	if loud <= quiet {
		t.Errorf("loud section is %d px tall, quiet is %d — the waveform is not being drawn", loud, quiet)
	}
}

// TestWriteWaveformThumbHandlesSilence: an empty or malformed peak set still produces a
// tile, because a missing image in the grid is worse than a flat line.
func TestWriteWaveformThumbHandlesSilence(t *testing.T) {
	dir := t.TempDir()
	for name, peaks := range map[string]audio.Peaks{
		"empty":      {},
		"mismatched": {Min: []float32{0.1}, Max: []float32{0.1, 0.2}},
	} {
		path := filepath.Join(dir, name+".webp")
		if err := writeWaveformThumb(path, peaks); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if info, err := os.Stat(path); err != nil || info.Size() == 0 {
			t.Errorf("%s: no tile written (%v)", name, err)
		}
	}
}

package palette

import (
	"image"
	"sort"
)

// qpixel is one sampled colour, 8 bits per channel.
type qpixel struct{ r, g, b uint8 }

// quantize reduces a photographic image to n representative swatches by median cut.
//
// Median cut over a bounded sample: the pixels are read on a stride so a 16-megapixel
// texture is summarised from ~65k samples rather than sorted whole. The visible total
// comes from the full count pass (Extract passes it in), so the reported counts are
// scaled up from the sample to the real image — an approximation, which is exactly
// what Kind "quantized" tells the UI to say.
func quantize(img image.Image, n, sampleBudget, visible int) []Swatch {
	sample := samplePixels(img, sampleBudget)
	if len(sample) == 0 {
		return nil
	}

	boxes := medianCut(sample, n)

	swatches := make([]Swatch, 0, len(boxes))
	for _, box := range boxes {
		if len(box) == 0 {
			continue
		}
		r, g, b := averageColor(box)
		// Scale the sampled bucket size back up to the real image so the count and
		// ratio describe the whole picture, not the sample.
		ratio := float64(len(box)) / float64(len(sample))
		count := int(ratio*float64(visible) + 0.5)
		s := swatch(r, g, b, count, visible)
		s.Ratio = ratio
		swatches = append(swatches, s)
	}
	sortSwatches(swatches)
	return swatches
}

// samplePixels reads visible pixels on a stride chosen so at most budget of them
// are returned. Fully transparent pixels are excluded, consistent with the exact
// path (§8).
func samplePixels(img image.Image, budget int) []qpixel {
	b := img.Bounds()
	total := b.Dx() * b.Dy()
	if total <= 0 {
		return nil
	}
	step := 1
	for total/(step*step) > budget {
		step++
	}

	out := make([]qpixel, 0, budget)
	for y := b.Min.Y; y < b.Max.Y; y += step {
		for x := b.Min.X; x < b.Max.X; x += step {
			r, g, bl, a := img.At(x, y).RGBA()
			if a == 0 {
				continue
			}
			out = append(out, qpixel{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)})
		}
	}
	return out
}

// medianCut splits the sample into at most n boxes, repeatedly cutting the box with
// the widest colour range along its widest channel at the median. It is the classic
// algorithm, chosen over k-means because it is deterministic — no random seeding —
// so identical bytes yield an identical palette on every rescan.
func medianCut(pixels []qpixel, n int) [][]qpixel {
	if n < 1 {
		n = 1
	}
	boxes := [][]qpixel{pixels}

	for len(boxes) < n {
		// Find the box with the largest single-channel range that can still split.
		bestIdx, bestRange, bestChan := -1, -1, 0
		for i, box := range boxes {
			if len(box) < 2 {
				continue
			}
			ch, rng := widestChannel(box)
			if rng > bestRange {
				bestIdx, bestRange, bestChan = i, rng, ch
			}
		}
		if bestIdx < 0 {
			break // nothing left to split
		}

		box := boxes[bestIdx]
		sortByChannel(box, bestChan)
		mid := len(box) / 2
		left, right := box[:mid], box[mid:]
		// Replace the split box with its two halves.
		boxes[bestIdx] = left
		boxes = append(boxes, right)
	}
	return boxes
}

// widestChannel returns the channel (0=R,1=G,2=B) with the largest min-to-max
// spread in the box, and that spread.
func widestChannel(box []qpixel) (channel, spread int) {
	var minR, minG, minB uint8 = 255, 255, 255
	var maxR, maxG, maxB uint8
	for _, p := range box {
		minR, maxR = minU8(minR, p.r), maxU8(maxR, p.r)
		minG, maxG = minU8(minG, p.g), maxU8(maxG, p.g)
		minB, maxB = minU8(minB, p.b), maxU8(maxB, p.b)
	}
	rR, rG, rB := int(maxR-minR), int(maxG-minG), int(maxB-minB)
	channel, spread = 0, rR
	if rG > spread {
		channel, spread = 1, rG
	}
	if rB > spread {
		channel, spread = 2, rB
	}
	return channel, spread
}

func sortByChannel(box []qpixel, ch int) {
	sort.Slice(box, func(i, j int) bool {
		switch ch {
		case 0:
			return box[i].r < box[j].r
		case 1:
			return box[i].g < box[j].g
		default:
			return box[i].b < box[j].b
		}
	})
}

// averageColor is the box's representative colour: the mean of its members.
func averageColor(box []qpixel) (r, g, b uint8) {
	var sr, sg, sb int
	for _, p := range box {
		sr += int(p.r)
		sg += int(p.g)
		sb += int(p.b)
	}
	n := len(box)
	return uint8(sr / n), uint8(sg / n), uint8(sb / n)
}

func minU8(a, b uint8) uint8 {
	if a < b {
		return a
	}
	return b
}

func maxU8(a, b uint8) uint8 {
	if a > b {
		return a
	}
	return b
}

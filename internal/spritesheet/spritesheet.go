// Package spritesheet is the §6 grid detection: given a packed sheet, guess the
// cell grid so the frames can be stepped and animated. It is deliberately a
// *guesser* — §6 insists "never silently guess wrong", so it returns a
// confidence and the UI confirms or corrects before the guess is trusted.
//
// The scoring heuristic (recorded in docs/decisions.md) is:
//
//   - candidate column/row counts are the divisors of each dimension that yield
//     a cell no smaller than minCell and no more than maxCount cells per axis;
//   - each candidate grid is scored by how transparent its interior seam lines
//     are (a real grid has transparent gutters between frames), by how square its
//     cells are, and by every cell actually containing some opaque content;
//   - the best-scoring grid wins, and it is only "confident" when the seams are
//     clearly transparent — a tight, gutterless sheet scores low on purpose and
//     is left for the user to confirm.
package spritesheet

import (
	"image"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	minCell  = 8  // a cell smaller than this is almost certainly not a frame
	maxCount = 64 // more than this many frames per axis is implausible
	// confidentSeam is the fraction of interior seam pixels that must be fully
	// transparent for a detection to be trusted without confirmation.
	confidentSeam = 0.9
)

// Grid is a detected (or proposed) frame grid.
type Grid struct {
	Cols       int
	Rows       int
	FrameW     int
	FrameH     int
	FrameCount int
	// Score is the heuristic score in [0,1]; Confident reflects a clear grid.
	Score     float64
	Confident bool
}

var frameCountName = regexp.MustCompile(`\d+x\d+|[_-](\d{1,3})(f|fps|frames)?$`)

// IsCandidate reports whether an image is worth running detection on (§6): its
// dimensions suggest a grid, or its filename hints at a sheet. Sidecar-described
// sheets are handled by the caller before this.
func IsCandidate(filename string, w, h int) bool {
	name := strings.ToLower(strings.TrimSuffix(filepath.Base(filename), filepath.Ext(filename)))
	if strings.Contains(name, "sheet") || strings.Contains(name, "atlas") ||
		strings.Contains(name, "anim") || strings.Contains(name, "sprite") {
		return true
	}
	if frameCountName.MatchString(name) {
		return true
	}
	// Dimensions that divide into several plausible square-ish cells.
	return len(candidateCounts(w)) > 0 && len(candidateCounts(h)) > 0 &&
		(w >= 2*minCell || h >= 2*minCell)
}

// candidateCounts returns the per-axis frame counts worth trying for a dimension:
// exact divisors giving a cell of at least minCell, up to maxCount frames.
func candidateCounts(dim int) []int {
	var out []int
	for n := 2; n <= maxCount; n++ {
		if dim%n == 0 && dim/n >= minCell {
			out = append(out, n)
		}
	}
	return out
}

// Detect proposes the best frame grid for an image, and whether it is confident.
// The second return is false when the image yields no plausible grid at all.
func Detect(img image.Image) (Grid, bool) {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w < 2*minCell && h < 2*minCell {
		return Grid{}, false
	}

	colOpts := append([]int{1}, candidateCounts(w)...)
	rowOpts := append([]int{1}, candidateCounts(h)...)

	alpha := alphaGrid(img) // 0..255 per pixel, for fast seam/content tests

	var best Grid
	found := false
	for _, cols := range colOpts {
		for _, rows := range rowOpts {
			if cols*rows < 2 {
				continue // a 1x1 "grid" is not a sheet
			}
			cellW, cellH := w/cols, h/rows
			if cellW < minCell || cellH < minCell {
				continue
			}
			g := Grid{Cols: cols, Rows: rows, FrameW: cellW, FrameH: cellH, FrameCount: cols * rows}
			seam := seamTransparency(alpha, w, h, cols, rows, cellW, cellH)
			content := cellContentRatio(alpha, w, cols, rows, cellW, cellH)
			aspect := squareness(cellW, cellH)
			g.Score = 0.6*seam + 0.25*aspect + 0.15*content
			g.Confident = seam >= confidentSeam && content >= 0.75
			// Content is a hard gate: a grid with empty cells is not the real one.
			if content < 0.5 {
				continue
			}
			// On a tie, prefer the finer grid: when a coarser grid scores as well, it
			// is only because its seams happen to fall on the finer grid's transparent
			// gutters, so the finer one is the real frame size.
			const eps = 0.01
			switch {
			case !found, g.Score > best.Score+eps:
				best, found = g, true
			case g.Score >= best.Score-eps && g.FrameCount > best.FrameCount:
				best = g
			}
		}
	}
	return best, found
}

// alphaGrid extracts the alpha channel into a flat slice for cheap repeated reads.
func alphaGrid(img image.Image) []uint8 {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	a := make([]uint8, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			_, _, _, av := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			a[y*w+x] = uint8(av >> 8)
		}
	}
	return a
}

// seamTransparency is the fraction of interior seam-line pixels that are fully
// transparent — the primary signal that a candidate grid matches real gutters.
func seamTransparency(a []uint8, w, h, cols, rows, cellW, cellH int) float64 {
	transparent, total := 0, 0
	for c := 1; c < cols; c++ {
		x := c * cellW
		for y := 0; y < h; y++ {
			total++
			if a[y*w+x] == 0 {
				transparent++
			}
		}
	}
	for r := 1; r < rows; r++ {
		y := r * cellH
		for x := 0; x < w; x++ {
			total++
			if a[y*w+x] == 0 {
				transparent++
			}
		}
	}
	if total == 0 {
		return 0 // a single row and column: no interior seams to judge
	}
	return float64(transparent) / float64(total)
}

// cellContentRatio is the fraction of cells that contain at least one opaque
// pixel — a grid whose cells are mostly empty is the wrong grid.
func cellContentRatio(a []uint8, w, cols, rows, cellW, cellH int) float64 {
	withContent := 0
	for r := 0; r < rows; r++ {
		for c := 0; c < cols; c++ {
			if cellHasContent(a, w, c*cellW, r*cellH, cellW, cellH) {
				withContent++
			}
		}
	}
	return float64(withContent) / float64(cols*rows)
}

func cellHasContent(a []uint8, w, x0, y0, cw, ch int) bool {
	for y := y0; y < y0+ch; y++ {
		for x := x0; x < x0+cw; x++ {
			if a[y*w+x] > 8 {
				return true
			}
		}
	}
	return false
}

// squareness is 1 for a square cell, decreasing as it gets more oblong.
func squareness(cellW, cellH int) float64 {
	if cellW == 0 || cellH == 0 {
		return 0
	}
	lo, hi := cellW, cellH
	if lo > hi {
		lo, hi = hi, lo
	}
	return float64(lo) / float64(hi)
}

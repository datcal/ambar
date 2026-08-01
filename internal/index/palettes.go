package index

import (
	"context"
	"fmt"
	"math"
	"sort"
)

// §7 asks for "a pack-level palette consistency view answering 'does this tileset
// sit next to that character set'". That is a different question from the per-asset
// palette panel (§8) and from `color:` search: it compares two whole packs, because
// the incoherence a mixed-source library suffers from is between packs, not within
// one file.
//
// The comparison is deliberately simple to explain, because a score nobody
// understands is a score nobody trusts: bucket each pack's swatches into a coarse
// colour grid weighted by how much of the pack they cover, then ask how much of each
// pack's weight has a near-enough colour in the other. The answer is two coverage
// percentages and the colours behind them, not one opaque number.

// paletteBucket is the width of a colour bucket per channel when aggregating a
// pack's palette. 24 of 255 groups the shades an artist would call "the same
// brown" without merging brown into red.
const paletteBucket = 24

// minSwatchRatio ignores a colour that covers almost none of an image. A swatch at 0.5% of one
// sprite is an antialiasing artefact or a stray pixel, and aggregating thousands of those
// produces a muddy average rather than a library's palette.
const minSwatchRatio = 0.02

// PackColour is one aggregated colour of a pack's palette.
type PackColour struct {
	R, G, B int
	// Weight is the summed swatch ratio across the pack's assets — roughly "how much
	// of this pack is this colour". Normalised so a pack's weights sum to 1.
	Weight float64
	// Assets is how many of the pack's assets contain a colour in this bucket, which
	// is what distinguishes a pack-wide palette from one loud outlier file.
	Assets int
}

// Hex is the colour as #rrggbb.
func (c PackColour) Hex() string { return fmt.Sprintf("#%02x%02x%02x", c.R, c.G, c.B) }

// HexQuery is the hex without the '#', for a `color:` query in a URL.
func (c PackColour) HexQuery() string { return fmt.Sprintf("%02x%02x%02x", c.R, c.G, c.B) }

// Percent is Weight as a rounded percentage, for display.
func (c PackColour) Percent() string { return fmt.Sprintf("%.0f", c.Weight*100) }

// PackPalette, the pack-palette comparison and their queries lived here until M16, when
// /palettes was removed — a page nobody opened, and §7's question ("does this tileset sit next
// to that character set") is answered in practice by the sidebar's colour filter and by
// `color:` search, both of which people do use.
//
// What remains is LibraryColours and its row type PackColour: the library's own dominant
// colours, which is what that filter is built from.

// LibraryColours picks the sidebar's colour filter: a row of swatches that spans the
// library rather than the top of one heap (M15, reworked in M17).
//
// The first version returned the top N buckets by coverage, which is the obvious ranking
// and the wrong one. Pixel art is mostly outlines and shadows, so coverage ranks
// near-blacks and dark browns first: on this library the leader was #0d0d13, present in
// **38% of all images**. Nine of the top twelve were the same dark cluster. As a *filter*
// that is useless twice over — the row cannot offer green, and clicking the colour it does
// offer returns a third of the library.
//
// So the candidates are still ranked by coverage, but the selection round-robins across
// hue families and the result is ordered by hue. Every family present gets its strongest
// colour before any family gets a second, which is what makes a green appear at all: the
// library has 24 green buckets and they were being outweighed by shadow.
func (ix *Indexer) LibraryColours(ctx context.Context, topColours int) ([]PackColour, error) {
	if topColours <= 0 {
		topColours = 18
	}
	// Candidates, not the answer: enough of the ranking to choose from, still bounded.
	// The GROUP BY already runs over every swatch, so a larger LIMIT costs the scan
	// nothing extra — it only decides how many rows come back.
	candidates := topColours * 10
	if candidates < 200 {
		candidates = 200
	}
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT cast(round(sum(s.r * s.ratio) / sum(s.ratio)) AS INTEGER),
		       cast(round(sum(s.g * s.ratio) / sum(s.ratio)) AS INTEGER),
		       cast(round(sum(s.b * s.ratio) / sum(s.ratio)) AS INTEGER),
		       sum(s.ratio),
		       count(DISTINCT s.asset_id)
		FROM asset_swatches s
		JOIN assets a ON a.id = s.asset_id
		WHERE a.missing_since IS NULL AND s.ratio >= ?
		GROUP BY s.r / ?, s.g / ?, s.b / ?
		ORDER BY sum(s.ratio) DESC
		LIMIT ?`,
		minSwatchRatio, paletteBucket, paletteBucket, paletteBucket, candidates)
	if err != nil {
		return nil, fmt.Errorf("aggregate library palette: %w", err)
	}
	defer rows.Close()

	var all []PackColour
	for rows.Next() {
		var c PackColour
		if err := rows.Scan(&c.R, &c.G, &c.B, &c.Weight, &c.Assets); err != nil {
			return nil, fmt.Errorf("scan library colour: %w", err)
		}
		all = append(all, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate library palette: %w", err)
	}

	out := spreadAcrossHues(all, topColours)

	var total float64
	for _, c := range out {
		total += c.Weight
	}
	if total > 0 {
		for i := range out {
			out[i].Weight /= total
		}
	}
	return out, nil
}

// hueFamilies is how many slices the colour wheel is cut into. Twelve is the smallest
// number where red, orange, yellow, green, cyan, blue, purple and pink are all separate
// things to the eye, which is the only test that matters for a row of swatches.
const hueFamilies = 12

// neutralFamily is where greys, near-blacks and near-whites go. They are a family in
// their own right — "the dark ones" is a real thing to filter by — but only one, so
// twenty shades of shadow cannot crowd out every hue.
const neutralFamily = hueFamilies

// spreadAcrossHues selects n colours from candidates (which arrive strongest first) by
// taking the best of every family, then the second best of every family, and so on. The
// result is sorted by hue so the strip reads as a spectrum rather than a ranking.
func spreadAcrossHues(candidates []PackColour, n int) []PackColour {
	if len(candidates) <= n {
		out := append([]PackColour(nil), candidates...)
		sortByHue(out)
		return out
	}

	families := make([][]PackColour, hueFamilies+1)
	for _, c := range candidates {
		families[familyOf(c)] = append(families[familyOf(c)], c)
	}

	out := make([]PackColour, 0, n)
	for round := 0; len(out) < n; round++ {
		progressed := false
		for _, family := range families {
			if round >= len(family) {
				continue
			}
			progressed = true
			out = append(out, family[round])
			if len(out) == n {
				break
			}
		}
		if !progressed {
			break // every family exhausted
		}
	}
	sortByHue(out)
	return out
}

// familyOf assigns a colour to a hue slice, or to the neutrals.
func familyOf(c PackColour) int {
	h, s, l := hsl(c.R, c.G, c.B)
	// Thresholds by eye against the real library: below 15% saturation a swatch reads as
	// grey whatever its hue says, and at the extremes of lightness the hue is noise —
	// #0d0d13 is "black", not "a dark blue".
	if s < 0.15 || l < 0.12 || l > 0.93 {
		return neutralFamily
	}
	family := int(h * hueFamilies)
	if family >= hueFamilies {
		family = hueFamilies - 1
	}
	return family
}

// sortByHue orders a palette the way a palette is drawn: around the wheel, then by
// lightness within a hue, with the neutrals gathered at the end darkest-first.
func sortByHue(colours []PackColour) {
	sort.SliceStable(colours, func(i, j int) bool {
		fi, fj := familyOf(colours[i]), familyOf(colours[j])
		if fi != fj {
			return fi < fj
		}
		hi, _, li := hsl(colours[i].R, colours[i].G, colours[i].B)
		hj, _, lj := hsl(colours[j].R, colours[j].G, colours[j].B)
		if fi == neutralFamily {
			return li < lj
		}
		if hi != hj {
			return hi < hj
		}
		return li < lj
	})
}

// hsl converts 8-bit RGB to hue (0..1), saturation and lightness. Standard conversion,
// written out because the alternative is a dependency for eleven lines.
func hsl(r8, g8, b8 int) (h, s, l float64) {
	r, g, b := float64(r8)/255, float64(g8)/255, float64(b8)/255
	max, min := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (max + min) / 2
	if max == min {
		return 0, 0, l // grey: hue is undefined, and saying 0 keeps it in the neutrals
	}
	d := max - min
	if l > 0.5 {
		s = d / (2 - max - min)
	} else {
		s = d / (max + min)
	}
	switch max {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, s, l
}

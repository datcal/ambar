package index

import (
	"context"
	"fmt"
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

// LibraryColours aggregates the whole library's palette into the sidebar's colour filter (M15).
//
// Same bucketing as a pack's palette, without the pack filter: §7 calls colour the
// thing that decides whether a mixed-source library looks coherent, and a row of the
// library's own dominant colours turns `color:` search from a syntax you have to
// remember into something you can click.
func (ix *Indexer) LibraryColours(ctx context.Context, topColours int) ([]PackColour, error) {
	if topColours <= 0 {
		topColours = 18
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
		minSwatchRatio, paletteBucket, paletteBucket, paletteBucket, topColours)
	if err != nil {
		return nil, fmt.Errorf("aggregate library palette: %w", err)
	}
	defer rows.Close()

	var out []PackColour
	var total float64
	for rows.Next() {
		var c PackColour
		if err := rows.Scan(&c.R, &c.G, &c.B, &c.Weight, &c.Assets); err != nil {
			return nil, fmt.Errorf("scan library colour: %w", err)
		}
		out = append(out, c)
		total += c.Weight
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate library palette: %w", err)
	}
	if total > 0 {
		for i := range out {
			out[i].Weight /= total
		}
	}
	return out, nil
}

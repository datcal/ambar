package index

import (
	"context"
	"database/sql"
	"errors"
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

// PackPalette is a pack's aggregated palette.
type PackPalette struct {
	PackID   int64
	PackName string
	PackPath string
	// Colours are the dominant buckets, most-covering first.
	Colours []PackColour
	// AnalysedAssets is how many of the pack's assets have palette data at all. A
	// pack with two analysed assets out of four hundred is not a palette; the view
	// says so rather than pretending.
	AnalysedAssets int
	TotalAssets    int
}

// HasPalette reports whether there is enough data to compare.
func (p PackPalette) HasPalette() bool { return len(p.Colours) > 0 }

// Coverage is the share of the pack's assets that have been analysed.
func (p PackPalette) Coverage() string {
	if p.TotalAssets == 0 {
		return "0"
	}
	return fmt.Sprintf("%.0f", float64(p.AnalysedAssets)/float64(p.TotalAssets)*100)
}

// PackPalettes aggregates every pack's palette, ordered by name. Packs with no
// palette data are included with no colours, because "nothing has been derived here
// yet" is the answer the user needs rather than a missing row.
func (ix *Indexer) PackPalettes(ctx context.Context, topColours int) ([]PackPalette, error) {
	if topColours <= 0 {
		topColours = 10
	}

	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT p.id, p.name, p.library_rel_path,
		       count(*) FILTER (WHERE a.missing_since IS NULL) AS total,
		       count(*) FILTER (WHERE a.missing_since IS NULL AND a.palette_json IS NOT NULL) AS analysed
		FROM packs p
		LEFT JOIN assets a ON a.pack_id = p.id
		GROUP BY p.id
		ORDER BY p.name`)
	if err != nil {
		return nil, fmt.Errorf("list packs for palettes: %w", err)
	}
	defer rows.Close()

	var out []PackPalette
	for rows.Next() {
		var p PackPalette
		if err := rows.Scan(&p.PackID, &p.PackName, &p.PackPath, &p.TotalAssets, &p.AnalysedAssets); err != nil {
			return nil, fmt.Errorf("scan pack palette row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list packs for palettes: %w", err)
	}

	for i := range out {
		if out[i].AnalysedAssets == 0 {
			continue
		}
		colours, err := ix.packColours(ctx, out[i].PackID, topColours)
		if err != nil {
			return nil, err
		}
		out[i].Colours = colours
	}
	return out, nil
}

// ErrPackNotFound means the id names no pack. It exists so a pack id arriving from a
// URL produces a message rather than a 500.
var ErrPackNotFound = errors.New("pack not found")

// PackPaletteOf aggregates one pack's palette.
func (ix *Indexer) PackPaletteOf(ctx context.Context, packID int64, topColours int) (PackPalette, error) {
	var p PackPalette
	err := ix.db.Reader.QueryRowContext(ctx, `
		SELECT p.id, p.name, p.library_rel_path,
		       (SELECT count(*) FROM assets a WHERE a.pack_id = p.id AND a.missing_since IS NULL),
		       (SELECT count(*) FROM assets a WHERE a.pack_id = p.id AND a.missing_since IS NULL
		                                       AND a.palette_json IS NOT NULL)
		FROM packs p WHERE p.id = ?`, packID).
		Scan(&p.PackID, &p.PackName, &p.PackPath, &p.TotalAssets, &p.AnalysedAssets)
	if errors.Is(err, sql.ErrNoRows) {
		return p, fmt.Errorf("%w: %d", ErrPackNotFound, packID)
	}
	if err != nil {
		return p, fmt.Errorf("load pack %d: %w", packID, err)
	}
	if p.AnalysedAssets == 0 {
		return p, nil
	}
	colours, err := ix.packColours(ctx, packID, topColours)
	if err != nil {
		return p, err
	}
	p.Colours = colours
	return p, nil
}

// packColours buckets a pack's swatches and returns the dominant ones.
//
// The bucket is a coarse grid; the reported colour is the weighted mean *within* the
// bucket, so a strip of chips shows the pack's actual colours rather than the grid
// they landed in.
func (ix *Indexer) packColours(ctx context.Context, packID int64, topColours int) ([]PackColour, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT cast(round(sum(s.r * s.ratio) / sum(s.ratio)) AS INTEGER),
		       cast(round(sum(s.g * s.ratio) / sum(s.ratio)) AS INTEGER),
		       cast(round(sum(s.b * s.ratio) / sum(s.ratio)) AS INTEGER),
		       sum(s.ratio),
		       count(DISTINCT s.asset_id)
		FROM asset_swatches s
		JOIN assets a ON a.id = s.asset_id
		WHERE a.pack_id = ? AND a.missing_since IS NULL AND s.ratio >= ?
		GROUP BY s.r / ?, s.g / ?, s.b / ?
		ORDER BY sum(s.ratio) DESC
		LIMIT ?`,
		packID, minPackSwatchRatio, paletteBucket, paletteBucket, paletteBucket, topColours)
	if err != nil {
		return nil, fmt.Errorf("aggregate palette for pack %d: %w", packID, err)
	}
	defer rows.Close()

	var (
		colours []PackColour
		total   float64
	)
	for rows.Next() {
		var c PackColour
		if err := rows.Scan(&c.R, &c.G, &c.B, &c.Weight, &c.Assets); err != nil {
			return nil, fmt.Errorf("scan pack colour: %w", err)
		}
		colours = append(colours, c)
		total += c.Weight
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("aggregate palette for pack %d: %w", packID, err)
	}

	// Normalise against the colours actually shown, so the strip's percentages add up
	// to 100 and read as "of this pack's palette".
	if total > 0 {
		for i := range colours {
			colours[i].Weight /= total
		}
	}
	return colours, nil
}

// minPackSwatchRatio drops swatches that are a rounding error in their own asset,
// the same floor colour search uses.
const minPackSwatchRatio = 0.005

// LibraryColours aggregates the whole library's palette, for the sidebar's colour
// filter (M15).
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
		minPackSwatchRatio, paletteBucket, paletteBucket, paletteBucket, topColours)
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

// PackComparison answers §7's question for one pair of packs.
type PackComparison struct {
	A PackPalette
	B PackPalette
	// AInB is the share of A's palette weight that has a near-enough colour in B, and
	// BInA the reverse. Both are reported because they answer different questions: a
	// small character set can sit entirely inside a large tileset's palette while the
	// tileset is mostly colours the characters never use.
	AInB float64
	BInA float64
	// Shared, OnlyInA and OnlyInB are the colours behind those numbers.
	Shared  []PackColour
	OnlyInA []PackColour
	OnlyInB []PackColour
	// Tolerance is the per-channel slack used, so the page can say what it means.
	Tolerance int
}

// Verdict is the one-line answer, in words rather than a number.
//
// Deliberately hedged: this is a hint for a human deciding whether two packs will
// look right together, not a measurement. §7 wants the question answerable, not
// automated.
func (c PackComparison) Verdict() string {
	low := math.Min(c.AInB, c.BInA)
	switch {
	case !c.A.HasPalette() || !c.B.HasPalette():
		return "not enough palette data to say — run `ambar derive` for these packs"
	case low >= 0.75:
		return "these two share most of their palette; they should sit together well"
	case low >= 0.5:
		return "a substantial shared palette, with each pack keeping colours of its own"
	case low >= 0.25:
		return "partly compatible — expect to recolour or to lean on the shared colours"
	default:
		return "different palettes; putting these side by side will show"
	}
}

// SharePercent renders a coverage share.
func percent(v float64) string { return fmt.Sprintf("%.0f", v*100) }

// AInBPercent and BInAPercent are the coverages as percentages.
func (c PackComparison) AInBPercent() string { return percent(c.AInB) }

// BInAPercent is AInBPercent in the other direction.
func (c PackComparison) BInAPercent() string { return percent(c.BInA) }

// ComparePacks aggregates both packs and matches their colours.
func (ix *Indexer) ComparePacks(ctx context.Context, packA, packB int64, tolerance int) (PackComparison, error) {
	if tolerance <= 0 {
		tolerance = paletteBucket
	}
	// A wider strip than the display default: the comparison should consider a pack's
	// whole palette, not only what fits on screen.
	const compareColours = 24

	a, err := ix.PackPaletteOf(ctx, packA, compareColours)
	if err != nil {
		return PackComparison{}, err
	}
	b, err := ix.PackPaletteOf(ctx, packB, compareColours)
	if err != nil {
		return PackComparison{}, err
	}

	cmp := PackComparison{A: a, B: b, Tolerance: tolerance}
	if !a.HasPalette() || !b.HasPalette() {
		return cmp, nil
	}

	near := func(x, y PackColour) bool {
		return abs(x.R-y.R) <= tolerance && abs(x.G-y.G) <= tolerance && abs(x.B-y.B) <= tolerance
	}

	for _, ca := range a.Colours {
		matched := false
		for _, cb := range b.Colours {
			if near(ca, cb) {
				matched = true
				break
			}
		}
		if matched {
			cmp.AInB += ca.Weight
			cmp.Shared = append(cmp.Shared, ca)
		} else {
			cmp.OnlyInA = append(cmp.OnlyInA, ca)
		}
	}
	for _, cb := range b.Colours {
		matched := false
		for _, ca := range a.Colours {
			if near(cb, ca) {
				matched = true
				break
			}
		}
		if matched {
			cmp.BInA += cb.Weight
		} else {
			cmp.OnlyInB = append(cmp.OnlyInB, cb)
		}
	}

	// Heaviest first in every list, so the strips read as "what matters most".
	for _, list := range [][]PackColour{cmp.Shared, cmp.OnlyInA, cmp.OnlyInB} {
		sort.Slice(list, func(i, j int) bool { return list[i].Weight > list[j].Weight })
	}
	return cmp, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

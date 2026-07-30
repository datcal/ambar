package index

import (
	"context"
	"fmt"
	"strings"

	"github.com/datcal/ambar/internal/search"
	"github.com/datcal/ambar/internal/tags"
)

// tagResolver adapts the tag store to search.TagResolver: a tag token resolves
// to itself plus every descendant (§7 hierarchy), with aliases handled by the
// store's Resolve.
type tagResolver struct{ store *tags.Store }

func (r tagResolver) ResolveTag(ctx context.Context, token string) ([]int64, bool, error) {
	t, ok, err := r.store.Resolve(ctx, token)
	if err != nil || !ok {
		return nil, ok, err
	}
	ids, err := r.store.DescendantIDs(ctx, t.ID)
	return ids, true, err
}

// compileQuery parses and compiles a §7 query string into a SQL clause over the
// given asset table alias. A blank or all-no-op query yields an empty clause,
// which the caller simply skips.
func (ix *Indexer) compileQuery(ctx context.Context, query, alias string) (search.Compiled, error) {
	if strings.TrimSpace(query) == "" {
		return search.Compiled{}, nil
	}
	q, err := search.Parse(query)
	if err != nil {
		return search.Compiled{}, err
	}
	return search.CompileWith(ctx, q, alias, tagResolver{tags.NewStore(ix.db)}, swatchResolver{ix})
}

// swatchResolver adapts the indexed swatch table to search.SwatchResolver, which is
// what `palette-near:<asset_id>` compares against (§7).
type swatchResolver struct{ ix *Indexer }

func (r swatchResolver) SwatchesOf(ctx context.Context, assetID int64) ([]search.Swatch, bool, error) {
	rows, err := r.ix.db.Reader.QueryContext(ctx, `
		SELECT r, g, b, ratio FROM asset_swatches WHERE asset_id = ? ORDER BY rank`, assetID)
	if err != nil {
		return nil, false, fmt.Errorf("load swatches for asset %d: %w", assetID, err)
	}
	defer rows.Close()

	var out []search.Swatch
	for rows.Next() {
		var s search.Swatch
		if err := rows.Scan(&s.R, &s.G, &s.B, &s.Ratio); err != nil {
			return nil, false, fmt.Errorf("scan swatch: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("load swatches for asset %d: %w", assetID, err)
	}
	// found is false when the asset has no palette — not analysed, or fully
	// transparent. The compiler turns that into "matches nothing", which is honest:
	// nothing is near a palette that does not exist.
	return out, len(out) > 0, nil
}

// assetWhere builds the full WHERE for asset-level listing over the `a` alias:
// the sidebar facets (kind, pack, missing) plus the compiled §7 query. It is the
// shared filter behind List and Facets, so a facet count matches the grid.
func (ix *Indexer) assetWhere(ctx context.Context, opts ListOptions) ([]string, []any, error) {
	where, args, err := listFilters(opts)
	if err != nil {
		return nil, nil, err
	}
	compiled, err := ix.compileQuery(ctx, opts.Query, "a")
	if err != nil {
		return nil, nil, err
	}
	if compiled.SQL != "" {
		where = append(where, compiled.SQL)
		args = append(args, compiled.Args...)
	}
	return where, args, nil
}

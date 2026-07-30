package index

import (
	"context"
	"fmt"
	"strings"
)

// MatchingAssetIDs returns every asset id matching opts, ignoring paging — the
// target set for §7's "tag everything matching this search". Missing assets are
// included or not exactly as the query's filter says, so a bulk tag applies to
// the same rows the grid shows.
func (ix *Indexer) MatchingAssetIDs(ctx context.Context, opts ListOptions) ([]int64, error) {
	o := opts
	o.Cursor = ""
	where, args, err := ix.assetWhere(ctx, o)
	if err != nil {
		return nil, err
	}
	rows, err := ix.db.Reader.QueryContext(ctx,
		`SELECT a.id FROM assets a WHERE `+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, fmt.Errorf("matching asset ids: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// Facet is one tag present in a result set, with how many assets carry it. It is
// the drill-down unit of §7's faceted sidebar: "tags present in the current
// result set with live counts, zero-match tags hidden".
type Facet struct {
	Canonical string
	Namespace string
	Count     int
}

// DefaultFacetLimit caps how many facets the sidebar shows.
const DefaultFacetLimit = 40

// Facets returns the tags carried by the assets matching opts, most-frequent
// first, counting both a direct tag and one inherited from the asset's pack (§7).
// The filter is exactly List's, so a facet's count matches what clicking it
// narrows the grid to.
//
// Counts are over assets, not groups: an asset appears once per tag regardless
// of how many format variants its group has, which keeps a "17" next to a tag
// meaning "17 things", not "17 files that might be the same artwork".
func (ix *Indexer) Facets(ctx context.Context, opts ListOptions, limit int) ([]Facet, error) {
	if limit <= 0 || limit > 200 {
		limit = DefaultFacetLimit
	}

	// The result set is the asset-level filter, cursor and paging excluded: facets
	// describe the whole match, not the visible page.
	facetOpts := opts
	facetOpts.Cursor = ""
	where, args, err := ix.assetWhere(ctx, facetOpts)
	if err != nil {
		return nil, err
	}

	query := `
		WITH matching AS (
			SELECT a.id AS aid, a.pack_id AS pid FROM assets a WHERE ` + strings.Join(where, " AND ") + `
		),
		present AS (
			SELECT m.aid, t.namespace AS ns, t.namespace || ':' || t.name AS canon
			FROM matching m
			JOIN asset_tags at ON at.asset_id = m.aid
			JOIN tags t ON t.id = at.tag_id
			UNION
			SELECT m.aid, t.namespace, t.namespace || ':' || t.name
			FROM matching m
			JOIN pack_tags pt ON pt.pack_id = m.pid
			JOIN tags t ON t.id = pt.tag_id
		)
		SELECT canon, ns, count(*) AS n
		FROM present
		GROUP BY canon, ns
		ORDER BY n DESC, canon
		LIMIT ?`

	rows, err := ix.db.Reader.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("facets: %w", err)
	}
	defer rows.Close()

	var out []Facet
	for rows.Next() {
		var f Facet
		if err := rows.Scan(&f.Canonical, &f.Namespace, &f.Count); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

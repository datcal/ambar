package tags

import (
	"context"
	"database/sql"
	"fmt"
)

// tagTextExpr computes the assets_fts.tag_text value for the row being updated,
// correlated on assets_fts.rowid (which is the asset id). It gathers the asset's
// direct tags and the tags inherited from its pack, expands each up the
// hierarchy to its ancestors via the closure table, and emits every reachable
// tag's canonical string plus its aliases as one space-joined blob.
//
// The FTS tokenizer then splits `type:sfx:impact` into type/sfx/impact, so free
// text search finds an asset by any hierarchy segment or alias. The structured
// `namespace:name` filters of the §7 query language do not use this column; they
// query asset_tags and tag_closure directly. This column exists only for the
// free-text half of search.
//
// coalesce keeps the column an empty string rather than NULL when an asset has
// no tags, which is the common case and which FTS is happiest with.
const tagTextExpr = `coalesce((
	WITH direct(tag_id) AS (
		SELECT tag_id FROM asset_tags WHERE asset_id = assets_fts.rowid
		UNION
		SELECT pt.tag_id FROM pack_tags pt
		JOIN assets a ON a.pack_id = pt.pack_id
		WHERE a.id = assets_fts.rowid
	),
	reachable(tag_id) AS (
		SELECT DISTINCT c.ancestor_id
		FROM tag_closure c JOIN direct d ON c.descendant_id = d.tag_id
	)
	SELECT group_concat(txt, ' ') FROM (
		SELECT namespace || ':' || name AS txt FROM tags WHERE id IN (SELECT tag_id FROM reachable)
		UNION ALL
		SELECT alias FROM tag_aliases WHERE tag_id IN (SELECT tag_id FROM reachable)
	)
), '')`

// reindexAssetFTS rewrites one asset's tag_text after its tags changed.
func reindexAssetFTS(ctx context.Context, tx *sql.Tx, assetID int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE assets_fts SET tag_text = `+tagTextExpr+` WHERE rowid = ?`, assetID); err != nil {
		return fmt.Errorf("reindex asset %d for search: %w", assetID, err)
	}
	return nil
}

// reindexPackMembersFTS rewrites tag_text for every asset in a pack, used when a
// pack tag changes and all members' inherited tags shift with it.
func reindexPackMembersFTS(ctx context.Context, tx *sql.Tx, packID int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE assets_fts SET tag_text = `+tagTextExpr+`
		 WHERE rowid IN (SELECT id FROM assets WHERE pack_id = ?)`, packID); err != nil {
		return fmt.Errorf("reindex pack %d members for search: %w", packID, err)
	}
	return nil
}

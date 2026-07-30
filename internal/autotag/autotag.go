// Package autotag derives the machine tags of §7 from what the index already
// knows: an asset's folder path and its classified kind and analysis flags.
//
// Two sources, kept distinct so §7's "overridable by manual tags" holds:
//
//   - auto_path — one `folder:<segment>` tag per meaningful folder in the
//     asset's pack-relative path, normalised by library.PathTagSegments.
//   - auto_type — `type:<kind>`, and `style:pixel-art` / `has:alpha` /
//     `has:animation` from the M2 image analysis.
//
// It is additive and idempotent: re-running applies nothing new and never
// removes or demotes a manual tag. It does not delete auto tags that no longer
// apply — a rare case (a reclassified file) left to a future reconcile pass.
package autotag

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/tags"
)

// PathTags returns the auto_path tags for a pack-relative path: a `folder:`
// namespaced tag per normalised, meaningful folder segment (§7).
func PathTags(relPath string) []string {
	segs := library.PathTagSegments(relPath)
	out := make([]string, 0, len(segs))
	for _, s := range segs {
		out = append(out, "folder:"+s)
	}
	return out
}

// TypeTags returns the auto_type tags for an asset's kind and analysis flags.
func TypeTags(kind string, isPixelArt, hasAlpha bool, frameCount int) []string {
	var out []string
	if kind != "" {
		out = append(out, "type:"+kind)
	}
	if isPixelArt {
		out = append(out, "style:pixel-art")
	}
	if hasAlpha {
		out = append(out, "has:alpha")
	}
	if frameCount > 1 {
		out = append(out, "has:animation")
	}
	return out
}

// Tagger applies auto tags across the whole index.
type Tagger struct {
	tags *tags.Store
	db   *db.DB
	log  *slog.Logger
}

// New builds a Tagger.
func New(database *db.DB, store *tags.Store, log *slog.Logger) *Tagger {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Tagger{tags: store, db: database, log: log}
}

// Report summarises a Retag run.
type Report struct {
	Assets       int // assets examined
	AssetsTagged int // assets that received at least one tag application
	PathTags     int // auto_path applications
	TypeTags     int // auto_type applications
	DistinctTags int // distinct canonical tags ensured
	Pruned       int // stale auto tags removed (a reclassified file, a renamed folder)
}

// Retag walks every asset and reconciles its auto tags: what applies now is
// applied, and what no longer applies is removed. Tags are ensured once per distinct
// canonical string and cached, so a 20k-asset library ensures each `folder:rocks` or
// `type:image` a single time.
func (tg *Tagger) Retag(ctx context.Context) (Report, error) {
	assets, err := tg.loadAssets(ctx, "", nil)
	if err != nil {
		return Report{}, err
	}
	return tg.apply(ctx, assets)
}

// RetagAssets reconciles the auto tags of specific assets. Used after a single
// asset's analysis lands, where a full sweep would be absurd.
func (tg *Tagger) RetagAssets(ctx context.Context, ids []int64) (Report, error) {
	if len(ids) == 0 {
		return Report{}, nil
	}
	args := make([]any, 0, len(ids))
	ph := make([]byte, 0, len(ids)*3)
	for i, id := range ids {
		if i > 0 {
			ph = append(ph, ',', ' ')
		}
		ph = append(ph, '?')
		args = append(args, id)
	}
	assets, err := tg.loadAssets(ctx, "WHERE id IN ("+string(ph)+")", args)
	if err != nil {
		return Report{}, err
	}
	return tg.apply(ctx, assets)
}

// RetagContent reconciles every asset holding the given content hash.
//
// This is the hook derive uses: `style:pixel-art` and `has:alpha` come from image
// analysis, so they cannot be right until that analysis exists, and analysis is keyed
// on content — every asset with these bytes gets the same answer.
func (tg *Tagger) RetagContent(ctx context.Context, sha256hex string) (Report, error) {
	assets, err := tg.loadAssets(ctx, "WHERE sha256 = ? AND missing_since IS NULL", []any{sha256hex})
	if err != nil {
		return Report{}, err
	}
	return tg.apply(ctx, assets)
}

// assetRow is what tagging needs about an asset.
type assetRow struct {
	id       int64
	relPath  string
	kind     string
	pixelArt bool
	hasAlpha bool
	frames   int
}

// loadAssets reads the tagging inputs, optionally filtered.
func (tg *Tagger) loadAssets(ctx context.Context, where string, args []any) ([]assetRow, error) {
	query := `SELECT id, rel_path, kind, is_pixel_art, has_alpha, frame_count FROM assets`
	if where != "" {
		query += " " + where
	}
	rows, err := tg.db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("retag: load assets: %w", err)
	}
	defer rows.Close()

	var assets []assetRow
	for rows.Next() {
		var (
			a          assetRow
			pixelArt   sql.NullInt64
			hasAlpha   sql.NullInt64
			frameCount sql.NullInt64
		)
		if err := rows.Scan(&a.id, &a.relPath, &a.kind, &pixelArt, &hasAlpha, &frameCount); err != nil {
			return nil, fmt.Errorf("retag: scan asset: %w", err)
		}
		a.pixelArt = pixelArt.Valid && pixelArt.Int64 != 0
		a.hasAlpha = hasAlpha.Valid && hasAlpha.Int64 != 0
		a.frames = int(frameCount.Int64)
		assets = append(assets, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return assets, nil
}

// apply is the shared core: ensure, apply, prune.
func (tg *Tagger) apply(ctx context.Context, assets []assetRow) (Report, error) {
	var rep Report

	// Reading is done; ensuring and applying take the writer. Cache ensured ids so
	// each distinct tag is created at most once across the whole run.
	ensured := map[string]int64{}
	ensure := func(canonical string) (int64, error) {
		if id, ok := ensured[canonical]; ok {
			return id, nil
		}
		t, err := tg.tags.Ensure(ctx, canonical)
		if err != nil {
			return 0, err
		}
		ensured[canonical] = t.ID
		return t.ID, nil
	}

	for _, a := range assets {
		rep.Assets++
		pathTags := PathTags(a.relPath)
		typeTags := TypeTags(a.kind, a.pixelArt, a.hasAlpha, a.frames)

		items := make([]tags.AssetTagItem, 0, len(pathTags)+len(typeTags))
		for _, canonical := range pathTags {
			id, err := ensure(canonical)
			if err != nil {
				return rep, err
			}
			items = append(items, tags.AssetTagItem{TagID: id, Source: tags.SourceAutoPath})
		}
		for _, canonical := range typeTags {
			id, err := ensure(canonical)
			if err != nil {
				return rep, err
			}
			items = append(items, tags.AssetTagItem{TagID: id, Source: tags.SourceAutoType})
		}
		if len(items) > 0 {
			if err := tg.tags.ApplyAssetTags(ctx, a.id, items); err != nil {
				return rep, fmt.Errorf("retag asset %d: %w", a.id, err)
			}
			rep.AssetsTagged++
			rep.PathTags += len(pathTags)
			rep.TypeTags += len(typeTags)
		}

		// Reconcile: drop the auto tags this asset no longer earns. Manual and
		// inherited tags are untouched — the keep list is only what this pass computed,
		// and PruneAutoTags filters by source.
		keep := make([]int64, 0, len(items))
		for _, it := range items {
			keep = append(keep, it.TagID)
		}
		removed, err := tg.tags.PruneAutoTags(ctx, a.id, keep)
		if err != nil {
			return rep, fmt.Errorf("prune stale auto tags on asset %d: %w", a.id, err)
		}
		rep.Pruned += removed
	}
	rep.DistinctTags = len(ensured)
	return rep, nil
}

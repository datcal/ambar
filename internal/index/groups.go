package index

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/library"
)

// primaryPrecedence is §5.1's rule, verbatim: "Primary-variant precedence, first match
// wins: png > webp > glb > gltf > fbx > obj > svg > aseprite > psd > kra > xcf."
//
// The idea is that the primary is the engine-ready file — the one you would actually
// drop into Godot — while the rest are editable sources attached to it.
var primaryPrecedence = []string{
	"png", "webp", "glb", "gltf", "fbx", "obj", "svg",
	"aseprite", "ase", // .ase is the older extension for the same format
	"psd", "kra", "xcf",
}

// precedenceRank returns a sort key for an extension; lower wins. Anything unlisted
// ranks last but is still eligible, so a group of nothing but .wav files still has a
// primary.
func precedenceRank(ext string) int {
	for i, candidate := range primaryPrecedence {
		if ext == candidate {
			return i
		}
	}
	return len(primaryPrecedence)
}

// GroupKey is §5.1's grouping key: "group by (pack, relative path with the
// format-folder segment removed, filename without extension)".
//
// So PNG/Plant1/idle.png, PSD/Plant1/idle.psd and ASEPRITE/Plant1/idle.aseprite all
// reduce to Plant1/idle — one logical asset in three formats.
func GroupKey(relPath string) string {
	stripped := library.StripFormatFolders(relPath)
	if ext := path.Ext(stripped); ext != "" {
		stripped = strings.TrimSuffix(stripped, ext)
	}
	return stripped
}

// GroupStats is what one regrouping pass changed.
type GroupStats struct {
	Groups int
	// MultiVariant is how many groups hold more than one file. §5.1's whole point:
	// without collapsing, each of these would be several grid rows.
	MultiVariant int
	Created      int
	Updated      int
	Reassigned   int
}

// groupMember is one asset as grouping sees it.
type groupMember struct {
	id      int64
	relPath string
	ext     string
	missing bool
	groupID int64
}

// Regroup recomputes every asset group.
//
// Runs as a scan phase. It derives entirely from rel_path, so it reads no files and
// costs one query plus the writes that actually change something — a rescan with no
// filesystem changes writes nothing.
//
// Missing assets stay in their group (§12 keeps their rows) but are never chosen as the
// primary while a present sibling exists: the grid should show a thumbnail that can
// actually be produced.
func (ix *Indexer) Regroup(ctx context.Context) (GroupStats, error) {
	var stats GroupStats

	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT a.id, a.pack_id, a.rel_path, a.ext, a.missing_since, coalesce(a.group_id, 0)
		FROM assets a
		ORDER BY a.pack_id, a.rel_path`)
	if err != nil {
		return stats, fmt.Errorf("load assets for grouping: %w", err)
	}
	defer rows.Close()

	// Keyed by pack, then by group key.
	byPack := map[int64]map[string][]groupMember{}
	for rows.Next() {
		var (
			m       groupMember
			packID  int64
			missing *int64
		)
		if err := rows.Scan(&m.id, &packID, &m.relPath, &m.ext, &missing, &m.groupID); err != nil {
			return stats, err
		}
		m.missing = missing != nil

		key := GroupKey(m.relPath)
		if byPack[packID] == nil {
			byPack[packID] = map[string][]groupMember{}
		}
		byPack[packID][key] = append(byPack[packID][key], m)
	}
	if err := rows.Err(); err != nil {
		return stats, err
	}

	existing, err := ix.loadGroups(ctx)
	if err != nil {
		return stats, err
	}

	now := time.Now().Unix()

	tx, err := ix.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return stats, fmt.Errorf("begin regrouping: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	// Deterministic order, so a scan summary and a test both see the same thing.
	packIDs := make([]int64, 0, len(byPack))
	for packID := range byPack {
		packIDs = append(packIDs, packID)
	}
	sort.Slice(packIDs, func(i, j int) bool { return packIDs[i] < packIDs[j] })

	for _, packID := range packIDs {
		groups := byPack[packID]

		keys := make([]string, 0, len(groups))
		for key := range groups {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			members := groups[key]
			primary := choosePrimary(members)

			stats.Groups++
			if len(members) > 1 {
				stats.MultiVariant++
			}

			identity := packGroupKey{packID: packID, key: key}
			prior, known := existing[identity]

			var groupID int64
			switch {
			case !known:
				if err := tx.QueryRowContext(ctx, `
					INSERT INTO asset_groups (pack_id, group_key, primary_asset_id, variant_count,
					                          created_at, updated_at)
					VALUES (?, ?, ?, ?, ?, ?)
					RETURNING id`,
					packID, key, primary.id, len(members), now, now,
				).Scan(&groupID); err != nil {
					return stats, fmt.Errorf("create group %q in pack %d: %w", key, packID, err)
				}
				stats.Created++

			default:
				groupID = prior.id
				if prior.primaryAssetID != primary.id || prior.variantCount != len(members) {
					if _, err := tx.ExecContext(ctx, `
						UPDATE asset_groups
						SET primary_asset_id = ?, variant_count = ?, updated_at = ?
						WHERE id = ?`, primary.id, len(members), now, groupID); err != nil {
						return stats, fmt.Errorf("update group %d: %w", groupID, err)
					}
					stats.Updated++
				}
			}

			// Point members at the group, writing only where it changed.
			for _, m := range members {
				if m.groupID == groupID {
					continue
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE assets SET group_id = ?, updated_at = ? WHERE id = ?`,
					groupID, now, m.id); err != nil {
					return stats, fmt.Errorf("assign asset %d to group %d: %w", m.id, groupID, err)
				}
				stats.Reassigned++
			}
		}
	}

	// Groups whose every member disappeared. Deleting an empty *group* is not
	// deleting user data — invariant 3 is about library content, and the asset rows
	// themselves are still there, marked missing. Leaving them would show empty rows
	// in the grid forever.
	for identity, prior := range existing {
		if _, stillWanted := byPack[identity.packID][identity.key]; stillWanted {
			continue
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM asset_groups WHERE id = ?`, prior.id); err != nil {
			return stats, fmt.Errorf("remove empty group %d: %w", prior.id, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return stats, fmt.Errorf("commit regrouping: %w", err)
	}
	return stats, nil
}

// choosePrimary applies §5.1's precedence.
//
// A present file always beats a missing one regardless of extension: the primary is
// what the grid renders, and rendering something that is not on disk is pointless.
// Within that, precedence decides, and rel_path breaks ties so the choice is stable
// across scans.
func choosePrimary(members []groupMember) groupMember {
	best := members[0]
	for _, m := range members[1:] {
		if betterPrimary(m, best) {
			best = m
		}
	}
	return best
}

func betterPrimary(candidate, current groupMember) bool {
	if candidate.missing != current.missing {
		return !candidate.missing
	}
	cRank, curRank := precedenceRank(candidate.ext), precedenceRank(current.ext)
	if cRank != curRank {
		return cRank < curRank
	}
	return candidate.relPath < current.relPath
}

type packGroupKey struct {
	packID int64
	key    string
}

type groupRow struct {
	id             int64
	primaryAssetID int64
	variantCount   int
}

func (ix *Indexer) loadGroups(ctx context.Context) (map[packGroupKey]groupRow, error) {
	rows, err := ix.db.Reader.QueryContext(ctx,
		`SELECT id, pack_id, group_key, coalesce(primary_asset_id, 0), variant_count FROM asset_groups`)
	if err != nil {
		return nil, fmt.Errorf("load asset groups: %w", err)
	}
	defer rows.Close()

	out := map[packGroupKey]groupRow{}
	for rows.Next() {
		var (
			identity packGroupKey
			row      groupRow
		)
		if err := rows.Scan(&row.id, &identity.packID, &identity.key,
			&row.primaryAssetID, &row.variantCount); err != nil {
			return nil, err
		}
		out[identity] = row
	}
	return out, rows.Err()
}

// --- reading groups for the UI ----------------------------------------------

// Group is one logical asset: its primary variant plus a count of the rest.
type Group struct {
	ID       int64
	PackID   int64
	PackName string
	GroupKey string
	// Primary is the variant the grid displays.
	Primary Asset
	// VariantCount includes the primary, so 1 means "no other formats".
	VariantCount int
}

// MultiVariant reports whether this group holds more than one format.
func (g Group) MultiVariant() bool { return g.VariantCount > 1 }

// GroupPage is one page of groups.
type GroupPage struct {
	Groups     []Group
	NextCursor string
	Total      int
}

// ListGroups is the grid query: one row per logical asset rather than per file.
//
// This is what makes the grid usable on the target library. §5.1: "Indexing them
// independently means every sprite appears three or four times and the grid becomes
// noise." It is also invariant 7 — variants are never presented as duplicates.
//
// Filters match against *any* variant but return the group once, so searching "psd"
// finds artwork that has a PSD even though the grid shows its PNG.
func (ix *Indexer) ListGroups(ctx context.Context, opts ListOptions) (*GroupPage, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	where, args := groupFilters(opts)

	var total int
	if err := ix.db.Reader.QueryRowContext(ctx, `
		SELECT count(*)
		FROM asset_groups g
		JOIN assets a ON a.id = g.primary_asset_id
		JOIN packs p ON p.id = g.pack_id
		WHERE `+strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count groups: %w", err)
	}

	pageWhere, pageArgs := where, args
	if opts.Cursor != "" {
		filename, id, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		pageWhere = append(append([]string{}, where...), `(a.filename, g.id) > (?, ?)`)
		pageArgs = append(append([]any{}, args...), filename, id)
	}

	query := `
		SELECT g.id, g.pack_id, g.group_key, g.variant_count, ` + assetColumns + `
		FROM asset_groups g
		JOIN assets a ON a.id = g.primary_asset_id
		JOIN packs p ON p.id = g.pack_id
		WHERE ` + strings.Join(pageWhere, " AND ") + `
		ORDER BY a.filename, g.id
		LIMIT ?`

	rows, err := ix.db.Reader.QueryContext(ctx, query, append(pageArgs, limit+1)...)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	page := &GroupPage{Total: total}
	for rows.Next() {
		var g Group
		scanned, err := scanGroupRow(rows, &g)
		if err != nil {
			return nil, err
		}
		g.Primary = scanned
		g.PackName = scanned.PackName
		page.Groups = append(page.Groups, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(page.Groups) > limit {
		last := page.Groups[limit-1]
		page.Groups = page.Groups[:limit]
		page.NextCursor = encodeCursor(last.Primary.Filename, last.ID)
	}
	return page, nil
}

// groupFilters builds the WHERE clause for group listing.
func groupFilters(opts ListOptions) ([]string, []any) {
	where := []string{"1 = 1"}
	var args []any

	if !opts.IncludeMissing {
		where = append(where, "a.missing_since IS NULL")
	}
	if opts.Kind != "" {
		// Matched against any variant: a group whose primary is a PNG but which also
		// holds a .glb should appear under a model filter.
		where = append(where,
			"EXISTS (SELECT 1 FROM assets v WHERE v.group_id = g.id AND v.kind = ?)")
		args = append(args, opts.Kind)
	}
	if opts.PackID != 0 {
		where = append(where, "g.pack_id = ?")
		args = append(args, opts.PackID)
	}
	if match := FTSQuery(opts.Query); match != "" {
		// Any variant matching pulls in the whole group, so searching for a format's
		// filename finds the artwork even though the grid shows a different variant.
		where = append(where, `g.id IN (
			SELECT v.group_id FROM assets v
			WHERE v.group_id IS NOT NULL
			  AND v.id IN (SELECT rowid FROM assets_fts WHERE assets_fts MATCH ?))`)
		args = append(args, match)
	}
	return where, args
}

// Variants returns every asset in a group, primary first then by precedence.
//
// §5.1: "The grid shows one entry per group; the detail panel lists variants with
// download links per format."
func (ix *Indexer) Variants(ctx context.Context, groupID int64) ([]Asset, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `SELECT `+assetColumns+`
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		WHERE a.group_id = ?`, groupID)
	if err != nil {
		return nil, fmt.Errorf("load variants of group %d: %w", groupID, err)
	}
	defer rows.Close()

	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.Slice(out, func(i, j int) bool {
		ri, rj := precedenceRank(out[i].Ext), precedenceRank(out[j].Ext)
		if ri != rj {
			return ri < rj
		}
		return out[i].RelPath < out[j].RelPath
	})
	return out, nil
}

// GroupOf returns the group an asset belongs to, for the detail page.
func (ix *Indexer) GroupOf(ctx context.Context, assetID int64) (Group, error) {
	var g Group
	row := ix.db.Reader.QueryRowContext(ctx, `
		SELECT g.id, g.pack_id, g.group_key, g.variant_count, `+assetColumns+`
		FROM asset_groups g
		JOIN assets a ON a.id = g.primary_asset_id
		JOIN packs p ON p.id = g.pack_id
		WHERE g.id = (SELECT group_id FROM assets WHERE id = ?)`, assetID)

	primary, err := scanGroupRow(row, &g)
	if err != nil {
		return Group{}, err
	}
	g.Primary = primary
	g.PackName = primary.PackName
	return g, nil
}

// scanGroupRow reads the four group columns followed by a full asset row.
func scanGroupRow(row scanner, g *Group) (Asset, error) {
	var (
		a                Asset
		width, height    *int64
		mtime            int64
		firstSeen        int64
		lastVerified     int64
		missingSince     *int64
		contentChangedAt *int64
		derived          deriveColumns
	)
	if err := row.Scan(
		&g.ID, &g.PackID, &g.GroupKey, &g.VariantCount,
		&a.ID, &a.PackID, &a.PackName, &a.PackSlug, &a.PackRelPath, &a.RelPath,
		&a.Filename, &a.Ext, &a.Kind, &a.Size, &mtime, &a.SHA256, &width, &height,
		&firstSeen, &lastVerified, &missingSince, &contentChangedAt,
		&derived.hasAlpha, &derived.hasSemitransparent, &derived.colorCount,
		&derived.isPixelArt, &derived.phash,
		&derived.frameCount, &derived.fps, &derived.animationNames,
		&a.DeriveState, &a.DeriveError, &a.DeriveVersion,
	); err != nil {
		return Asset{}, err
	}
	derived.apply(&a)

	a.ModTime = time.Unix(mtime, 0)
	a.FirstSeenAt = time.Unix(firstSeen, 0)
	a.LastVerifiedAt = time.Unix(lastVerified, 0)
	if width != nil {
		a.Width = int(*width)
	}
	if height != nil {
		a.Height = int(*height)
	}
	if missingSince != nil {
		t := time.Unix(*missingSince, 0)
		a.MissingSince = &t
	}
	if contentChangedAt != nil {
		t := time.Unix(*contentChangedAt, 0)
		a.ContentChangedAt = &t
	}
	return a, nil
}

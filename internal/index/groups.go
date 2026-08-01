package index

import (
	"context"
	"database/sql"
	"errors"
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
	Groups []Group
	// NextCursor is the API's keyset position; empty for the grid's numbered paging.
	NextCursor string
	Total      int

	// Page is 1-based, PageSize is how many fit on one, and Sort is the order they came
	// back in — all three so the UI can render "page 3 of 57" without recomputing them.
	Page     int
	PageSize int
	Sort     SortOrder
}

// Pages is the total number of pages, at least 1 so an empty library still has a page 1.
func (p GroupPage) Pages() int {
	if p.PageSize <= 0 {
		return 1
	}
	return max(1, (p.Total+p.PageSize-1)/p.PageSize)
}

// HasPrev and HasNext are for the pager's arrows.
func (p GroupPage) HasPrev() bool { return p.Page > 1 }
func (p GroupPage) HasNext() bool { return p.Page < p.Pages() }

// PrevPage and NextPage clamp, so a hand-edited page number cannot produce a link to 0.
func (p GroupPage) PrevPage() int { return max(1, p.Page-1) }
func (p GroupPage) NextPage() int { return min(p.Pages(), p.Page+1) }

// FirstShown and LastShown are the 1-based range on this page, for "101–200 of 5610".
func (p GroupPage) FirstShown() int {
	if p.Total == 0 {
		return 0
	}
	return (p.Page-1)*p.PageSize + 1
}

func (p GroupPage) LastShown() int {
	return min(p.Total, p.FirstShown()+len(p.Groups)-1)
}

// PageNumbers is the list of page links to render, with 0 standing for a gap.
//
// A library of 5610 assets is 57 pages, and 57 links is not navigation. This is the usual
// window: the first page, the last page, the current one with a neighbour either side, and
// an ellipsis where numbers were skipped.
func (p GroupPage) PageNumbers() []int {
	total := p.Pages()
	if total <= 9 {
		out := make([]int, 0, total)
		for i := 1; i <= total; i++ {
			out = append(out, i)
		}
		return out
	}

	want := map[int]bool{1: true, total: true}
	for i := p.Page - 1; i <= p.Page+1; i++ {
		if i >= 1 && i <= total {
			want[i] = true
		}
	}

	out := make([]int, 0, 9)
	prev := 0
	for i := 1; i <= total; i++ {
		if !want[i] {
			continue
		}
		if prev != 0 && i != prev+1 {
			out = append(out, 0) // a gap
		}
		out = append(out, i)
		prev = i
	}
	return out
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

	// The §7 query is matched against ANY variant in the group, compiled over the
	// `v` alias and wrapped so a match on one format pulls in the whole group.
	compiled, err := ix.compileQuery(ctx, opts.Query, "v")
	if err != nil {
		return nil, err
	}
	if compiled.SQL != "" {
		where = append(where, `g.id IN (
			SELECT v.group_id FROM assets v
			WHERE v.group_id IS NOT NULL AND `+compiled.SQL+`)`)
		args = append(args, compiled.Args...)
	}

	var total int
	if err := ix.db.Reader.QueryRowContext(ctx, `
		SELECT count(*)
		FROM asset_groups g
		JOIN assets a ON a.id = g.primary_asset_id
		JOIN packs p ON p.id = g.pack_id
		WHERE `+strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count groups: %w", err)
	}

	// Two paging mechanisms, on purpose (M16):
	//
	//   - The API keeps the keyset cursor (§10). It is stateless, stable while the library
	//     changes under it, and a client walking every asset only ever needs "next".
	//   - The grid uses an offset, because numbered pages are what it needs and a cursor
	//     cannot produce them: you cannot jump to page 4, cannot go back, and cannot share
	//     a URL for where you are.
	//
	// The cursor path only supports the default order, which is all the API ever asked for.
	sort := opts.Sort
	if sort == "" {
		sort = SortDefault
	}

	// Page == 0 is the cursor caller: the API leaves it unset, the grid always sends a
	// number. The whole mode runs in the keyset's own order, including its first page —
	// deriving a cursor from a page that was ordered some other way is how a keyset walk
	// starts repeating and skipping rows, which is exactly what TestGroupPagination caught
	// the first time this was written.
	cursorMode := opts.Page == 0

	order := sort.orderBy()
	pageWhere, pageArgs := where, args
	offset := 0

	switch {
	case cursorMode:
		order = "a.filename ASC, g.id ASC"
		if opts.Cursor != "" {
			filename, id, err := decodeCursor(opts.Cursor)
			if err != nil {
				return nil, err
			}
			pageWhere = append(append([]string{}, where...), `(a.filename, g.id) > (?, ?)`)
			pageArgs = append(append([]any{}, args...), filename, id)
		}
	case opts.Page > 1:
		offset = (opts.Page - 1) * limit
	}

	query := `
		SELECT g.id, g.pack_id, g.group_key, g.variant_count, ` + assetListColumns + `
		FROM asset_groups g
		JOIN assets a ON a.id = g.primary_asset_id
		JOIN packs p ON p.id = g.pack_id
		WHERE ` + strings.Join(pageWhere, " AND ") + `
		ORDER BY ` + order + `
		LIMIT ? OFFSET ?`

	rows, err := ix.db.Reader.QueryContext(ctx, query, append(pageArgs, limit+1, offset)...)
	if err != nil {
		return nil, fmt.Errorf("list groups: %w", err)
	}
	defer rows.Close()

	page := &GroupPage{Total: total, Page: max(1, opts.Page), PageSize: limit, Sort: sort}
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
		// A cursor is only offered to the caller paging by cursor; handing one to a
		// numbered pager would invite mixing the two, which skips rows.
		if cursorMode {
			page.NextCursor = encodeCursor(last.Primary.Filename, last.ID)
		}
	}
	return page, nil
}

// Neighbour is the asset on one side of the one you are looking at.
type Neighbour struct {
	ID       int64
	Filename string
}

// Neighbours returns the groups either side of a given one, in the same order the grid
// uses, under the same filters.
//
// This is what makes the detail page a place you can *browse* rather than a dead end you
// have to back out of: §8 asks for keyboard navigation, and an asset browser without
// previous/next is a filesystem with extra steps.
//
// The position is given as (filename, groupID) rather than as an offset, so it costs one
// indexed comparison instead of counting rows — the same keyset the grid pages with. The
// filters come in as the ordinary ListOptions, so J and K walk the search you came from
// once the grid passes it along, and the whole library when it does not.
func (ix *Indexer) Neighbours(ctx context.Context, opts ListOptions, filename string, groupID int64) (prev, next *Neighbour, err error) {
	where, args := groupFilters(opts)

	compiled, err := ix.compileQuery(ctx, opts.Query, "v")
	if err != nil {
		return nil, nil, err
	}
	if compiled.SQL != "" {
		where = append(where, `g.id IN (
			SELECT v.group_id FROM assets v
			WHERE v.group_id IS NOT NULL AND `+compiled.SQL+`)`)
		args = append(args, compiled.Args...)
	}

	side := func(cmp, direction string) (*Neighbour, error) {
		query := `
			SELECT g.primary_asset_id, a.filename
			FROM asset_groups g
			JOIN assets a ON a.id = g.primary_asset_id
			JOIN packs p ON p.id = g.pack_id
			WHERE ` + strings.Join(where, " AND ") + `
			  AND (a.filename, g.id) ` + cmp + ` (?, ?)
			ORDER BY a.filename ` + direction + `, g.id ` + direction + `
			LIMIT 1`

		row := ix.db.Reader.QueryRowContext(ctx, query, append(append([]any{}, args...), filename, groupID)...)
		var n Neighbour
		switch err := row.Scan(&n.ID, &n.Filename); {
		case errors.Is(err, sql.ErrNoRows):
			return nil, nil // an end of the list, not a fault
		case err != nil:
			return nil, err
		}
		return &n, nil
	}

	if prev, err = side("<", "DESC"); err != nil {
		return nil, nil, fmt.Errorf("previous asset: %w", err)
	}
	if next, err = side(">", "ASC"); err != nil {
		return nil, nil, fmt.Errorf("next asset: %w", err)
	}
	return prev, next, nil
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
	// The folder tree's filter (M14). Matched against the primary asset's path: a
	// group belongs to the directory its engine-ready file sits in, which is the one
	// the grid tile represents.
	if dir := cleanDir(opts.Dir); dir != "" {
		where = append(where, dirExpr("a"))
		args = append(args, dir, dir+"/", dir+"/")
	}
	// The §7 query clause is added by ListGroups, which has the context and tag
	// resolver the compiler needs; groupFilters covers only the sidebar facets.
	return where, args
}

// Variants returns every asset in a group, primary first then by precedence.
//
// §5.1: "The grid shows one entry per group; the detail panel lists variants with
// download links per format."
func (ix *Indexer) Variants(ctx context.Context, groupID int64) ([]Asset, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `SELECT `+assetListColumns+`
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
		durationMS       sql.NullInt64
		sampleRate       sql.NullInt64
		channels         sql.NullInt64
		bitDepth         sql.NullInt64
		peakDBFS         sql.NullFloat64
		isLoopable       sql.NullInt64
		triCount         sql.NullInt64
		vertCount        sql.NullInt64
		bboxX            sql.NullFloat64
		bboxY            sql.NullFloat64
		bboxZ            sql.NullFloat64
		materialCount    sql.NullInt64
		frameW           sql.NullInt64
		frameH           sql.NullInt64
		frameCols        sql.NullInt64
		frameRows        sql.NullInt64
		frameSource      sql.NullString
		paletteJSON      sql.NullString
		paletteKind      sql.NullString
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
		&durationMS, &sampleRate, &channels, &bitDepth, &peakDBFS, &isLoopable,
		&triCount, &vertCount, &bboxX, &bboxY, &bboxZ, &materialCount,
		&frameW, &frameH, &frameCols, &frameRows, &frameSource,
		&paletteJSON, &paletteKind,
	); err != nil {
		return Asset{}, err
	}
	derived.apply(&a)
	a.DurationMS = int(durationMS.Int64)
	a.SampleRate = int(sampleRate.Int64)
	a.Channels = int(channels.Int64)
	a.BitDepth = int(bitDepth.Int64)
	a.PeakDBFS = peakDBFS.Float64
	a.IsLoopable = isLoopable.Valid && isLoopable.Int64 != 0
	a.TriCount = int(triCount.Int64)
	a.VertCount = int(vertCount.Int64)
	a.BBoxX, a.BBoxY, a.BBoxZ = bboxX.Float64, bboxY.Float64, bboxZ.Float64
	a.MaterialCount = int(materialCount.Int64)
	a.FrameW = int(frameW.Int64)
	a.FrameH = int(frameH.Int64)
	a.FrameCols = int(frameCols.Int64)
	a.FrameRows = int(frameRows.Int64)
	a.FrameSource = frameSource.String
	a.PaletteJSON = paletteJSON.String
	a.PaletteKind = paletteKind.String

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

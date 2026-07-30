package dupes

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// asset is one live indexed file, with everything the findings and the keep
// annotations need. Loaded once per scan: a duplicate report is inherently a
// whole-library question, and twenty thousand of these is a few megabytes.
type asset struct {
	id     int64
	path   string // library-relative, slash-separated
	packID int64
	sha    string
	size   int64
	// phash is the 64-bit perceptual hash; hasPhash says whether there is one at
	// all. Two fields rather than a zero sentinel: an all-zero hash is a legitimate
	// value (a uniformly flat image produces one), and folding it together with
	// "absent" would drop those images out of the comparison entirely.
	phash        uint64
	hasPhash     bool
	groupID      int64
	firstSeenAt  int64
	isPrimary    bool
	variantCount int
}

// pack is one pack with its content hash set, which is what §9.1's pack
// similarity is computed from.
type pack struct {
	id                 int64
	name               string
	path               string
	provenanceComplete bool
	firstSeenAt        int64

	hashes map[string]struct{}
	assets int
	bytes  int64

	packTags  int
	assetTags int
}

type snapshot struct {
	assets []*asset
	packs  map[int64]*pack
	// uses maps an asset id to the projects that reference it, so a copy that can
	// never be removed says so (invariant 5).
	uses map[int64][]string
}

// copyOf builds the reportable form of an asset, with the §9.1 annotations
// attached.
func (s *snapshot) copyOf(a *asset) Copy {
	c := Copy{
		AssetID:        a.id,
		Path:           a.path,
		PackID:         a.packID,
		Bytes:          a.size,
		Sha:            a.sha,
		InRaw:          inRaw(a.path),
		Depth:          strings.Count(a.path, "/") + 1,
		FirstSeenAt:    a.firstSeenAt,
		VariantCount:   a.variantCount,
		IsGroupPrimary: a.isPrimary,
		ProjectUses:    s.uses[a.id],
	}
	if p := s.packs[a.packID]; p != nil {
		c.PackName = p.name
		c.PackPath = p.path
		c.ProvenanceComplete = p.provenanceComplete
	}
	return c
}

// inRaw reports whether any path segment is `raw` — §17's staging bucket, and one
// of the §9.1 keep-policy facts ("outside `raw/` or not").
func inRaw(path string) bool {
	for _, seg := range strings.Split(path, "/") {
		if strings.EqualFold(seg, "raw") {
			return true
		}
	}
	return false
}

// load reads the whole live index. Only assets the last scan still found are
// included: an asset marked missing is not a copy of anything, which is what keeps
// §9.1's rule 2 (a move is not a duplicate) from producing false findings.
func (d *Detector) load(ctx context.Context) (*snapshot, error) {
	snap := &snapshot{packs: map[int64]*pack{}, uses: map[int64][]string{}}

	packRows, err := d.db.Reader.QueryContext(ctx, `
		SELECT id, name, library_rel_path, provenance_state, first_seen_at
		FROM packs`)
	if err != nil {
		return nil, fmt.Errorf("load packs: %w", err)
	}
	defer packRows.Close()

	for packRows.Next() {
		p := &pack{hashes: map[string]struct{}{}}
		var state string
		if err := packRows.Scan(&p.id, &p.name, &p.path, &state, &p.firstSeenAt); err != nil {
			return nil, fmt.Errorf("scan pack: %w", err)
		}
		p.provenanceComplete = state == "complete"
		snap.packs[p.id] = p
	}
	if err := packRows.Err(); err != nil {
		return nil, fmt.Errorf("load packs: %w", err)
	}

	assetRows, err := d.db.Reader.QueryContext(ctx, `
		SELECT a.id, a.pack_id,
		       CASE WHEN p.library_rel_path = '' THEN a.rel_path
		            ELSE p.library_rel_path || '/' || a.rel_path END AS lib_path,
		       a.sha256, a.size, a.phash, a.group_id, a.first_seen_at,
		       COALESCE(g.variant_count, 0),
		       CASE WHEN g.primary_asset_id = a.id THEN 1 ELSE 0 END
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		LEFT JOIN asset_groups g ON g.id = a.group_id
		WHERE a.missing_since IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("load assets: %w", err)
	}
	defer assetRows.Close()

	for assetRows.Next() {
		a := &asset{}
		var phash sql.NullString
		var groupID sql.NullInt64
		var primary int
		if err := assetRows.Scan(&a.id, &a.packID, &a.path, &a.sha, &a.size,
			&phash, &groupID, &a.firstSeenAt, &a.variantCount, &primary); err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		if phash.Valid && len(phash.String) == 16 {
			// 16 hex characters (§ 0003_derive.sql). An unparseable value is treated as
			// absent rather than as zero, which would cluster every broken row together.
			if v, err := strconv.ParseUint(phash.String, 16, 64); err == nil {
				a.phash, a.hasPhash = v, true
			}
		}
		if groupID.Valid {
			a.groupID = groupID.Int64
		}
		a.isPrimary = primary == 1

		snap.assets = append(snap.assets, a)
		if p := snap.packs[a.packID]; p != nil {
			p.hashes[a.sha] = struct{}{}
			p.assets++
			p.bytes += a.size
		}
	}
	if err := assetRows.Err(); err != nil {
		return nil, fmt.Errorf("load assets: %w", err)
	}

	// Packs with no live assets cannot take part in a similarity comparison, and an
	// empty hash set would be a subset of everything.
	for id, p := range snap.packs {
		if p.assets == 0 {
			delete(snap.packs, id)
		}
	}

	if err := d.loadTagCounts(ctx, snap); err != nil {
		return nil, err
	}
	if err := d.loadProjectUses(ctx, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

// loadTagCounts counts the curation that would be lost if a pack were removed —
// what §9.1 says must be transferred onto the superset first.
func (d *Detector) loadTagCounts(ctx context.Context, snap *snapshot) error {
	packTags, err := d.db.Reader.QueryContext(ctx, `
		SELECT pack_id, count(*) FROM pack_tags GROUP BY pack_id`)
	if err != nil {
		return fmt.Errorf("count pack tags: %w", err)
	}
	defer packTags.Close()
	for packTags.Next() {
		var id int64
		var n int
		if err := packTags.Scan(&id, &n); err != nil {
			return fmt.Errorf("scan pack tag count: %w", err)
		}
		if p := snap.packs[id]; p != nil {
			p.packTags = n
		}
	}
	if err := packTags.Err(); err != nil {
		return fmt.Errorf("count pack tags: %w", err)
	}

	// Only manual tags count as curation worth warning about: the auto_path and
	// auto_type tags are recomputed from the filesystem on the next scan, so they
	// are not lost in any meaningful sense.
	assetTags, err := d.db.Reader.QueryContext(ctx, `
		SELECT a.pack_id, count(*)
		FROM asset_tags t
		JOIN assets a ON a.id = t.asset_id
		WHERE t.source = 'manual'
		GROUP BY a.pack_id`)
	if err != nil {
		return fmt.Errorf("count asset tags: %w", err)
	}
	defer assetTags.Close()
	for assetTags.Next() {
		var id int64
		var n int
		if err := assetTags.Scan(&id, &n); err != nil {
			return fmt.Errorf("scan asset tag count: %w", err)
		}
		if p := snap.packs[id]; p != nil {
			p.assetTags = n
		}
	}
	return assetTags.Err()
}

// loadProjectUses reads the active project references (§10).
func (d *Detector) loadProjectUses(ctx context.Context, snap *snapshot) error {
	rows, err := d.db.Reader.QueryContext(ctx, `
		SELECT u.asset_id,
		       CASE WHEN pr.name != '' THEN pr.name ELSE pr.uuid END
		FROM project_uses u
		JOIN projects pr ON pr.id = u.project_id
		WHERE u.removed_at IS NULL`)
	if err != nil {
		return fmt.Errorf("load project uses: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return fmt.Errorf("scan project use: %w", err)
		}
		seen := false
		for _, existing := range snap.uses[id] {
			if existing == name {
				seen = true
				break
			}
		}
		if !seen {
			snap.uses[id] = append(snap.uses[id], name)
		}
	}
	return rows.Err()
}

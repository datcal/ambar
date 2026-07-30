package dupes

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// §9.1 "Resolve at pack level first": one craftpix pack downloaded twice under
// slightly different folder names produces four hundred duplicate file rows and no
// actionable insight. Pack similarity is computed from the set of member sha256
// values, which is why it survives a rename, a re-extract, or a different folder
// depth.

// PackRef is one side of a pack comparison.
type PackRef struct {
	PackID int64  `json:"pack_id"`
	Name   string `json:"name"`
	// Path is library-relative: it is also the removal target if the user chooses
	// to act on this pack.
	Path               string `json:"path"`
	Assets             int    `json:"assets"`
	Bytes              int64  `json:"bytes"`
	UniqueHashes       int    `json:"unique_hashes"`
	ProvenanceComplete bool   `json:"provenance_complete"`
	FirstSeenAt        int64  `json:"first_seen_at"`
	// PackTags and ManualAssetTags are the curation §9.1 wants transferred before a
	// pack is removed.
	PackTags        int `json:"pack_tags"`
	ManualAssetTags int `json:"manual_asset_tags"`
	// ProjectUses names the projects using files in this pack. Non-empty means the
	// pack cannot be removed wholesale (invariant 5), and the report says so before
	// the user invests any thought in it.
	ProjectUses []string `json:"project_uses,omitempty"`
}

// Blocked reports whether a project depends on this pack.
func (r PackRef) Blocked() bool { return len(r.ProjectUses) > 0 }

// FirstSeen is the timestamp as a time, for templates.
func (r PackRef) FirstSeen() time.Time { return time.Unix(r.FirstSeenAt, 0) }

// PackFinding is one pack relationship. For a subset, Candidate is contained in
// Container. For identical packs the two are interchangeable and Candidate is
// merely the one the keep heuristics would let go. For an overlap neither is a
// proposal — the finding exists to be read.
type PackFinding struct {
	Kind      Kind    `json:"kind"`
	Candidate PackRef `json:"candidate"`
	Container PackRef `json:"container"`

	SharedHashes    int     `json:"shared_hashes"`
	OnlyInCandidate int     `json:"only_in_candidate"`
	OnlyInContainer int     `json:"only_in_container"`
	Jaccard         float64 `json:"jaccard"`
	// Bytes is what removing Candidate would reclaim. Zero for an overlap, which is
	// reported only.
	Bytes int64 `json:"bytes"`
	// Transfers names what would have to move onto Container first, in words, so the
	// UI can say it before acting (§9.1).
	Transfers []string `json:"transfers,omitempty"`
}

// ID is the finding label recorded in the trash record and the audit log.
func (f PackFinding) ID() string {
	return fmt.Sprintf("dupes:%s:%d:%d", f.Kind, f.Candidate.PackID, f.Container.PackID)
}

// SimilarityPercent is the Jaccard similarity as a rounded percentage.
func (f PackFinding) SimilarityPercent() int { return int(f.Jaccard*100 + 0.5) }

// packFindings compares every pack pair that shares at least one content hash.
//
// The pairing is driven by an inverted hash -> packs index rather than a nested
// loop over all packs: a library with a thousand packs has half a million pairs,
// almost none of which share anything.
func (d *Detector) packFindings(snap *snapshot, report *Report) []PackFinding {
	if len(snap.packs) < 2 {
		return nil
	}

	// Which packs use each asset, so a pack-level block can name the projects.
	usesByPack := map[int64][]string{}
	for _, a := range snap.assets {
		for _, project := range snap.uses[a.id] {
			if !containsString(usesByPack[a.packID], project) {
				usesByPack[a.packID] = append(usesByPack[a.packID], project)
			}
		}
	}

	// Inverted index, then a shared-hash count per candidate pair.
	packsBySha := map[string][]int64{}
	for id, p := range snap.packs {
		for sha := range p.hashes {
			packsBySha[sha] = append(packsBySha[sha], id)
		}
	}
	type pairKey struct{ a, b int64 } // always a < b
	shared := map[pairKey]int{}
	for _, ids := range packsBySha {
		if len(ids) < 2 {
			continue
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		for i := 0; i < len(ids); i++ {
			for j := i + 1; j < len(ids); j++ {
				shared[pairKey{ids[i], ids[j]}]++
			}
		}
	}

	// Bytes a pack would reclaim if removed, counting only assets whose content also
	// exists in the other pack: those are the bytes that are genuinely redundant.
	bytesShared := func(from, with *pack) int64 {
		var total int64
		for _, a := range snap.assets {
			if a.packID != from.id {
				continue
			}
			if _, ok := with.hashes[a.sha]; ok {
				total += a.size
			}
		}
		return total
	}

	var findings []PackFinding
	for pair, count := range shared {
		a, b := snap.packs[pair.a], snap.packs[pair.b]
		if a == nil || b == nil {
			continue
		}
		aSize, bSize := len(a.hashes), len(b.hashes)
		union := aSize + bSize - count
		jaccard := 0.0
		if union > 0 {
			jaccard = float64(count) / float64(union)
		}

		refA := packRef(a, usesByPack[a.id])
		refB := packRef(b, usesByPack[b.id])

		var finding PackFinding
		switch {
		case count == aSize && count == bSize:
			// Same content hash set. §9.1: report the pair and let the user pick which
			// one goes, if either. The candidate is the one the keep heuristics favour
			// letting go — a hint, exactly like the file-level one.
			candidate, container := refA, refB
			if packKeepScore(refA) > packKeepScore(refB) {
				candidate, container = refB, refA
			}
			finding = PackFinding{
				Kind: KindPackIdentical, Candidate: candidate, Container: container,
				Bytes: candidate.Bytes,
			}
		case count == aSize:
			finding = PackFinding{Kind: KindPackSubset, Candidate: refA, Container: refB,
				Bytes: bytesShared(a, b)}
		case count == bSize:
			finding = PackFinding{Kind: KindPackSubset, Candidate: refB, Container: refA,
				Bytes: bytesShared(b, a)}
		case jaccard >= d.opts.PackJaccard:
			// Neither contains the other and the remainder matters, so there is no clean
			// answer and nothing is proposed (§9.1).
			finding = PackFinding{Kind: KindPackOverlap, Candidate: refA, Container: refB}
		default:
			continue // an incidental shared file is not a relationship
		}

		finding.SharedHashes = count
		finding.Jaccard = jaccard
		finding.OnlyInCandidate = len(snap.packs[finding.Candidate.PackID].hashes) - count
		finding.OnlyInContainer = len(snap.packs[finding.Container.PackID].hashes) - count
		if finding.Kind.Actionable() {
			finding.Transfers = transferNotes(finding.Candidate)
		}
		findings = append(findings, finding)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Bytes != findings[j].Bytes {
			return findings[i].Bytes > findings[j].Bytes
		}
		return findings[i].ID() < findings[j].ID()
	})
	return capFindings(findings, d.opts.MaxFindings, "pack similarity", report)
}

func packRef(p *pack, projectUses []string) PackRef {
	sort.Strings(projectUses)
	return PackRef{
		PackID:             p.id,
		Name:               p.name,
		Path:               p.path,
		Assets:             p.assets,
		Bytes:              p.bytes,
		UniqueHashes:       len(p.hashes),
		ProvenanceComplete: p.provenanceComplete,
		FirstSeenAt:        p.firstSeenAt,
		PackTags:           p.packTags,
		ManualAssetTags:    p.assetTags,
		ProjectUses:        projectUses,
	}
}

// packKeepScore is the pack-level version of the file-level keep hint: the same
// §9.1 facts, applied to a whole pack. Higher means "keep this one".
func packKeepScore(r PackRef) int {
	score := 0
	if r.Blocked() {
		score += 1000
	}
	if r.ProvenanceComplete {
		score += 100
	}
	if !inRaw(r.Path) {
		score += 50
	}
	score += r.PackTags + r.ManualAssetTags
	// An older pack is more likely the one that has been curated and referenced.
	if r.FirstSeenAt > 0 {
		score -= 1
	}
	return score
}

// transferNotes says, in words, what curation would have to move onto the
// container before the candidate could go (§9.1: "transfer its tags and provenance
// onto the superset first … and say so before acting").
func transferNotes(r PackRef) []string {
	var notes []string
	if r.PackTags > 0 {
		notes = append(notes, fmt.Sprintf("%d pack tag(s)", r.PackTags))
	}
	if r.ManualAssetTags > 0 {
		notes = append(notes, fmt.Sprintf("%d manually tagged file(s)", r.ManualAssetTags))
	}
	if r.ProvenanceComplete {
		notes = append(notes, "provenance (source, author, licence)")
	}
	return notes
}

// TransferSummary is what a curation transfer moved.
type TransferSummary struct {
	PackTags   int      `json:"pack_tags"`
	AssetTags  int      `json:"asset_tags"`
	Provenance []string `json:"provenance,omitempty"`
	Unmatched  int      `json:"unmatched_assets,omitempty"`
}

// Empty reports whether nothing needed transferring.
func (s TransferSummary) Empty() bool {
	return s.PackTags == 0 && s.AssetTags == 0 && len(s.Provenance) == 0
}

// Describe is the summary as a sentence for the audit log and the UI.
func (s TransferSummary) Describe() string {
	if s.Empty() {
		return "nothing to transfer"
	}
	var parts []string
	if s.PackTags > 0 {
		parts = append(parts, fmt.Sprintf("%d pack tag(s)", s.PackTags))
	}
	if s.AssetTags > 0 {
		parts = append(parts, fmt.Sprintf("%d asset tag(s)", s.AssetTags))
	}
	if len(s.Provenance) > 0 {
		parts = append(parts, fmt.Sprintf("provenance fields: %v", s.Provenance))
	}
	return "transferred " + joinComma(parts)
}

// provenanceFields are the pack columns a transfer can fill. Deliberately a list
// rather than "every column": ids, timestamps and the archive record belong to the
// pack that was actually downloaded, and copying those would invent history.
var provenanceFields = []string{
	"source_url", "source_site", "source_author", "source_author_url",
	"license_note", "attribution_text", "currency", "order_ref", "notes",
}

// TransferCuration copies tags and provenance from one pack onto another, filling
// gaps only — it never overwrites something the target already has.
//
// §9.1 requires this before a subset pack is removed, "so nothing curated is
// lost". It runs inside a transaction: a half-transferred pack followed by a
// removal is exactly the data loss the rule exists to prevent.
func TransferCuration(ctx context.Context, database *db.DB, fromPackID, toPackID int64) (TransferSummary, error) {
	var summary TransferSummary
	if fromPackID == toPackID {
		return summary, fmt.Errorf("cannot transfer a pack onto itself")
	}

	tx, err := database.Writer.BeginTx(ctx, nil)
	if err != nil {
		return summary, fmt.Errorf("begin transfer: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful commit

	now := time.Now().Unix()

	// Pack tags: insert what the target does not already carry.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO pack_tags (pack_id, tag_id, source, created_at)
		SELECT ?, tag_id, source, ?
		FROM pack_tags
		WHERE pack_id = ?
		ON CONFLICT(pack_id, tag_id) DO NOTHING`, toPackID, now, fromPackID)
	if err != nil {
		return summary, fmt.Errorf("transfer pack tags: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		summary.PackTags = int(n)
	}

	// Asset tags follow the content, not the path: a manual tag on the subset's
	// hero.png belongs on whichever file in the container has the same bytes. Only
	// manual tags move — the automatic ones are recomputed by the next scan.
	res, err = tx.ExecContext(ctx, `
		INSERT INTO asset_tags (asset_id, tag_id, source, created_by, created_at)
		SELECT target.id, t.tag_id, t.source, t.created_by, ?
		FROM asset_tags t
		JOIN assets source ON source.id = t.asset_id AND source.pack_id = ?
		JOIN assets target ON target.sha256 = source.sha256 AND target.pack_id = ?
		WHERE t.source = 'manual'
		ON CONFLICT(asset_id, tag_id) DO NOTHING`, now, fromPackID, toPackID)
	if err != nil {
		return summary, fmt.Errorf("transfer asset tags: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil {
		summary.AssetTags = int(n)
	}

	// A manually tagged file with no content match in the container would lose its
	// tag, so it is counted and reported rather than passed over.
	if err := tx.QueryRowContext(ctx, `
		SELECT count(DISTINCT source.id)
		FROM asset_tags t
		JOIN assets source ON source.id = t.asset_id AND source.pack_id = ?
		WHERE t.source = 'manual'
		  AND NOT EXISTS (SELECT 1 FROM assets target
		                  WHERE target.pack_id = ? AND target.sha256 = source.sha256)`,
		fromPackID, toPackID).Scan(&summary.Unmatched); err != nil {
		return summary, fmt.Errorf("count unmatched tagged assets: %w", err)
	}

	// Provenance, gap-filling only.
	for _, field := range provenanceFields {
		//nolint:gosec // field comes from the package-level allow-list above, never from input
		res, err := tx.ExecContext(ctx, fmt.Sprintf(`
			UPDATE packs SET %s = (SELECT %s FROM packs WHERE id = ?), updated_at = ?
			WHERE id = ? AND %s = ''
			  AND (SELECT %s FROM packs WHERE id = ?) != ''`,
			field, field, field, field), fromPackID, now, toPackID, fromPackID)
		if err != nil {
			return summary, fmt.Errorf("transfer %s: %w", field, err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			summary.Provenance = append(summary.Provenance, field)
		}
	}

	// The licence is the one that matters most, and it is an id rather than text.
	res, err = tx.ExecContext(ctx, `
		UPDATE packs SET license_id = (SELECT license_id FROM packs WHERE id = ?),
		                 attribution_required = (SELECT attribution_required FROM packs WHERE id = ?),
		                 updated_at = ?
		WHERE id = ? AND license_id IS NULL
		  AND (SELECT license_id FROM packs WHERE id = ?) IS NOT NULL`,
		fromPackID, fromPackID, now, toPackID, fromPackID)
	if err != nil {
		return summary, fmt.Errorf("transfer licence: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		summary.Provenance = append(summary.Provenance, "license")
	}

	// If the source had complete provenance and the target was still waiting for it,
	// the target now has the same facts and can say so.
	if _, err := tx.ExecContext(ctx, `
		UPDATE packs SET provenance_state = 'complete', updated_at = ?
		WHERE id = ? AND provenance_state = 'needs_provenance'
		  AND (SELECT provenance_state FROM packs WHERE id = ?) = 'complete'
		  AND source_url != '' AND license_id IS NOT NULL`,
		now, toPackID, fromPackID); err != nil {
		return summary, fmt.Errorf("transfer provenance state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return summary, fmt.Errorf("commit transfer: %w", err)
	}
	return summary, nil
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func joinComma(parts []string) string {
	switch len(parts) {
	case 0:
		return ""
	case 1:
		return parts[0]
	}
	out := parts[0]
	for _, p := range parts[1:] {
		out += ", " + p
	}
	return out
}

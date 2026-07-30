// Package dupes finds the four distinct relationships of spec §9.1 and reports
// them. Like internal/junk it detects and explains; it never removes anything.
// The removal path is internal/removal, and the split is deliberate: this package
// contains no code that can move or delete a file.
//
// §9.1 is explicit that conflating the four makes the feature useless, so they
// stay separate all the way to the UI:
//
//  1. Exact duplicates — identical sha256. Unambiguous, and the only kind safe to
//     act on directly.
//  2. Moved files — identical sha256 where the old path is gone. A move, not a
//     duplicate. Never reported: only assets the last scan still found are
//     considered, so a path that vanished stopped being a copy of anything.
//  3. Near-duplicate images — close phash, different bytes. Frequently
//     intentional (@2x exports, different tile sizes), so these are reported as
//     "review these" with no bulk action and no keep hint.
//  4. Format variants — the PNG/PSD/ASEPRITE of one artwork. Not duplicates at
//     all (invariant 7). The asset-group model is consumed here so variants of one
//     group are never paired against each other.
//
// Findings are resolved at pack level first (§9.1: a pack downloaded twice
// produces four hundred file rows and no actionable insight), then at file level,
// and everything is sorted by reclaimable bytes — largest win first.
package dupes

import (
	"context"
	"fmt"
	"math/bits"
	"sort"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/db"
)

// Kind labels a finding type.
type Kind string

const (
	// KindExact is a set of live assets sharing one sha256.
	KindExact Kind = "exact"
	// KindNear is a cluster of images with close perceptual hashes.
	KindNear Kind = "near"
	// KindPackIdentical is two packs with the same content hash set.
	KindPackIdentical Kind = "pack_identical"
	// KindPackSubset is a pack whose hashes are a strict subset of another's — the
	// CraftPix free-sample-then-full-purchase case §9.1 calls out.
	KindPackSubset Kind = "pack_subset"
	// KindPackOverlap is a high-similarity pair where neither contains the other.
	KindPackOverlap Kind = "pack_overlap"
)

// Title is a short human label.
func (k Kind) Title() string {
	switch k {
	case KindExact:
		return "Exact duplicates"
	case KindNear:
		return "Near-duplicate images"
	case KindPackIdentical:
		return "Identical packs"
	case KindPackSubset:
		return "Packs contained in another pack"
	case KindPackOverlap:
		return "Overlapping packs"
	default:
		return string(k)
	}
}

// Explanation says what the kind means and how far it can be trusted.
func (k Kind) Explanation() string {
	switch k {
	case KindExact:
		return "Byte-identical files. Removing all but one reclaims the space with no loss; " +
			"linking reclaims the same space and keeps every path working."
	case KindNear:
		return "Visually similar images with different bytes — often deliberate (@2x exports, " +
			"different tile sizes). Review these; they are not proposed for removal."
	case KindPackIdentical:
		return "Two packs with the same content. Pick which one goes, if either."
	case KindPackSubset:
		return "Every file in the smaller pack also exists in the larger one. Removing the " +
			"smaller pack transfers its tags and provenance onto the larger one first."
	case KindPackOverlap:
		return "Substantial overlap, but each pack has files the other does not. Reported for " +
			"information; there is no clean answer."
	default:
		return ""
	}
}

// Actionable reports whether a finding kind may offer removal controls at all.
// Near-duplicates never do (§9.1: "Report as 'review these', never as 'delete
// these'"), and neither does an overlapping pack pair.
func (k Kind) Actionable() bool {
	return k == KindExact || k == KindPackIdentical || k == KindPackSubset
}

// Copy is one live file in a finding, carrying the §9.1 keep-policy annotations:
// "Alongside each copy, show the facts that would inform a choice". They are facts
// and one hint, never a selection.
type Copy struct {
	AssetID  int64  `json:"asset_id"`
	Path     string `json:"path"` // library-relative, slash-separated
	PackID   int64  `json:"pack_id"`
	PackName string `json:"pack_name"`
	PackPath string `json:"pack_path"`
	Bytes    int64  `json:"bytes"`
	Sha      string `json:"sha256"`

	// --- the annotations (§9.1) ---

	// InRaw is whether the copy sits under a `raw/` segment: the staging area, so a
	// copy outside it is usually the curated one.
	InRaw bool `json:"in_raw"`
	// Depth is how many path segments deep the file is.
	Depth int `json:"depth"`
	// ProvenanceComplete is whether the owning pack has its §9 provenance filled in.
	ProvenanceComplete bool `json:"provenance_complete"`
	// FirstSeenAt is when Ambar first indexed this copy.
	FirstSeenAt int64 `json:"first_seen_at"`
	// VariantCount and IsGroupPrimary describe the §5.1 asset group: a copy that is
	// the primary of a multi-format group has source variants attached to it, which
	// is a reason to keep that one rather than its twin elsewhere.
	VariantCount   int  `json:"variant_count"`
	IsGroupPrimary bool `json:"is_group_primary"`
	// ProjectUses names the Godot projects that use this copy. Non-empty means the
	// copy can never be removed (invariant 5) — not a hint, a hard block.
	ProjectUses []string `json:"project_uses,omitempty"`

	// Favoured marks the copy the heuristics would keep. §9.1: "Label the copy the
	// heuristics would favour, as a hint. Never let that hint select anything."
	Favoured bool `json:"favoured"`
	// FavouredWhy explains the hint in words, so it can be argued with.
	FavouredWhy string `json:"favoured_why,omitempty"`
}

// Blocked reports whether this copy is off-limits for removal.
func (c Copy) Blocked() bool { return len(c.ProjectUses) > 0 }

// BlockedBy is the project list as a sentence.
func (c Copy) BlockedBy() string { return strings.Join(c.ProjectUses, ", ") }

// ExactFinding is one content hash with more than one live copy.
type ExactFinding struct {
	Sha    string `json:"sha256"`
	Size   int64  `json:"size"`
	Copies []Copy `json:"copies"`
	// Bytes is what removing (or linking) all but one copy reclaims.
	Bytes int64 `json:"bytes"`
}

// ID is a stable identifier for the finding, used as the finding label recorded in
// the trash record and the audit log.
func (f ExactFinding) ID() string { return "dupes:exact:" + f.Sha }

// Count is how many copies exist.
func (f ExactFinding) Count() int { return len(f.Copies) }

// NearFinding is a cluster of visually similar images. Review only.
type NearFinding struct {
	Copies []Copy `json:"copies"`
	// MaxDistance is the widest Hamming distance inside the cluster, so a reviewer
	// can see how loose the match is.
	MaxDistance int `json:"max_distance"`
	// Bytes is the size of all but the largest copy: what *would* be reclaimed if
	// they turned out to be redundant. Informational — nothing here is proposed.
	Bytes int64 `json:"bytes"`
}

// ID is a stable identifier for the cluster.
func (f NearFinding) ID() string {
	if len(f.Copies) == 0 {
		return "dupes:near"
	}
	return "dupes:near:" + strconv.FormatInt(f.Copies[0].AssetID, 10)
}

// Count is how many images are in the cluster.
func (f NearFinding) Count() int { return len(f.Copies) }

// Report is everything the detector found, each section sorted by reclaimable
// bytes, largest win first (§9.1).
type Report struct {
	Packs []PackFinding  `json:"packs"`
	Exact []ExactFinding `json:"exact"`
	Near  []NearFinding  `json:"near"`
	// Notes record what the scan deliberately did not do, so a truncated report
	// never reads as a complete one.
	Notes []string `json:"notes,omitempty"`
	Stats Stats    `json:"stats"`
}

// Stats is the summary line.
type Stats struct {
	Assets        int `json:"assets"`
	Packs         int `json:"packs"`
	ExactFindings int `json:"exact_findings"`
	NearClusters  int `json:"near_clusters"`
	PackFindings  int `json:"pack_findings"`

	// The two reclaimable figures are deliberately kept apart and never added
	// together. A pack that is contained in another pack is *also* a set of exact
	// duplicate files, so summing them would double-count the same bytes and promise
	// twice the space that exists — the one number §9.1 says makes the view worth
	// opening would be the one number that is wrong.
	//
	// Acting at pack level and then at file level reclaims PackReclaimableBytes;
	// working purely file by file reclaims ExactReclaimableBytes. Whichever route the
	// user takes, the honest headline is the larger of the two.
	PackReclaimableBytes  int64 `json:"pack_reclaimable_bytes"`
	ExactReclaimableBytes int64 `json:"exact_reclaimable_bytes"`
}

// ReclaimableBytes is the most that can be reclaimed, without double-counting the
// bytes a pack finding and its member file findings have in common.
func (s Stats) ReclaimableBytes() int64 {
	if s.PackReclaimableBytes > s.ExactReclaimableBytes {
		return s.PackReclaimableBytes
	}
	return s.ExactReclaimableBytes
}

// Empty reports whether nothing was found at all.
func (r *Report) Empty() bool {
	return len(r.Packs) == 0 && len(r.Exact) == 0 && len(r.Near) == 0
}

// Options tunes the detector. The defaults are what the deployment uses; they are
// options because the right near-duplicate threshold depends on a library, and
// because a test needs to force the caps.
type Options struct {
	// NearDistance is the maximum Hamming distance between two 64-bit perceptual
	// hashes for them to count as near-duplicates. 5 of 64 bits is tight enough that
	// a re-export or a resize matches while different artwork does not.
	NearDistance int
	// NearMaxAssets caps the near-duplicate pass, which is quadratic. Above the cap
	// the pass is skipped and the report says so rather than running for minutes
	// inside a job.
	NearMaxAssets int
	// PackJaccard is the minimum similarity for an overlapping pack pair to be worth
	// reporting at all.
	PackJaccard float64
	// MaxFindings caps each section, so one pathological library cannot produce a
	// report that no browser will render. What was dropped is recorded in Notes.
	MaxFindings int
}

// DefaultOptions is what the job uses.
func DefaultOptions() Options {
	return Options{NearDistance: 5, NearMaxAssets: 40_000, PackJaccard: 0.5, MaxFindings: 500}
}

func (o Options) withDefaults() Options {
	d := DefaultOptions()
	if o.NearDistance <= 0 {
		o.NearDistance = d.NearDistance
	}
	if o.NearMaxAssets <= 0 {
		o.NearMaxAssets = d.NearMaxAssets
	}
	if o.PackJaccard <= 0 {
		o.PackJaccard = d.PackJaccard
	}
	if o.MaxFindings <= 0 {
		o.MaxFindings = d.MaxFindings
	}
	return o
}

// Detector reads the index and produces a Report. It holds no state between
// scans.
type Detector struct {
	db   *db.DB
	opts Options
}

// NewDetector wraps a database.
func NewDetector(database *db.DB, opts Options) *Detector {
	return &Detector{db: database, opts: opts.withDefaults()}
}

// Scan produces the whole report: packs first, then exact duplicates, then the
// review-only near-duplicate clusters.
func (d *Detector) Scan(ctx context.Context) (*Report, error) {
	snap, err := d.load(ctx)
	if err != nil {
		return nil, err
	}

	report := &Report{Stats: Stats{Assets: len(snap.assets), Packs: len(snap.packs)}}
	report.Packs = d.packFindings(snap, report)
	report.Exact = d.exactFindings(snap, report)
	report.Near = d.nearFindings(snap, report)

	report.Stats.ExactFindings = len(report.Exact)
	report.Stats.NearClusters = len(report.Near)
	report.Stats.PackFindings = len(report.Packs)
	for _, f := range report.Exact {
		report.Stats.ExactReclaimableBytes += f.Bytes
	}
	for _, f := range report.Packs {
		if f.Kind.Actionable() {
			report.Stats.PackReclaimableBytes += f.Bytes
		}
	}
	return report, nil
}

// exactFindings groups live assets by content hash. §9.1 rule 1: unambiguous.
//
// Format variants (rule 4) cannot appear here: a PNG and its PSD have different
// bytes, so they never share a hash. Two byte-identical files inside one asset
// group are genuinely the same file stored twice, which is a duplicate whatever
// folder it sits in — so the group model is consumed for the near-duplicate pass
// and the keep hints, not to suppress an exact match.
func (d *Detector) exactFindings(snap *snapshot, report *Report) []ExactFinding {
	bySha := map[string][]*asset{}
	for _, a := range snap.assets {
		bySha[a.sha] = append(bySha[a.sha], a)
	}

	var findings []ExactFinding
	for sha, group := range bySha {
		if len(group) < 2 {
			continue
		}
		sort.Slice(group, func(i, j int) bool { return group[i].path < group[j].path })

		finding := ExactFinding{Sha: sha, Size: group[0].size}
		for _, a := range group {
			finding.Copies = append(finding.Copies, snap.copyOf(a))
		}
		annotateFavoured(finding.Copies)
		// Reclaimable: every copy but one.
		finding.Bytes = finding.Size * int64(len(group)-1)
		findings = append(findings, finding)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Bytes != findings[j].Bytes {
			return findings[i].Bytes > findings[j].Bytes
		}
		return findings[i].Sha < findings[j].Sha
	})
	return capFindings(findings, d.opts.MaxFindings, "exact duplicate", report)
}

// nearFindings clusters images by perceptual hash. Review only: §9.1 rule 3 is
// emphatic that these are frequently intentional.
func (d *Detector) nearFindings(snap *snapshot, report *Report) []NearFinding {
	var pool []*asset
	for _, a := range snap.assets {
		if !a.hasPhash {
			continue
		}
		pool = append(pool, a)
	}
	if len(pool) < 2 {
		return nil
	}
	if len(pool) > d.opts.NearMaxAssets {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"near-duplicate detection skipped: %d images have a perceptual hash, above the %d limit",
			len(pool), d.opts.NearMaxAssets))
		return nil
	}
	sort.Slice(pool, func(i, j int) bool { return pool[i].path < pool[j].path })

	// Union-find over the pairs within the threshold. Clustering rather than pairing
	// keeps three renders of one sprite as one finding instead of three.
	parent := make([]int, len(pool))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	union := func(i, j int) {
		ri, rj := find(i), find(j)
		if ri != rj {
			parent[rj] = ri
		}
	}

	widest := map[int]int{} // cluster root -> widest distance seen
	for i := 0; i < len(pool); i++ {
		for j := i + 1; j < len(pool); j++ {
			a, b := pool[i], pool[j]
			// Byte-identical files are an exact finding, not a near one.
			if a.sha == b.sha {
				continue
			}
			// Invariant 7: two variants of one asset group are one artwork in two
			// formats, never redundant copies of each other.
			if a.groupID != 0 && a.groupID == b.groupID {
				continue
			}
			dist := bits.OnesCount64(a.phash ^ b.phash)
			if dist > d.opts.NearDistance {
				continue
			}
			union(i, j)
			root := find(i)
			if dist > widest[root] {
				widest[root] = dist
			}
		}
	}

	// Group by root. A root with one member never matched anything and is dropped
	// below: a cluster of one is not a finding.
	clusters := map[int][]*asset{}
	for i, a := range pool {
		root := find(i)
		clusters[root] = append(clusters[root], a)
	}

	var findings []NearFinding
	for root, members := range clusters {
		if len(members) < 2 {
			continue
		}
		finding := NearFinding{MaxDistance: widest[root]}
		var largest int64
		for _, a := range members {
			finding.Copies = append(finding.Copies, snap.copyOf(a))
			finding.Bytes += a.size
			if a.size > largest {
				largest = a.size
			}
		}
		// What all but the biggest occupy — informational, since nothing here is
		// proposed for removal. No keep hint either: §9.1 wants no nudge at all.
		finding.Bytes -= largest
		sort.Slice(finding.Copies, func(i, j int) bool { return finding.Copies[i].Path < finding.Copies[j].Path })
		findings = append(findings, finding)
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Bytes != findings[j].Bytes {
			return findings[i].Bytes > findings[j].Bytes
		}
		return findings[i].ID() < findings[j].ID()
	})
	return capFindings(findings, d.opts.MaxFindings, "near-duplicate cluster", report)
}

// capFindings truncates a section and records what was dropped. A silent cap
// would read as "this is everything", which for a cleanup report is a lie worth
// avoiding.
func capFindings[T any](findings []T, max int, label string, report *Report) []T {
	if len(findings) <= max {
		return findings
	}
	report.Notes = append(report.Notes, fmt.Sprintf(
		"showing the %d largest %s findings of %d; the rest are not listed", max, label, len(findings)))
	return findings[:max]
}

// annotateFavoured marks the copy the keep heuristics prefer, with the reason in
// words. It sets a flag and a sentence — nothing else in the codebase reads
// Favoured to decide anything, which is the point (§9.1).
func annotateFavoured(copies []Copy) {
	if len(copies) < 2 {
		return
	}
	best, bestScore := -1, 0
	for i, c := range copies {
		score := keepScore(c)
		if best == -1 || score > bestScore ||
			(score == bestScore && copies[i].FirstSeenAt < copies[best].FirstSeenAt) {
			best, bestScore = i, score
		}
	}
	copies[best].Favoured = true
	copies[best].FavouredWhy = keepReason(copies[best])
}

// keepScore ranks a copy by the §9.1 annotation facts. The weights are a hint's
// weights: they decide which row gets a label, and nothing more.
func keepScore(c Copy) int {
	score := 0
	if c.Blocked() {
		// A project uses it, so it cannot be removed anyway. Making it the favoured
		// keeper means the hint agrees with the hard block instead of fighting it.
		score += 1000
	}
	if c.ProvenanceComplete {
		score += 100
	}
	if !c.InRaw {
		score += 50
	}
	if c.IsGroupPrimary && c.VariantCount > 1 {
		score += 25
	}
	// Shallower paths are usually the curated location.
	score -= c.Depth
	return score
}

func keepReason(c Copy) string {
	var why []string
	if c.Blocked() {
		why = append(why, "used by project "+c.BlockedBy())
	}
	if c.ProvenanceComplete {
		why = append(why, "its pack has complete provenance")
	}
	if !c.InRaw {
		why = append(why, "outside raw/")
	}
	if c.IsGroupPrimary && c.VariantCount > 1 {
		why = append(why, fmt.Sprintf("%d source variants depend on it", c.VariantCount-1))
	}
	if len(why) == 0 {
		return "nothing distinguishes the copies; this is simply the shallowest path"
	}
	return strings.Join(why, "; ")
}

// Kind names the finding type, so a template rendering a list of findings can ask
// for its title and explanation without the handler restating which section it is
// in.
func (f ExactFinding) Kind() Kind { return KindExact }

// Kind is ExactFinding.Kind for the review-only clusters.
func (f NearFinding) Kind() Kind { return KindNear }

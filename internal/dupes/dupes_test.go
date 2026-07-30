package dupes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// fixture builds an index directly. The detector reads nothing but the database,
// so no library tree is needed here — that is internal/removal's business.
type fixture struct {
	t   *testing.T
	db  *db.DB
	now int64
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "dupes.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, db: database, now: time.Unix(1_700_000_000, 0).Unix()}
}

// pack inserts a pack and returns its id. state is "complete" or
// "needs_provenance".
func (f *fixture) pack(relPath, state string) int64 {
	f.t.Helper()
	res, err := f.db.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, provenance_state,
		                   first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'folder', ?, ?, ?, ?, ?, ?)`,
		filepath.Base(relPath), filepath.Base(relPath), relPath, state,
		f.now, f.now, f.now, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

// assetOpts are the fields a test cares about; everything else gets a default.
type assetOpts struct {
	content string
	size    int64
	phash   string
	groupID int64
	primary bool
}

func (f *fixture) asset(packID int64, relPath string, opts assetOpts) int64 {
	f.t.Helper()
	if opts.content == "" {
		opts.content = relPath // unique by default
	}
	if opts.size == 0 {
		opts.size = 1000
	}
	sum := sha256.Sum256([]byte(opts.content))

	var phash any
	if opts.phash != "" {
		phash = opts.phash
	}
	var groupID any
	if opts.groupID != 0 {
		groupID = opts.groupID
	}
	res, err := f.db.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    phash, group_id, first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', 'image', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		packID, relPath, filepath.Base(relPath), opts.size, f.now,
		hex.EncodeToString(sum[:]), phash, groupID, f.now, f.now, f.now, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	if opts.primary {
		f.setPrimary(opts.groupID, id)
	}
	return id
}

// group creates an asset group with the given variant count (§5.1).
func (f *fixture) group(packID int64, key string, variants int) int64 {
	f.t.Helper()
	res, err := f.db.Writer.Exec(`
		INSERT INTO asset_groups (pack_id, group_key, variant_count, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, packID, key, variants, f.now, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *fixture) setPrimary(groupID, assetID int64) {
	f.t.Helper()
	if _, err := f.db.Writer.Exec(
		`UPDATE asset_groups SET primary_asset_id = ? WHERE id = ?`, assetID, groupID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) markMissing(assetID int64) {
	f.t.Helper()
	if _, err := f.db.Writer.Exec(
		`UPDATE assets SET missing_since = ? WHERE id = ?`, f.now, assetID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) useInProject(assetID int64, name string) {
	f.t.Helper()
	res, err := f.db.Writer.Exec(`
		INSERT INTO projects (uuid, name, created_at, updated_at) VALUES (?, ?, ?, ?)`,
		"uuid-"+name, name, f.now, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	projectID, _ := res.LastInsertId()
	if _, err := f.db.Writer.Exec(`
		INSERT INTO project_uses (project_id, asset_id, res_path, added_at)
		VALUES (?, ?, 'res://x.png', ?)`, projectID, assetID, f.now); err != nil {
		f.t.Fatal(err)
	}
}

// tag attaches a manual tag to an asset, creating the tag if needed. name is
// "namespace:name", e.g. "style:pixel".
func (f *fixture) tag(assetID int64, name string) {
	f.t.Helper()
	namespace, local, found := strings.Cut(name, ":")
	if !found {
		namespace, local = "", name
	}
	if _, err := f.db.Writer.Exec(`
		INSERT INTO tags (namespace, name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(namespace, name) DO UPDATE SET updated_at = excluded.updated_at`,
		namespace, local, f.now, f.now); err != nil {
		f.t.Fatal(err)
	}
	var tagID int64
	if err := f.db.Reader.QueryRow(
		`SELECT id FROM tags WHERE namespace = ? AND name = ?`, namespace, local).Scan(&tagID); err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`
		INSERT INTO asset_tags (asset_id, tag_id, source, created_at) VALUES (?, ?, 'manual', ?)
		ON CONFLICT(asset_id, tag_id) DO NOTHING`, assetID, tagID, f.now); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) scan(opts Options) *Report {
	f.t.Helper()
	report, err := NewDetector(f.db, opts).Scan(context.Background())
	if err != nil {
		f.t.Fatalf("scan: %v", err)
	}
	return report
}

// --- exact duplicates (§9.1 rule 1) -----------------------------------------

func TestExactDuplicatesAreReportedWithReclaimableBytes(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	f.asset(p, "a/hero.png", assetOpts{content: "hero", size: 2048})
	f.asset(p, "b/hero.png", assetOpts{content: "hero", size: 2048})
	f.asset(p, "c/hero.png", assetOpts{content: "hero", size: 2048})
	f.asset(p, "unique.png", assetOpts{})

	report := f.scan(Options{})

	if len(report.Exact) != 1 {
		t.Fatalf("want 1 exact finding, got %d: %+v", len(report.Exact), report.Exact)
	}
	finding := report.Exact[0]
	if finding.Count() != 3 {
		t.Errorf("want 3 copies, got %d", finding.Count())
	}
	// Reclaimable is every copy but one — never all of them.
	if finding.Bytes != 4096 {
		t.Errorf("reclaimable = %d, want 4096 (two of three copies)", finding.Bytes)
	}
	if report.Stats.ReclaimableBytes() != 4096 || report.Stats.ExactReclaimableBytes != 4096 {
		t.Errorf("stats reclaimable = %d", report.Stats.ReclaimableBytes())
	}
}

func TestExactFindingsAreSortedByLargestWin(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	f.asset(p, "small-1.png", assetOpts{content: "small", size: 10})
	f.asset(p, "small-2.png", assetOpts{content: "small", size: 10})
	f.asset(p, "big-1.png", assetOpts{content: "big", size: 5000})
	f.asset(p, "big-2.png", assetOpts{content: "big", size: 5000})

	report := f.scan(Options{})
	if len(report.Exact) != 2 {
		t.Fatalf("want 2 findings, got %d", len(report.Exact))
	}
	if report.Exact[0].Bytes < report.Exact[1].Bytes {
		t.Errorf("findings must be sorted largest win first, got %d then %d",
			report.Exact[0].Bytes, report.Exact[1].Bytes)
	}
}

// §9.1 rule 2: a move is not a duplicate.
func TestMovedFilesAreNotDuplicates(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	old := f.asset(p, "old/place/hero.png", assetOpts{content: "hero"})
	f.asset(p, "new/place/hero.png", assetOpts{content: "hero"})
	// The scan found the file at its new path and marked the old row missing. That
	// is a move, and reporting it as a duplicate would invite deleting the only copy.
	f.markMissing(old)

	report := f.scan(Options{})
	if len(report.Exact) != 0 {
		t.Errorf("a moved file must not be reported as a duplicate: %+v", report.Exact)
	}
}

// §9.1 rule 4 / invariant 7: format variants are one asset, not duplicates. They
// have different bytes, so they cannot collide on sha256 — but they must not
// collide in the near-duplicate pass either.
func TestFormatVariantsOfOneGroupAreNeverNearDuplicates(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	g := f.group(p, "Plant1/idle", 3)
	// Same artwork, three formats: identical perceptual hash, different bytes.
	png := f.asset(p, "PNG/Plant1/idle.png", assetOpts{content: "png-bytes", phash: "ffffffffffffffff", groupID: g, primary: true})
	f.asset(p, "PSD/Plant1/idle.psd", assetOpts{content: "psd-bytes", phash: "ffffffffffffffff", groupID: g})
	f.asset(p, "ASEPRITE/Plant1/idle.aseprite", assetOpts{content: "ase-bytes", phash: "ffffffffffffffff", groupID: g})

	report := f.scan(Options{})

	if len(report.Near) != 0 {
		t.Fatalf("variants of one group must never be paired: %+v", report.Near)
	}
	if len(report.Exact) != 0 {
		t.Errorf("different formats have different bytes: %+v", report.Exact)
	}
	_ = png
}

// --- near duplicates (§9.1 rule 3) ------------------------------------------

func TestNearDuplicatesClusterAndAreReviewOnly(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	// Three re-exports of one sprite: hashes within a few bits of each other.
	f.asset(p, "hero.png", assetOpts{content: "one", phash: "0000000000000000", size: 100})
	f.asset(p, "hero@2x.png", assetOpts{content: "two", phash: "0000000000000003", size: 400})
	f.asset(p, "hero@4x.png", assetOpts{content: "three", phash: "0000000000000007", size: 900})
	// Different artwork entirely: half the bits differ.
	f.asset(p, "tree.png", assetOpts{content: "four", phash: "ffffffff00000000", size: 100})

	report := f.scan(Options{NearDistance: 5})

	if len(report.Near) != 1 {
		t.Fatalf("want one cluster, got %d: %+v", len(report.Near), report.Near)
	}
	cluster := report.Near[0]
	if cluster.Count() != 3 {
		t.Errorf("want 3 members, got %d: %+v", cluster.Count(), cluster.Copies)
	}
	// Informational only: nothing here counts towards reclaimable space, because
	// nothing here is proposed for removal.
	if report.Stats.ReclaimableBytes() != 0 {
		t.Errorf("near-duplicates must not count as reclaimable, got %d", report.Stats.ReclaimableBytes())
	}
	// And no keep hint: §9.1 wants no nudge at all on these.
	for _, c := range cluster.Copies {
		if c.Favoured {
			t.Errorf("a near-duplicate must carry no keep hint: %+v", c)
		}
	}
	if KindNear.Actionable() {
		t.Error("KindNear must never be actionable")
	}
}

func TestNearDuplicatesRespectTheDistanceThreshold(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	f.asset(p, "a.png", assetOpts{content: "a", phash: "0000000000000000"})
	// Four bits apart.
	f.asset(p, "b.png", assetOpts{content: "b", phash: "000000000000000f"})

	if report := f.scan(Options{NearDistance: 3}); len(report.Near) != 0 {
		t.Errorf("distance 4 must not match at threshold 3: %+v", report.Near)
	}
	if report := f.scan(Options{NearDistance: 4}); len(report.Near) != 1 {
		t.Errorf("distance 4 must match at threshold 4: %+v", report.Near)
	}
}

func TestNearDuplicateScanIsCappedAndSaysSo(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	for i := 0; i < 4; i++ {
		f.asset(p, fmt.Sprintf("img-%d.png", i), assetOpts{
			content: fmt.Sprintf("c%d", i), phash: fmt.Sprintf("000000000000000%d", i)})
	}

	report := f.scan(Options{NearMaxAssets: 3})
	if len(report.Near) != 0 {
		t.Errorf("the pass must be skipped above the cap, got %+v", report.Near)
	}
	if len(report.Notes) == 0 {
		t.Fatal("a skipped pass must be recorded in Notes, never silently omitted")
	}
}

// A contained pack and its member files describe the same bytes. Adding the two
// figures would promise twice the space that exists, so the headline never sums
// them.
func TestPackAndFileFindingsDoNotDoubleCountTheSameBytes(t *testing.T) {
	f := newFixture(t)
	full := f.pack("2d/full", "complete")
	free := f.pack("raw/free", "needs_provenance")
	for _, name := range []string{"a.png", "b.png"} {
		f.asset(full, name, assetOpts{content: name, size: 1000})
		f.asset(free, name, assetOpts{content: name, size: 1000})
	}
	f.asset(full, "extra.png", assetOpts{content: "extra", size: 1000})

	report := f.scan(Options{})

	// 2000 bytes are redundant, and both routes reclaim exactly those.
	if report.Stats.PackReclaimableBytes != 2000 {
		t.Errorf("pack reclaimable = %d, want 2000", report.Stats.PackReclaimableBytes)
	}
	if report.Stats.ExactReclaimableBytes != 2000 {
		t.Errorf("exact reclaimable = %d, want 2000", report.Stats.ExactReclaimableBytes)
	}
	if got := report.Stats.ReclaimableBytes(); got != 2000 {
		t.Errorf("headline reclaimable = %d, want 2000 — the two figures must not be summed", got)
	}
}

func TestIdenticalBytesAreNotAlsoANearDuplicate(t *testing.T) {
	f := newFixture(t)
	p := f.pack("2d/pack", "complete")
	f.asset(p, "a.png", assetOpts{content: "same", phash: "0000000000000000"})
	f.asset(p, "b.png", assetOpts{content: "same", phash: "0000000000000000"})

	report := f.scan(Options{})
	if len(report.Exact) != 1 {
		t.Errorf("want the exact finding, got %+v", report.Exact)
	}
	if len(report.Near) != 0 {
		t.Errorf("byte-identical files belong in the exact section only: %+v", report.Near)
	}
}

// --- keep-policy annotations (§9.1) -----------------------------------------

func TestKeepHintFavoursTheCuratedCopyWithoutSelectingIt(t *testing.T) {
	f := newFixture(t)
	raw := f.pack("raw/craftpix-free-trees", "needs_provenance")
	curated := f.pack("2d/trees", "complete")
	f.asset(raw, "deep/nested/tree.png", assetOpts{content: "tree"})
	f.asset(curated, "tree.png", assetOpts{content: "tree"})

	report := f.scan(Options{})
	if len(report.Exact) != 1 {
		t.Fatalf("want 1 finding: %+v", report.Exact)
	}

	var favoured []string
	for _, c := range report.Exact[0].Copies {
		if c.Favoured {
			favoured = append(favoured, c.Path)
			if c.FavouredWhy == "" {
				t.Error("a hint must explain itself")
			}
		}
	}
	if len(favoured) != 1 || favoured[0] != "2d/trees/tree.png" {
		t.Errorf("the curated copy outside raw/ should carry the hint, got %v", favoured)
	}
	// The annotations themselves must be present, since they are what a person
	// actually decides on.
	for _, c := range report.Exact[0].Copies {
		if c.Path == "raw/craftpix-free-trees/deep/nested/tree.png" {
			// raw / craftpix-free-trees / deep / nested / tree.png
			if !c.InRaw || c.ProvenanceComplete || c.Depth != 5 {
				t.Errorf("annotations wrong: %+v", c)
			}
		}
	}
}

func TestProjectUseIsReportedAsABlockNotAHint(t *testing.T) {
	f := newFixture(t)
	a := f.pack("2d/a", "needs_provenance")
	b := f.pack("2d/b", "complete")
	used := f.asset(a, "hero.png", assetOpts{content: "hero"})
	f.asset(b, "hero.png", assetOpts{content: "hero"})
	f.useInProject(used, "Dungeon Game")

	report := f.scan(Options{})
	if len(report.Exact) != 1 {
		t.Fatalf("want 1 finding: %+v", report.Exact)
	}
	var blocked, hinted string
	for _, c := range report.Exact[0].Copies {
		if c.Blocked() {
			blocked = c.Path
			if c.BlockedBy() != "Dungeon Game" {
				t.Errorf("the block must name the project, got %q", c.BlockedBy())
			}
		}
		if c.Favoured {
			hinted = c.Path
		}
	}
	if blocked != "2d/a/hero.png" {
		t.Errorf("blocked copy = %q", blocked)
	}
	// The hint agrees with the hard block rather than proposing the impossible.
	if hinted != blocked {
		t.Errorf("the keep hint should favour the copy that cannot be removed, got %q", hinted)
	}
}

func TestGroupPrimaryWithSourceVariantsIsFavoured(t *testing.T) {
	f := newFixture(t)
	withSources := f.pack("2d/full", "needs_provenance")
	plain := f.pack("2d/plain", "needs_provenance")
	g := f.group(withSources, "hero", 3)
	f.asset(withSources, "PNG/hero.png", assetOpts{content: "hero", groupID: g, primary: true})
	f.asset(plain, "hero.png", assetOpts{content: "hero"})

	report := f.scan(Options{})
	if len(report.Exact) != 1 {
		t.Fatalf("want 1 finding: %+v", report.Exact)
	}
	for _, c := range report.Exact[0].Copies {
		if c.Path == "2d/full/PNG/hero.png" {
			if !c.Favoured {
				t.Errorf("the copy that source variants depend on should be favoured: %+v", c)
			}
			if !c.IsGroupPrimary || c.VariantCount != 3 {
				t.Errorf("group annotations wrong: %+v", c)
			}
		}
	}
}

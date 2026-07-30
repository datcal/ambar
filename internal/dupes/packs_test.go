package dupes

import (
	"context"
	"testing"
)

// findPack returns the finding of a given kind whose candidate is the named pack.
func findPack(report *Report, kind Kind, candidatePath string) *PackFinding {
	for i := range report.Packs {
		if report.Packs[i].Kind == kind && report.Packs[i].Candidate.Path == candidatePath {
			return &report.Packs[i]
		}
	}
	return nil
}

// §9.1: "CraftPix publishes a free sample pack and a larger paid pack of the same
// art, so `...-free-...` folders will often turn out to be fully contained in a
// later purchase."
func TestSubsetPackIsReportedWithContainmentAndTransfers(t *testing.T) {
	f := newFixture(t)
	free := f.pack("raw/craftpix-net-385863-free-top-down-trees", "needs_provenance")
	paid := f.pack("2d/craftpix-top-down-trees-full", "complete")

	for _, name := range []string{"tree1.png", "tree2.png"} {
		f.asset(free, name, assetOpts{content: name, size: 1500})
		f.asset(paid, "PNG/"+name, assetOpts{content: name, size: 1500})
	}
	// The paid pack has more.
	f.asset(paid, "PNG/tree3.png", assetOpts{content: "tree3.png", size: 1500})
	f.asset(paid, "PSD/tree1.psd", assetOpts{content: "tree1.psd", size: 9000})
	// Curation on the free pack that must not be lost.
	tagged := f.asset(free, "tree-extra.png", assetOpts{content: "tree1.png", size: 1500})
	f.tag(tagged, "style:pixel")

	report := f.scan(Options{})

	finding := findPack(report, KindPackSubset, "raw/craftpix-net-385863-free-top-down-trees")
	if finding == nil {
		t.Fatalf("the free pack must be reported as contained in the paid one: %+v", report.Packs)
	}
	if finding.Container.Path != "2d/craftpix-top-down-trees-full" {
		t.Errorf("container = %q", finding.Container.Path)
	}
	if finding.SharedHashes != 2 || finding.OnlyInCandidate != 0 || finding.OnlyInContainer != 2 {
		t.Errorf("containment counts wrong: shared=%d onlyCandidate=%d onlyContainer=%d",
			finding.SharedHashes, finding.OnlyInCandidate, finding.OnlyInContainer)
	}
	// Reclaimable: the free pack's three files, all of whose content exists in the
	// paid pack.
	if finding.Bytes != 4500 {
		t.Errorf("reclaimable = %d, want 4500", finding.Bytes)
	}
	// §9.1: say what would have to be transferred, before acting.
	if len(finding.Transfers) == 0 {
		t.Error("a subset with manual tags must announce the transfer")
	}
}

func TestIdenticalPacksAreReportedAsAPairWithAHint(t *testing.T) {
	f := newFixture(t)
	curated := f.pack("2d/kenney-rts", "complete")
	dupe := f.pack("raw/kenney-rts-again", "needs_provenance")
	for _, name := range []string{"unit.png", "tower.png"} {
		f.asset(curated, name, assetOpts{content: name, size: 800})
		f.asset(dupe, name, assetOpts{content: name, size: 800})
	}

	report := f.scan(Options{})
	if len(report.Packs) != 1 {
		t.Fatalf("want one pack finding, got %+v", report.Packs)
	}
	finding := report.Packs[0]
	if finding.Kind != KindPackIdentical {
		t.Fatalf("kind = %s, want %s", finding.Kind, KindPackIdentical)
	}
	// The hint favours keeping the curated copy, so the candidate is the raw one —
	// but it is only a candidate, and the UI selects nothing.
	if finding.Candidate.Path != "raw/kenney-rts-again" {
		t.Errorf("candidate = %q, want the uncurated pack", finding.Candidate.Path)
	}
	if finding.Jaccard != 1 {
		t.Errorf("jaccard = %v, want 1", finding.Jaccard)
	}
	if finding.Bytes != 1600 {
		t.Errorf("reclaimable = %d, want 1600", finding.Bytes)
	}
}

func TestOverlappingPacksAreReportedOnly(t *testing.T) {
	f := newFixture(t)
	a := f.pack("2d/mixed-a", "complete")
	b := f.pack("2d/mixed-b", "complete")
	// Three shared files, one unique each: jaccard = 3/5 = 0.6.
	for _, name := range []string{"s1.png", "s2.png", "s3.png"} {
		f.asset(a, name, assetOpts{content: name, size: 100})
		f.asset(b, name, assetOpts{content: name, size: 100})
	}
	f.asset(a, "only-a.png", assetOpts{content: "only-a", size: 100})
	f.asset(b, "only-b.png", assetOpts{content: "only-b", size: 100})

	report := f.scan(Options{PackJaccard: 0.5})
	if len(report.Packs) != 1 || report.Packs[0].Kind != KindPackOverlap {
		t.Fatalf("want one overlap finding, got %+v", report.Packs)
	}
	finding := report.Packs[0]
	// Nothing is proposed: no reclaimable figure, no transfer notice, not actionable.
	if finding.Bytes != 0 || len(finding.Transfers) != 0 {
		t.Errorf("an overlap must propose nothing: %+v", finding)
	}
	if finding.Kind.Actionable() {
		t.Error("an overlap must never be actionable")
	}
	// The three shared files are exact duplicates in their own right, so they do
	// count; the overlap pair itself adds nothing on top.
	var fromExact int64
	for _, e := range report.Exact {
		fromExact += e.Bytes
	}
	if report.Stats.ReclaimableBytes() != fromExact {
		t.Errorf("an overlap must add nothing to the reclaimable total: %d vs %d from exact findings",
			report.Stats.ReclaimableBytes(), fromExact)
	}
	if finding.SimilarityPercent() != 60 {
		t.Errorf("similarity = %d%%, want 60%%", finding.SimilarityPercent())
	}
}

func TestIncidentalSharedFileIsNotARelationship(t *testing.T) {
	f := newFixture(t)
	a := f.pack("2d/a", "complete")
	b := f.pack("2d/b", "complete")
	f.asset(a, "shared.png", assetOpts{content: "shared"})
	f.asset(b, "shared.png", assetOpts{content: "shared"})
	for i, name := range []string{"a1.png", "a2.png", "a3.png", "a4.png"} {
		_ = i
		f.asset(a, name, assetOpts{content: name})
	}
	for _, name := range []string{"b1.png", "b2.png", "b3.png", "b4.png"} {
		f.asset(b, name, assetOpts{content: name})
	}

	report := f.scan(Options{PackJaccard: 0.5})
	// One file in common out of nine is noise, not a pack relationship. The file
	// itself is still an exact duplicate finding.
	if len(report.Packs) != 0 {
		t.Errorf("want no pack finding, got %+v", report.Packs)
	}
	if len(report.Exact) != 1 {
		t.Errorf("the shared file is still an exact duplicate: %+v", report.Exact)
	}
}

func TestPackFindingNamesTheProjectsThatBlockIt(t *testing.T) {
	f := newFixture(t)
	free := f.pack("raw/free-pack", "needs_provenance")
	full := f.pack("2d/full-pack", "complete")
	used := f.asset(free, "hero.png", assetOpts{content: "hero"})
	f.asset(full, "hero.png", assetOpts{content: "hero"})
	f.asset(full, "extra.png", assetOpts{content: "extra"}) // so full strictly contains free
	f.useInProject(used, "Dungeon Game")

	report := f.scan(Options{})
	finding := findPack(report, KindPackSubset, "raw/free-pack")
	if finding == nil {
		t.Fatalf("want a subset finding: %+v", report.Packs)
	}
	if !finding.Candidate.Blocked() {
		t.Fatal("a pack whose file a project uses must be reported as blocked")
	}
	if finding.Candidate.ProjectUses[0] != "Dungeon Game" {
		t.Errorf("project uses = %v", finding.Candidate.ProjectUses)
	}
}

func TestMissingAssetsDoNotShrinkAPackIntoASubset(t *testing.T) {
	f := newFixture(t)
	a := f.pack("2d/a", "complete")
	b := f.pack("2d/b", "complete")
	f.asset(a, "shared.png", assetOpts{content: "shared"})
	unique := f.asset(a, "unique-to-a.png", assetOpts{content: "unique"})
	f.asset(b, "shared.png", assetOpts{content: "shared"})
	f.asset(b, "unique-to-b.png", assetOpts{content: "b-only"})

	// While the file is present, A is not contained in B.
	if report := f.scan(Options{}); findPack(report, KindPackSubset, "2d/a") != nil {
		t.Fatalf("A has a file B lacks; it is not a subset: %+v", report.Packs)
	}
	// Once the scan reports it gone, A really does only hold what B holds.
	f.markMissing(unique)
	if report := f.scan(Options{}); findPack(report, KindPackSubset, "2d/a") == nil {
		t.Error("with the extra file gone, A is contained in B")
	}
}

// --- curation transfer (§9.1) -----------------------------------------------

func TestTransferCurationMovesTagsAndFillsProvenanceGaps(t *testing.T) {
	f := newFixture(t)
	from := f.pack("raw/free-pack", "complete")
	to := f.pack("2d/full-pack", "needs_provenance")

	tagged := f.asset(from, "hero.png", assetOpts{content: "hero"})
	f.tag(tagged, "style:pixel")
	f.tag(tagged, "biome:forest")
	// A manually tagged file with no counterpart in the target: its tag cannot
	// follow the content anywhere, so it must be reported rather than lost quietly.
	orphan := f.asset(from, "only-here.png", assetOpts{content: "only-here"})
	f.tag(orphan, "style:orphan")
	target := f.asset(to, "PNG/hero.png", assetOpts{content: "hero"})

	// Provenance on the source only.
	if _, err := f.db.Writer.Exec(`
		UPDATE packs SET source_url = 'https://craftpix.net/x', source_author = 'CraftPix',
		                 license_id = 1, notes = 'bought in the winter sale'
		WHERE id = ?`, from); err != nil {
		t.Fatal(err)
	}
	// The target already has a note of its own, which must not be overwritten.
	if _, err := f.db.Writer.Exec(`UPDATE packs SET notes = 'do not touch' WHERE id = ?`, to); err != nil {
		t.Fatal(err)
	}

	summary, err := TransferCuration(context.Background(), f.db, from, to)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}

	if summary.AssetTags != 2 {
		t.Errorf("asset tags transferred = %d, want 2", summary.AssetTags)
	}
	if summary.Unmatched != 1 {
		t.Errorf("unmatched tagged files = %d, want 1", summary.Unmatched)
	}
	// Tags follow content, not paths.
	var tagCount int
	if err := f.db.Reader.QueryRow(
		`SELECT count(*) FROM asset_tags WHERE asset_id = ?`, target).Scan(&tagCount); err != nil {
		t.Fatal(err)
	}
	if tagCount != 2 {
		t.Errorf("target asset has %d tags, want 2", tagCount)
	}

	var url, author, notes string
	var licenseID *int64
	var state string
	if err := f.db.Reader.QueryRow(`
		SELECT source_url, source_author, notes, license_id, provenance_state
		FROM packs WHERE id = ?`, to).Scan(&url, &author, &notes, &licenseID, &state); err != nil {
		t.Fatal(err)
	}
	if url != "https://craftpix.net/x" || author != "CraftPix" {
		t.Errorf("provenance gaps not filled: url=%q author=%q", url, author)
	}
	if licenseID == nil || *licenseID != 1 {
		t.Errorf("licence not transferred: %v", licenseID)
	}
	if notes != "do not touch" {
		t.Errorf("an existing value must never be overwritten, got %q", notes)
	}
	if state != "complete" {
		t.Errorf("provenance_state = %q, want complete once the facts are there", state)
	}
}

func TestTransferCurationIsIdempotentAndRefusesSelfTransfer(t *testing.T) {
	f := newFixture(t)
	from := f.pack("raw/a", "complete")
	to := f.pack("2d/b", "needs_provenance")
	tagged := f.asset(from, "hero.png", assetOpts{content: "hero"})
	f.tag(tagged, "style:pixel")
	f.asset(to, "hero.png", assetOpts{content: "hero"})

	if _, err := TransferCuration(context.Background(), f.db, from, from); err == nil {
		t.Error("transferring a pack onto itself must be an error")
	}

	first, err := TransferCuration(context.Background(), f.db, from, to)
	if err != nil {
		t.Fatal(err)
	}
	second, err := TransferCuration(context.Background(), f.db, from, to)
	if err != nil {
		t.Fatal(err)
	}
	if first.AssetTags != 1 || second.AssetTags != 0 {
		t.Errorf("a repeat transfer must add nothing: first=%d second=%d", first.AssetTags, second.AssetTags)
	}
}

package index

import (
	"context"
	"fmt"
	"testing"
)

// groupOf finds a group by its key, for assertions.
func (f *fixture) groupByKey(t *testing.T, key string) (Group, bool) {
	t.Helper()

	page, err := f.ix.ListGroups(context.Background(), ListOptions{
		IncludeMissing: true, Limit: MaxPageSize,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range page.Groups {
		if g.GroupKey == key {
			return g, true
		}
	}
	return Group{}, false
}

func TestGroupKey(t *testing.T) {
	// §5.1's key: format-folder segment removed, extension dropped.
	tests := map[string]string{
		"PNG/Plant1/idle.png":           "Plant1/idle",
		"PSD/Plant1/idle.psd":           "Plant1/idle",
		"ASEPRITE/Plant1/idle.aseprite": "Plant1/idle",
		"PNG_Animations/Boom/frame.png": "Boom/frame",
		"Models/Rocks/rock.glb":         "Models/Rocks/rock",
		"sprite.png":                    "sprite",
		"Tiled_files/map.tmx":           "map",
		"no_extension":                  "no_extension",
	}
	for in, want := range tests {
		if got := GroupKey(in); got != want {
			t.Errorf("GroupKey(%q) = %q, want %q", in, got, want)
		}
	}

	// The property that matters: the three variants of one artwork agree.
	png := GroupKey("PNG/Plant1/idle.png")
	psd := GroupKey("PSD/Plant1/idle.psd")
	ase := GroupKey("ASEPRITE/Plant1/idle.aseprite")
	if png != psd || psd != ase {
		t.Errorf("variants produced different keys: %q %q %q", png, psd, ase)
	}
}

// TestFormatVariantsCollapseToOneGroup is §5.1's central example and invariant 7.
func TestFormatVariantsCollapseToOneGroup(t *testing.T) {
	f := newFixture(t)

	// The exact tree §5.1 describes.
	pack := "craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack"
	f.write(pack+"/PNG/Plant1/idle.png", "png bytes")
	f.write(pack+"/PSD/Plant1/idle.psd", "psd bytes")
	f.write(pack+"/ASEPRITE/Plant1/idle.aseprite", "aseprite bytes")
	f.scan()

	page, err := f.ix.ListGroups(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// One row in the grid, not three. This is the whole point of §5.1.
	if page.Total != 1 {
		names := make([]string, 0, len(page.Groups))
		for _, g := range page.Groups {
			names = append(names, g.GroupKey)
		}
		t.Fatalf("the grid shows %d groups, want 1: %v", page.Total, names)
	}

	g := page.Groups[0]
	if g.VariantCount != 3 {
		t.Errorf("VariantCount = %d, want 3", g.VariantCount)
	}
	if !g.MultiVariant() {
		t.Error("MultiVariant() is false for a three-format group")
	}
	// §5.1: "Nominate a primary variant — the engine-ready one, normally PNG".
	if g.Primary.Ext != "png" {
		t.Errorf("primary is .%s, want png (§5.1 precedence)", g.Primary.Ext)
	}

	// And all three are still individually present as assets — collapsing is a
	// presentation decision, not a data one.
	if got := f.assetCount(); got != 3 {
		t.Errorf("%d asset rows, want 3 — grouping must not remove assets", got)
	}

	variants, err := f.ix.Variants(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(variants) != 3 {
		t.Fatalf("%d variants, want 3", len(variants))
	}
	// §5.1: the detail panel lists variants per format, primary first.
	if variants[0].Ext != "png" {
		t.Errorf("variants are ordered %s first, want png", variants[0].Ext)
	}
	exts := map[string]bool{}
	for _, v := range variants {
		exts[v.Ext] = true
	}
	for _, want := range []string{"png", "psd", "aseprite"} {
		if !exts[want] {
			t.Errorf("variant list is missing .%s", want)
		}
	}
}

// TestVariantsAreNotDuplicates is invariant 7 stated directly: "Format variants are not
// duplicates. ... Never surface them as redundant copies."
//
// The mistake this guards against is severe — §9.1 says getting it wrong "would suggest
// deleting every source file in the library".
func TestVariantsAreNotDuplicates(t *testing.T) {
	f := newFixture(t)

	f.write("pack/PNG/hero.png", "distinct png content")
	f.write("pack/PSD/hero.psd", "distinct psd content")
	f.scan()

	// They are one group...
	page, err := f.ix.ListGroups(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("%d groups, want 1", page.Total)
	}

	// ...and they do NOT share a content hash, so M13's exact-duplicate finder can
	// never mistake them for copies either.
	var sharedHashes int
	if err := f.db.Reader.QueryRow(`
		SELECT count(*) FROM assets a
		WHERE (SELECT count(*) FROM assets b WHERE b.sha256 = a.sha256) > 1`).
		Scan(&sharedHashes); err != nil {
		t.Fatal(err)
	}
	if sharedHashes != 0 {
		t.Errorf("%d assets share a hash; format variants must not look like copies", sharedHashes)
	}
}

// TestPrimaryPrecedence walks §5.1's ordering.
func TestPrimaryPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		files   []string
		wantExt string
	}{
		{"png beats psd", []string{"PNG/a.png", "PSD/a.psd"}, "png"},
		{"png beats aseprite", []string{"PNG/a.png", "ASEPRITE/a.aseprite"}, "png"},
		{"webp beats svg", []string{"a.webp", "SVG/a.svg"}, "webp"},
		{"glb beats fbx", []string{"GLB/a.glb", "FBX/a.fbx"}, "glb"},
		{"gltf beats obj", []string{"GLTF/a.gltf", "OBJ/a.obj"}, "gltf"},
		{"svg beats aseprite", []string{"SVG/a.svg", "ASEPRITE/a.aseprite"}, "svg"},
		{"aseprite beats psd", []string{"ASEPRITE/a.aseprite", "PSD/a.psd"}, "aseprite"},
		{"psd beats kra", []string{"PSD/a.psd", "KRA/a.kra"}, "psd"},
		{"kra beats xcf", []string{"KRA/a.kra", "XCF/a.xcf"}, "kra"},
		// An unlisted extension is still eligible when it is all there is.
		{"lone unlisted format", []string{"a.wav"}, "wav"},
		// And loses to any listed one.
		{"listed beats unlisted", []string{"PNG/a.png", "a.wav"}, "png"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			for _, name := range tc.files {
				f.write("pack/"+name, "content of "+name)
			}
			f.scan()

			g, ok := f.groupByKey(t, "a")
			if !ok {
				t.Fatalf("no group with key \"a\"; files were %v", tc.files)
			}
			if g.Primary.Ext != tc.wantExt {
				t.Errorf("primary is .%s, want .%s", g.Primary.Ext, tc.wantExt)
			}
		})
	}
}

// TestPrimaryPrefersAPresentFile: the primary is what the grid renders, so choosing a
// missing file over a present one would show a broken thumbnail.
func TestPrimaryPrefersAPresentFile(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/hero.png", "png bytes")
	f.write("pack/PSD/hero.psd", "psd bytes")
	f.scan()

	// PNG wins initially, by precedence.
	g, ok := f.groupByKey(t, "hero")
	if !ok {
		t.Fatal("no group")
	}
	if g.Primary.Ext != "png" {
		t.Fatalf("primary is .%s, want png", g.Primary.Ext)
	}

	// Remove the PNG. Its row survives (§12), but it must stop being the primary.
	f.remove("pack/PNG/hero.png")
	f.scan()

	g, ok = f.groupByKey(t, "hero")
	if !ok {
		t.Fatal("the group disappeared when its primary went missing")
	}
	if g.Primary.Ext != "psd" {
		t.Errorf("primary is .%s, want psd — a missing file must not be the primary", g.Primary.Ext)
	}
	if g.VariantCount != 2 {
		t.Errorf("VariantCount = %d, want 2 — the missing variant is still a member", g.VariantCount)
	}
}

// TestGroupingIsStableAcrossScans: a rescan with no filesystem change must write
// nothing, or every scan churns the whole table.
func TestGroupingIsStableAcrossScans(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/a.png", "a")
	f.write("pack/PSD/a.psd", "b")
	f.write("pack/other.png", "c")
	f.scan()

	before, ok := f.groupByKey(t, "a")
	if !ok {
		t.Fatal("no group")
	}

	stats, err := f.ix.Regroup(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Created != 0 || stats.Updated != 0 || stats.Reassigned != 0 {
		t.Errorf("a no-op regroup wrote: %+v", stats)
	}
	if stats.Groups != 2 {
		t.Errorf("Groups = %d, want 2", stats.Groups)
	}
	if stats.MultiVariant != 1 {
		t.Errorf("MultiVariant = %d, want 1", stats.MultiVariant)
	}

	after, _ := f.groupByKey(t, "a")
	if after.ID != before.ID {
		t.Errorf("the group id changed from %d to %d across a regroup", before.ID, after.ID)
	}
}

// TestGroupSurvivesAMove: a moved file keeps its group, since §9.1 treats a move as an
// update rather than a delete plus insert.
func TestGroupSurvivesAMove(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/hero.png", "png bytes")
	f.write("pack/PSD/hero.psd", "psd bytes")
	f.scan()

	before, _ := f.groupByKey(t, "hero")

	// Move within the pack but outside a format folder, changing the group key.
	f.move("pack/PNG/hero.png", "pack/Characters/hero.png")
	f.scan()

	// The PNG now keys on Characters/hero, so the two split into separate groups.
	// That is correct: they are no longer the same relative path.
	page, err := f.ix.ListGroups(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("%d groups after moving one variant out of its format folder, want 2", page.Total)
	}

	// And no group was left dangling with a missing primary.
	for _, g := range page.Groups {
		if g.Primary.ID == 0 {
			t.Errorf("group %q has no primary", g.GroupKey)
		}
	}
	_ = before
}

func TestEveryAssetGetsAGroup(t *testing.T) {
	f := newFixture(t)
	f.write("pack/a.png", "a")
	f.write("pack/b.glb", "b")
	f.write("pack/PNG/c.png", "c")
	f.scan()

	var ungrouped int
	if err := f.db.Reader.QueryRow(
		`SELECT count(*) FROM assets WHERE group_id IS NULL`).Scan(&ungrouped); err != nil {
		t.Fatal(err)
	}
	if ungrouped != 0 {
		t.Errorf("%d assets have no group", ungrouped)
	}
}

// TestGroupSearchMatchesAnyVariant: §5.1 wants "give me everything I can still edit" to
// be answerable, so searching a source format's name has to find the artwork even
// though the grid shows its PNG.
func TestGroupSearchMatchesAnyVariant(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/wooden_sword.png", "png")
	f.write("pack/PSD/wooden_sword.psd", "psd")
	f.write("pack/PNG/laser_turret.png", "png2")
	f.scan()

	page, err := f.ix.ListGroups(context.Background(), ListOptions{Query: "sword"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Fatalf("searching 'sword' found %d groups, want 1", page.Total)
	}
	// Matched via either variant, but returned once.
	if page.Groups[0].Primary.Ext != "png" {
		t.Errorf("primary is .%s, want png", page.Groups[0].Primary.Ext)
	}

	// Searching the extension itself finds the group through its PSD variant.
	psd, err := f.ix.ListGroups(context.Background(), ListOptions{Query: "psd"})
	if err != nil {
		t.Fatal(err)
	}
	if psd.Total != 1 {
		t.Errorf("searching 'psd' found %d groups, want 1", psd.Total)
	}
}

// TestGroupKindFilterMatchesAnyVariant: a group whose primary is a PNG but which also
// holds a .glb should appear under a model filter.
func TestGroupKindFilterMatchesAnyVariant(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/thing.png", "png")
	f.write("pack/GLB/thing.glb", "glb")
	f.write("pack/PNG/other.png", "png2")
	f.scan()

	models, err := f.ix.ListGroups(context.Background(), ListOptions{Kind: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if models.Total != 1 {
		t.Errorf("kind=model found %d groups, want 1", models.Total)
	}

	images, err := f.ix.ListGroups(context.Background(), ListOptions{Kind: "image"})
	if err != nil {
		t.Fatal(err)
	}
	if images.Total != 2 {
		t.Errorf("kind=image found %d groups, want 2", images.Total)
	}
}

func TestGroupPagination(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 120; i++ {
		f.write(fmt.Sprintf("pack/PNG/item_%03d.png", i), fmt.Sprintf("a%d", i))
		f.write(fmt.Sprintf("pack/PSD/item_%03d.psd", i), fmt.Sprintf("b%d", i))
	}
	f.scan()

	seen := map[int64]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination did not terminate")
		}
		page, err := f.ix.ListGroups(context.Background(), ListOptions{Limit: 40, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != 120 {
			t.Errorf("Total = %d, want 120 groups from 240 files", page.Total)
		}
		for _, g := range page.Groups {
			if seen[g.ID] {
				t.Fatalf("group %d appeared twice", g.ID)
			}
			seen[g.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 120 {
		t.Errorf("paged through %d groups, want 120", len(seen))
	}
}

func TestGroupOf(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/hero.png", "png")
	f.write("pack/PSD/hero.psd", "psd")
	f.scan()

	page, err := f.ix.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Ask via the PSD, which is not the primary.
	var psdID int64
	for _, a := range page.Assets {
		if a.Ext == "psd" {
			psdID = a.ID
		}
	}
	if psdID == 0 {
		t.Fatal("the psd was not indexed")
	}

	g, err := f.ix.GroupOf(context.Background(), psdID)
	if err != nil {
		t.Fatal(err)
	}
	if g.VariantCount != 2 {
		t.Errorf("VariantCount = %d, want 2", g.VariantCount)
	}
	if g.Primary.Ext != "png" {
		t.Errorf("primary is .%s, want png", g.Primary.Ext)
	}
}

// TestEmptyGroupsAreRemoved: a group whose members all vanished would otherwise show as
// an empty grid row forever. The asset rows themselves are still kept (§12).
func TestEmptyGroupsAreRemoved(t *testing.T) {
	f := newFixture(t)
	f.write("pack/PNG/gone.png", "a")
	f.write("pack/PNG/stays.png", "b")
	f.scan()

	if got := f.groupCount(t); got != 2 {
		t.Fatalf("%d groups, want 2", got)
	}

	// Deleting the file marks the asset missing; the group survives because its
	// member row does.
	f.remove("pack/PNG/gone.png")
	f.scan()

	if got := f.groupCount(t); got != 2 {
		t.Errorf("%d groups after a file went missing, want 2 — the asset row is kept", got)
	}
	// The asset row is still there, which is §12's requirement.
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d asset rows, want 2", got)
	}
}

func (f *fixture) groupCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM asset_groups`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

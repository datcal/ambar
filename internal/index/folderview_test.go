package index

import (
	"context"
	"os"
	"reflect"
	"sort"
	"testing"
)

// The folder view: "include subfolders" off, the numbers on the kind switch, the tree's
// per-kind counts, and finding a folder by name. The fixture is a vendor pack shaped like
// the real ones — a cover image at the top, models in format folders two levels down —
// because that shape is what made "show me the models in this pack" unanswerable.

func folderViewFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.write("3d/Vendor/POLYGON - Spy Kit/cover.png", "cover")
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/FBX/SM_Car.fbx", "car fbx")
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/Obj/SM_Car.obj", "car obj")
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/FBX/SM_Pen.fbx", "pen fbx")
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/Textures/Palette.png", "palette")
	f.write("3d/Vendor/POLYGON - Heist/Source/FBX/SM_Safe.fbx", "safe")
	f.write("2d/tiles/grass.png", "grass")
	f.scan()
	return f
}

func groupPaths(t *testing.T, ix *Indexer, opts ListOptions) []string {
	t.Helper()
	opts.Limit = MaxPageSize
	opts.Page = 1
	page, err := ix.ListGroups(context.Background(), opts)
	if err != nil {
		t.Fatalf("list %+v: %v", opts, err)
	}
	var out []string
	for _, g := range page.Groups {
		out = append(out, g.Primary.LibraryPath())
	}
	sort.Strings(out)
	return out
}

func TestDirOnlyLeavesOutSubfolders(t *testing.T) {
	f := folderViewFixture(t)
	pack := "3d/Vendor/POLYGON - Spy Kit"

	// With subfolders, the whole pack: three logical assets plus the cover, the car being
	// one tile for its FBX and its OBJ.
	got := groupPaths(t, f.ix, ListOptions{Dir: pack})
	if len(got) != 4 {
		t.Errorf("Dir=%q => %v, want 4 groups", pack, got)
	}

	// Without, only what sits in the folder itself.
	got = groupPaths(t, f.ix, ListOptions{Dir: pack, DirOnly: true})
	if want := []string{pack + "/cover.png"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DirOnly at the pack => %v, want %v", got, want)
	}
	got = groupPaths(t, f.ix, ListOptions{Dir: pack + "/Source/FBX", DirOnly: true})
	if want := []string{pack + "/Source/FBX/SM_Car.fbx", pack + "/Source/FBX/SM_Pen.fbx"}; !reflect.DeepEqual(got, want) {
		t.Errorf("DirOnly in FBX => %v, want %v", got, want)
	}

	// A prefix of a folder name is not that folder: "POLYGON - Spy" must not match
	// "POLYGON - Spy Kit" just because the strings start the same.
	if got := groupPaths(t, f.ix, ListOptions{Dir: "3d/Vendor/POLYGON - Spy", DirOnly: true}); len(got) != 0 {
		t.Errorf("a partial folder name matched %v", got)
	}

	// No Dir means the library root itself, which holds nothing here.
	if got := groupPaths(t, f.ix, ListOptions{DirOnly: true}); len(got) != 0 {
		t.Errorf("DirOnly at the root => %v, want nothing", got)
	}
}

func TestKindCountsMatchWhatTheKindFilterLists(t *testing.T) {
	f := folderViewFixture(t)
	ctx := context.Background()
	pack := "3d/Vendor/POLYGON - Spy Kit"

	tally, err := f.ix.KindCounts(ctx, ListOptions{Dir: pack, Kind: "image"})
	if err != nil {
		t.Fatal(err)
	}
	// Kind is ignored: these are the numbers for every choice, the current one included.
	if tally.Total != 4 {
		t.Errorf("total = %d, want 4", tally.Total)
	}
	for _, kind := range []string{"model", "image"} {
		listed := len(groupPaths(t, f.ix, ListOptions{Dir: pack, Kind: kind}))
		if got := tally.Count(kind); got != listed {
			t.Errorf("count for %s = %d, but the kind filter lists %d", kind, got, listed)
		}
	}
	if tally.Count("model") != 2 {
		t.Errorf("models = %d, want 2 (the car's FBX and OBJ are one)", tally.Count("model"))
	}
	if tally.Count("audio") != 0 {
		t.Errorf("audio = %d, want 0", tally.Count("audio"))
	}

	// The search narrows the counts too.
	tally, err = f.ix.KindCounts(ctx, ListOptions{Dir: pack, Query: "car"})
	if err != nil {
		t.Fatal(err)
	}
	if tally.Total != 1 || tally.Count("model") != 1 {
		t.Errorf("searching 'car' => %+v, want one model", tally)
	}
}

func TestTreeCountsLogicalAssetsByKind(t *testing.T) {
	f := folderViewFixture(t)

	root, err := f.ix.Tree(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	pack := root.Find("3d/Vendor/POLYGON - Spy Kit")
	if pack == nil {
		t.Fatal("Find did not reach the pack")
	}
	// Files: five. Logical assets: four, because the car is one.
	if pack.Assets != 5 || pack.Groups != 4 {
		t.Errorf("pack assets=%d groups=%d, want 5 and 4", pack.Assets, pack.Groups)
	}
	if pack.Count("model") != 2 || pack.Count("image") != 2 || pack.Count("") != 4 {
		t.Errorf("pack counts = %v (all %d)", pack.Kinds, pack.Count(""))
	}
	// A group is placed where its primary sits — the FBX, by §5.1's precedence — so the
	// Obj folder holds a file but no tile of its own.
	if obj := root.Find("3d/Vendor/POLYGON - Spy Kit/Source/Obj"); obj == nil || obj.Assets != 1 || obj.Groups != 0 {
		t.Errorf("Source/Obj = %+v, want one file and no groups", obj)
	}
	if root.Count("model") != 3 {
		t.Errorf("library models = %d, want 3", root.Count("model"))
	}
	if root.Find("3d/nowhere") != nil {
		t.Error("Find invented a folder")
	}

	// Each count agrees with the grid for the same folder.
	for _, path := range []string{"3d", "3d/Vendor/POLYGON - Spy Kit", "3d/Vendor/POLYGON - Spy Kit/Source/FBX"} {
		node := root.Find(path)
		for _, kind := range []string{"", "model", "image"} {
			listed := len(groupPaths(t, f.ix, ListOptions{Dir: path, Kind: kind}))
			if node.Count(kind) != listed {
				t.Errorf("%s kind %q: tree says %d, grid lists %d", path, kind, node.Count(kind), listed)
			}
		}
	}
}

func TestFindFolders(t *testing.T) {
	f := folderViewFixture(t)
	root, err := f.ix.Tree(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	paths := func(query string) []string {
		var out []string
		for _, n := range FindFolders(root, query, FolderSearchLimit) {
			out = append(out, n.Path)
		}
		return out
	}

	// The pack, not every folder inside it.
	if got, want := paths("spy"), []string{"3d/Vendor/POLYGON - Spy Kit"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spy => %v, want %v", got, want)
	}
	// Several words narrow down through the path; the last one names the folder.
	if got, want := paths("spy fbx"), []string{"3d/Vendor/POLYGON - Spy Kit/Source/FBX"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spy fbx => %v, want %v", got, want)
	}
	// An exact name beats a longer one, and a shallow folder a deep one.
	got := paths("fbx")
	if len(got) != 2 {
		t.Errorf("fbx => %v, want both FBX folders", got)
	}
	if got := paths("vendor"); len(got) == 0 || got[0] != "3d/Vendor" {
		t.Errorf("vendor => %v, want 3d/Vendor first", got)
	}
	if got := paths("   "); got != nil {
		t.Errorf("a blank query found %v", got)
	}
	if got := paths("nothing-like-this"); got != nil {
		t.Errorf("an unknown name found %v", got)
	}
}

func TestListGroupsCarriesFormats(t *testing.T) {
	f := folderViewFixture(t)
	page, err := f.ix.ListGroups(context.Background(), ListOptions{Query: "car", Limit: 10, Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Groups) != 1 {
		t.Fatalf("want the car, got %d groups", len(page.Groups))
	}
	if got, want := page.Groups[0].Formats, []string{"fbx", "obj"}; !reflect.DeepEqual(got, want) {
		t.Errorf("formats = %v, want %v (primary first)", got, want)
	}
}

// The tree's tally is the kind switch for a folder without a search, so it has to agree
// with KindCounts exactly — including a group that is two kinds at once, which counts
// under both, as the grid's kind filter lists it under both.
func TestTreeTallyAgreesWithKindCounts(t *testing.T) {
	f := folderViewFixture(t)
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/Mixed/thing.png", "picture")
	f.write("3d/Vendor/POLYGON - Spy Kit/Source/Mixed/thing.glb", "model")
	f.scan()
	ctx := context.Background()

	root, err := f.ix.Tree(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"", "3d", "3d/Vendor/POLYGON - Spy Kit", "3d/Vendor/POLYGON - Spy Kit/Source/Mixed", "2d/tiles"} {
		for _, direct := range []bool{false, true} {
			if dir == "" && direct {
				continue
			}
			want, err := f.ix.KindCounts(ctx, ListOptions{Dir: dir, DirOnly: direct})
			if err != nil {
				t.Fatal(err)
			}
			got := root.Find(dir).Tally(direct)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("%q direct=%v: tree %+v, KindCounts %+v", dir, direct, got, want)
			}
		}
	}
	mixed := root.Find("3d/Vendor/POLYGON - Spy Kit/Source/Mixed")
	if mixed.Count("model") != 1 || mixed.Count("image") != 1 || mixed.Count("") != 1 {
		t.Errorf("a PNG+GLB group: %v (total %d), want one under each kind and one in all", mixed.Kinds, mixed.Groups)
	}
}

// The two ways a page is read — the planner's own, and walking the order's index for a
// large result — must give the same rows in the same order, and paging through either must
// meet every group once. Equal sizes and equal first-seen times on purpose: only the
// tiebreaker separates them.
func TestSortIndexWalkMatchesThePlanner(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 23; i++ {
		f.write("pack/PNG/same_"+string(rune('a'+i))+".png", "xxxx")
	}
	f.write("pack/PSD/same_a.psd", "variant")
	f.scan()

	walk := func(sortBy SortOrder, threshold int) []int64 {
		saved := sortIndexMinRows
		sortIndexMinRows = threshold
		defer func() { sortIndexMinRows = saved }()
		var ids []int64
		for page := 1; page < 10; page++ {
			got, err := f.ix.ListGroups(context.Background(), ListOptions{Sort: sortBy, Limit: 5, Page: page})
			if err != nil {
				t.Fatalf("%s page %d: %v", sortBy, page, err)
			}
			for _, g := range got.Groups {
				ids = append(ids, g.ID)
			}
			if !got.HasNext() {
				break
			}
		}
		return ids
	}
	for _, sortBy := range []SortOrder{SortNewest, SortModified, SortName, SortNameDesc, SortLargest, SortKind} {
		planner := walk(sortBy, 1<<30)
		indexed := walk(sortBy, 0)
		if !reflect.DeepEqual(planner, indexed) {
			t.Errorf("%s: the index walk gave %v, the planner %v", sortBy, indexed, planner)
		}
		seen := map[int64]bool{}
		for _, id := range indexed {
			if seen[id] {
				t.Errorf("%s: group %d on two pages", sortBy, id)
			}
			seen[id] = true
		}
		if len(seen) != 23 {
			t.Errorf("%s: paging met %d groups, want 23", sortBy, len(seen))
		}
	}

	// A hint replaces the count, and the page is still the same.
	got, err := f.ix.ListGroups(context.Background(), ListOptions{Limit: 5, Page: 1, TotalHint: 23})
	if err != nil || got.Total != 23 || len(got.Groups) != 5 {
		t.Errorf("with a hint: total %d, %d groups, err %v", got.Total, len(got.Groups), err)
	}
}

// C and C++ are code; Unreal packages stay "other" and are found with ext:. Rows indexed
// before that are moved by migration 0022, which also drops the tagger's stale type:other
// tag but never a person's.
func TestCodeKindExtSearchAndTheRepair(t *testing.T) {
	f := newFixture(t)
	f.write("unreal-pack/Content/BP_Door.uasset", "package")
	f.write("unreal-pack/Content/Maps/Level.umap", "map")
	f.write("unreal-pack/Source/Door.cpp", "code")
	f.write("unreal-pack/Source/Door.h", "header")
	f.write("unreal-pack/notes.ini", "ini")
	f.scan()
	ctx := context.Background()

	kindOf := func(name string) string {
		var kind string
		if err := f.db.Reader.QueryRow(`SELECT kind FROM assets WHERE filename = ?`, name).Scan(&kind); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return kind
	}
	want := map[string]string{"BP_Door.uasset": "other", "Level.umap": "other", "Door.cpp": "script", "Door.h": "script", "notes.ini": "other"}
	for name, kind := range want {
		if got := kindOf(name); got != kind {
			t.Errorf("%s indexed as %q, want %q", name, got, kind)
		}
	}

	// ext: finds the Unreal packages, alone or with a negation.
	if got := f.searchPaths("ext:uasset,umap"); len(got) != 2 {
		t.Errorf("ext:uasset,umap => %v, want the two packages", got)
	}
	if got := f.searchPaths("ext:.UASSET"); len(got) != 1 {
		t.Errorf("ext:.UASSET => %v, want one (case and the dot do not matter)", got)
	}
	if got := f.searchPaths("-ext:uasset,umap,cpp,h"); len(got) != 1 {
		t.Errorf("-ext:… => %v, want only notes.ini", got)
	}

	// As an older index had the code: "other", with the tagger's type:other.
	if _, err := f.db.Writer.Exec(`UPDATE assets SET kind = 'other' WHERE ext IN ('cpp', 'h')`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`INSERT OR IGNORE INTO tags (namespace, name, created_at, updated_at) VALUES ('type', 'other', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	var otherTag int64
	if err := f.db.Reader.QueryRow(`SELECT id FROM tags WHERE namespace = 'type' AND name = 'other'`).Scan(&otherTag); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`DELETE FROM asset_tags WHERE tag_id = ?`, otherTag); err != nil {
		t.Fatal(err)
	}
	tag := func(source, filename string) {
		if _, err := f.db.Writer.Exec(`INSERT INTO asset_tags (asset_id, tag_id, source, created_at)
			SELECT id, ?, ?, 0 FROM assets WHERE filename = ?`, otherTag, source, filename); err != nil {
			t.Fatal(err)
		}
	}
	tag("auto_type", "Door.h")
	tag("auto_type", "BP_Door.uasset")
	tag("manual", "Door.cpp")

	sqlText, err := os.ReadFile("../db/migrations/0022_code_kind.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.ExecContext(ctx, string(sqlText)); err != nil {
		t.Fatalf("migration: %v", err)
	}
	for name, kind := range want {
		if got := kindOf(name); got != kind {
			t.Errorf("after the migration %s is %q, want %q", name, got, kind)
		}
	}
	tagged := func(filename string) bool {
		var n int
		f.db.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN assets a ON a.id = at.asset_id
			WHERE a.filename = ? AND at.tag_id = ?`, filename, otherTag).Scan(&n)
		return n > 0
	}
	if tagged("Door.h") {
		t.Error("the tagger's type:other stayed on reclassified code")
	}
	if !tagged("BP_Door.uasset") {
		t.Error("a file that is still 'other' lost its tag")
	}
	if !tagged("Door.cpp") {
		t.Error("a person's tag was removed")
	}
}

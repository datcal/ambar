package index

import (
	"context"
	"reflect"
	"sort"
	"strconv"
	"testing"

	"github.com/datcal/ambar/internal/tags"
)

// searchPaths runs a query through List and returns the matched library paths,
// sorted, so a test can assert on the set regardless of order.
func (f *fixture) searchPaths(query string) []string {
	f.t.Helper()
	page, err := f.ix.List(context.Background(), ListOptions{Query: query, Limit: MaxPageSize})
	if err != nil {
		f.t.Fatalf("List(%q): %v", query, err)
	}
	out := make([]string, 0, len(page.Assets))
	for _, a := range page.Assets {
		out = append(out, a.LibraryPath())
	}
	sort.Strings(out)
	return out
}

func eq(t *testing.T, query string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Errorf("query %q => %v, want %v", query, got, want)
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("query %q => %v, want %v", query, got, want)
			return
		}
	}
}

// TestQueryLanguageEndToEnd drives the parser + compiler against a real index,
// proving the generated SQL runs and selects the right rows.
func TestQueryLanguageEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.write("weapons/wooden_sword.png", "a")
	f.write("weapons/laser_gun.png", "b")
	f.write("sfx/explosion.wav", "c")
	f.scan()

	ctx := context.Background()
	store := tags.NewStore(f.db)

	sword, _ := f.assetByPath("weapons/wooden_sword.png")
	explosion, _ := f.assetByPath("sfx/explosion.wav")

	// Hierarchy: the sword gets a deep tag; a filter on its ancestor must match it.
	if _, err := store.TagAsset(ctx, sword.ID, "type:melee:sword", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}
	// An alias for a tag, and a tag on the explosion.
	if _, err := store.TagAsset(ctx, explosion.ID, "type:sfx:impact", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SetAlias(ctx, "boom", "type:sfx:impact"); err != nil {
		t.Fatal(err)
	}
	// A pack tag the whole sfx pack inherits.
	if _, err := store.TagPack(ctx, explosion.PackID, "license:cc0", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		{"kind", "kind:audio", []string{"sfx/explosion.wav"}},
		{"free text prefix", "wood", []string{"weapons/wooden_sword.png"}},
		{"tag hierarchy ancestor", "type:melee", []string{"weapons/wooden_sword.png"}},
		{"tag alias", "boom", []string{"sfx/explosion.wav"}},
		{"inherited pack tag", "license:cc0", []string{"sfx/explosion.wav"}},
		{"negation excludes", "-kind:audio", []string{"weapons/laser_gun.png", "weapons/wooden_sword.png"}},
		{"implicit and", "kind:image wood", []string{"weapons/wooden_sword.png"}},
		{"or groups", "kind:audio OR type:melee", []string{"sfx/explosion.wav", "weapons/wooden_sword.png"}},
		{"phrase", `"laser"`, []string{"weapons/laser_gun.png"}},
		{"unknown tag matches nothing", "theme:nonexistent", nil},
		// Colour search has no swatch data here (nothing has been derived), so it
		// matches nothing rather than everything — see TestColourSearch for the
		// filtering itself.
		{"colour with no palette data", "color:#fff", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eq(t, tc.query, f.searchPaths(tc.query), tc.want)
		})
	}
}

// swatch is one palette entry for the fixture helper below.
type swatch struct {
	r, g, b int
	ratio   float64
}

// swatches writes indexed palette rows for an asset, the way derive does.
func (f *fixture) swatches(assetID int64, list ...swatch) {
	f.t.Helper()
	for i, s := range list {
		if _, err := f.db.Writer.Exec(`
			INSERT INTO asset_swatches (asset_id, rank, r, g, b, ratio) VALUES (?, ?, ?, ?, ?, ?)`,
			assetID, i, s.r, s.g, s.b, s.ratio); err != nil {
			f.t.Fatal(err)
		}
	}
}

// TestColourSearch covers §7's colour filters against real indexed swatches: the
// colour box with its tolerance, and palette-near's nearest-neighbour comparison.
//
// The swatch rows are written here directly rather than by running derive, because
// what is under test is the query, and a fixture PNG with a known palette is
// derive's business.
func TestColourSearch(t *testing.T) {
	f := newFixture(t)
	f.write("art/rust.png", "a")
	f.write("art/rust-lookalike.png", "b")
	f.write("art/forest.png", "c")
	f.write("art/undecided.png", "d")
	f.scan()

	rust, _ := f.assetByPath("art/rust.png")
	lookalike, _ := f.assetByPath("art/rust-lookalike.png")
	forest, _ := f.assetByPath("art/forest.png")
	undecided, _ := f.assetByPath("art/undecided.png")

	// rust and rust-lookalike share an art direction; forest does not. undecided has
	// one dominant colour plus a stray pixel of rust's red, which is what the ratio
	// floor is there to ignore.
	f.swatches(rust.ID,
		swatch{0x8b, 0x3a, 0x3a, 0.5}, // rust red
		swatch{0x4a, 0x3b, 0x2f, 0.3}, // brown
		swatch{0xd9, 0xc7, 0xa1, 0.2}, // sand
	)
	f.swatches(lookalike.ID,
		swatch{0x8d, 0x3c, 0x3d, 0.4}, // rust red, two units off
		swatch{0x4c, 0x3d, 0x30, 0.35},
		swatch{0xdb, 0xc9, 0xa3, 0.25},
	)
	f.swatches(forest.ID,
		swatch{0x2f, 0x6b, 0x35, 0.6},
		swatch{0x17, 0x3a, 0x1c, 0.4},
	)
	f.swatches(undecided.ID,
		swatch{0x11, 0x22, 0x33, 0.999},
		swatch{0x8b, 0x3a, 0x3a, 0.001}, // three stray pixels of rust red
	)

	tests := []struct {
		name  string
		query string
		want  []string
	}{
		// The default tolerance is deliberately narrow but not zero, so rust's red and
		// the lookalike's two-units-off red are the same colour to a person.
		{"colour within the default tolerance", "color:#8b3a3a",
			[]string{"art/rust-lookalike.png", "art/rust.png"}},
		{"no hash needed", "color:8b3a3a", []string{"art/rust-lookalike.png", "art/rust.png"}},
		{"zero tolerance is an exact match", "color:#8b3a3a~0", []string{"art/rust.png"}},
		{"short hex form", "color:#f00", nil},
		{"tight tolerance excludes the original", "color:#8d3c3d~1", []string{"art/rust-lookalike.png"}},
		{"wide tolerance catches everything", "color:#808080~255",
			[]string{"art/forest.png", "art/rust-lookalike.png", "art/rust.png", "art/undecided.png"}},
		// undecided holds three stray pixels of exactly rust's red. Below the ratio
		// floor, so "contains this colour" is false for it.
		{"a stray pixel is not a colour", "color:#8b3a3a~0", []string{"art/rust.png"}},
		{"negation", "-color:#8b3a3a", []string{"art/forest.png", "art/undecided.png"}},
		{"combines with other terms", "kind:image color:#8b3a3a~0", []string{"art/rust.png"}},
		{"malformed colour is ignored", "color:zzz", []string{"art/forest.png", "art/rust-lookalike.png",
			"art/rust.png", "art/undecided.png"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eq(t, tc.query, f.searchPaths(tc.query), tc.want)
		})
	}

	// palette-near finds the asset sharing an art direction, and not the one that
	// merely has a pixel in common.
	t.Run("palette-near", func(t *testing.T) {
		want := []string{"art/rust-lookalike.png", "art/rust.png"}
		eq(t, "palette-near", f.searchPaths(itoaQuery(rust.ID)), want)
	})
	t.Run("palette-near excludes a different palette", func(t *testing.T) {
		got := f.searchPaths(itoaQuery(forest.ID))
		eq(t, "palette-near forest", got, []string{"art/forest.png"})
	})
	t.Run("palette-near on an asset with no palette matches nothing", func(t *testing.T) {
		f.write("art/nopalette.png", "e")
		f.scan()
		empty, _ := f.assetByPath("art/nopalette.png")
		eq(t, "palette-near empty", f.searchPaths(itoaQuery(empty.ID)), nil)
	})
	t.Run("palette-near on an unknown id matches nothing", func(t *testing.T) {
		eq(t, "palette-near unknown", f.searchPaths("palette-near:999999"), nil)
	})
}

func itoaQuery(id int64) string {
	return "palette-near:" + strconv.FormatInt(id, 10)
}

// --- the folder tree (M14) ---------------------------------------------------

// TestTreeAndDirFilter covers the navigation the grid never had: a directory tree
// with rolled-up counts, and the filter that a click on a node applies.
func TestTreeAndDirFilter(t *testing.T) {
	f := newFixture(t)
	f.write("2d/pack-a/PNG/hero.png", "a")
	f.write("2d/pack-a/PNG/tree.png", "b")
	f.write("2d/pack-a/PSD/hero.psd", "c")
	f.write("2d/pack-b/sprite.png", "d")
	f.write("audio/hit.wav", "e")
	f.write("loose.png", "f")
	f.scan()

	root, err := f.ix.Tree(context.Background(), 0)
	if err != nil {
		t.Fatalf("tree: %v", err)
	}

	// Counts roll up: the whole library, then each branch.
	if root.Assets != 6 {
		t.Errorf("root holds %d assets, want 6", root.Assets)
	}
	// A file at the library root is counted directly on the root node.
	if root.Direct != 1 {
		t.Errorf("root has %d direct assets, want the one loose file", root.Direct)
	}

	byPath := map[string]*TreeNode{}
	var walk func(*TreeNode)
	walk = func(n *TreeNode) {
		byPath[n.Path] = n
		for _, c := range n.Children {
			walk(c)
		}
	}
	walk(root)

	for path, want := range map[string]int{
		"2d":            4,
		"2d/pack-a":     3,
		"2d/pack-a/PNG": 2,
		"2d/pack-a/PSD": 1,
		"2d/pack-b":     1,
		"audio":         1,
	} {
		node, ok := byPath[path]
		if !ok {
			t.Errorf("tree is missing %q", path)
			continue
		}
		if node.Assets != want {
			t.Errorf("%s holds %d assets, want %d", path, node.Assets, want)
		}
	}

	// Children are ordered like a file manager.
	var names []string
	for _, c := range root.Children {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"2d", "audio"}) {
		t.Errorf("top level = %v, want [2d audio]", names)
	}

	// Clicking a node filters the grid to that subtree.
	cases := map[string][]string{
		"2d":            {"2d/pack-a/PNG/hero.png", "2d/pack-a/PNG/tree.png", "2d/pack-b/sprite.png"},
		"2d/pack-a":     {"2d/pack-a/PNG/hero.png", "2d/pack-a/PNG/tree.png"},
		"2d/pack-a/PNG": {"2d/pack-a/PNG/hero.png", "2d/pack-a/PNG/tree.png"},
		"audio":         {"audio/hit.wav"},
	}
	for dir, want := range cases {
		page, err := f.ix.ListGroups(context.Background(), ListOptions{Dir: dir, Limit: MaxPageSize})
		if err != nil {
			t.Fatalf("list under %q: %v", dir, err)
		}
		var got []string
		for _, g := range page.Groups {
			got = append(got, g.Primary.LibraryPath())
		}
		sort.Strings(got)
		// The PSD is a format variant of hero.png, so the group's primary is the PNG —
		// invariant 7 holds through the folder filter too.
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Dir=%q => %v, want %v", dir, got, want)
		}
	}

	// A traversal or an empty filter browses everything rather than erroring.
	for _, dir := range []string{"", ".", "..", "../..", "/"} {
		page, err := f.ix.ListGroups(context.Background(), ListOptions{Dir: dir, Limit: MaxPageSize})
		if err != nil {
			t.Fatalf("Dir=%q: %v", dir, err)
		}
		if len(page.Groups) == 0 {
			t.Errorf("Dir=%q filtered everything out; it should browse the whole library", dir)
		}
	}

	// Flatten shows the top level plus the open branch, not the whole tree.
	flat := Flatten(root, "2d/pack-a")
	var visible []string
	for _, n := range flat {
		visible = append(visible, n.Path)
	}
	want := []string{"2d", "2d/pack-a", "2d/pack-a/PNG", "2d/pack-a/PSD", "2d/pack-b", "audio"}
	if !reflect.DeepEqual(visible, want) {
		t.Errorf("Flatten = %v, want %v", visible, want)
	}
	for _, n := range flat {
		if n.Path == "2d/pack-a" && !n.Selected {
			t.Error("the browsed directory should be marked selected")
		}
		if n.Path == "2d/pack-b" && n.Open {
			t.Error("an unrelated branch must stay closed")
		}
	}

	// maxDepth attributes anything deeper to its deepest allowed ancestor.
	shallow, err := f.ix.Tree(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range shallow.Children {
		for _, g := range c.Children {
			if len(g.Children) != 0 {
				t.Errorf("maxDepth=2 produced a node at depth 3: %s", g.Children[0].Path)
			}
		}
	}
}

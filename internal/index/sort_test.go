package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The browse orders (M16). Before this there was one — filename A→Z over the whole library —
// so "what did I download yesterday" had no answer at all.
func TestListGroupsSortOrders(t *testing.T) {
	f := newFixture(t)
	// Written b, a, c so filename order and insertion order disagree; sizes and mtimes are
	// set deliberately so every order has a different expected answer.
	f.write("pack/PNG/b_mid.png", "bb")
	f.write("pack/PNG/a_big.png", "aaaaaaaaaa")
	f.write("pack/PNG/c_small.png", "c")

	// mtimes: a is oldest on disk, c is newest.
	base := time.Now().Add(-72 * time.Hour)
	for i, name := range []string{"a_big.png", "b_mid.png", "c_small.png"} {
		path := filepath.Join(f.root, "pack", "PNG", name)
		when := base.Add(time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, when, when); err != nil {
			t.Fatal(err)
		}
	}
	f.scan()

	names := func(sort SortOrder) []string {
		page, err := f.ix.ListGroups(context.Background(), ListOptions{Sort: sort, Page: 1})
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(page.Groups))
		for _, g := range page.Groups {
			out = append(out, g.Primary.Filename)
		}
		return out
	}

	cases := []struct {
		sort SortOrder
		want []string
		why  string
	}{
		{SortName, []string{"a_big.png", "b_mid.png", "c_small.png"}, "alphabetical"},
		{SortNameDesc, []string{"c_small.png", "b_mid.png", "a_big.png"}, "reverse alphabetical"},
		{SortLargest, []string{"a_big.png", "b_mid.png", "c_small.png"}, "biggest file first"},
		{SortModified, []string{"c_small.png", "b_mid.png", "a_big.png"}, "newest file date first"},
	}
	for _, tc := range cases {
		got := names(tc.sort)
		if len(got) != len(tc.want) {
			t.Fatalf("sort=%s returned %d groups, want %d", tc.sort, len(got), len(tc.want))
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("sort=%s (%s) = %v, want %v", tc.sort, tc.why, got, tc.want)
				break
			}
		}
	}

	// The default is "recently added", which for one scan is a single instant — so this
	// asserts the contract rather than an order: the default must not be filename order,
	// which is what it silently was before.
	page, err := f.ix.ListGroups(context.Background(), ListOptions{Page: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.Sort != SortDefault {
		t.Errorf("default sort = %q, want %q", page.Sort, SortDefault)
	}
	if SortDefault == SortName {
		t.Error("the default order is filename again")
	}
}

// Numbered paging: every page of a run must be reachable, and the run must cover every group
// exactly once. An offset pager gets this wrong the moment its ORDER BY is not unique.
func TestListGroupsNumberedPagingCoversEverything(t *testing.T) {
	f := newFixture(t)
	// Identical sizes on purpose: with `size DESC` alone as the order, SQLite is free to
	// return these in any order per query, and a pager would repeat and skip rows. The
	// tiebreaker on g.id is what makes it stable.
	for i := 0; i < 25; i++ {
		f.write(filepath.Join("pack", "PNG", "same_"+itoa2(i)+".png"), "xxxx")
	}
	f.scan()

	seen := map[int64]int{}
	pages := 0
	for page := 1; ; page++ {
		if page > 10 {
			t.Fatal("paging did not terminate")
		}
		got, err := f.ix.ListGroups(context.Background(), ListOptions{Sort: SortLargest, Limit: 10, Page: page})
		if err != nil {
			t.Fatal(err)
		}
		if got.Total != 25 {
			t.Fatalf("Total = %d, want 25", got.Total)
		}
		if got.Pages() != 3 {
			t.Fatalf("Pages() = %d, want 3", got.Pages())
		}
		pages++
		for _, g := range got.Groups {
			seen[g.ID]++
		}
		if !got.HasNext() {
			break
		}
	}

	if pages != 3 {
		t.Errorf("walked %d pages, want 3", pages)
	}
	if len(seen) != 25 {
		t.Errorf("saw %d distinct groups, want 25", len(seen))
	}
	for id, times := range seen {
		if times != 1 {
			t.Errorf("group %d appeared %d times", id, times)
		}
	}
}

func itoa2(n int) string {
	const digits = "0123456789"
	return string([]byte{digits[n/10], digits[n%10]})
}

// TestSortByTriangleCount is M17's answer to "3D needs polygon count".
//
// `tris:<5000` had worked as a search filter since M16, but a filter needs a number you
// already have in mind. Browsing by cost — "show me the cheapest models in this pack" —
// had no order at all, and it is the question a five-person studio asks before it asks
// any other question about a model.
func TestSortByTriangleCount(t *testing.T) {
	f := newFixture(t)
	f.write("3d/pack/heavy.glb", "glTF-heavy")
	f.write("3d/pack/light.glb", "glTF-light")
	f.write("3d/pack/middle.glb", "glTF-middle")
	f.write("2d/pack/sprite.png", "a sprite with no triangles at all")
	f.scan()

	for name, tris := range map[string]int{"heavy.glb": 90000, "light.glb": 120, "middle.glb": 4000} {
		if _, err := f.db.Writer.Exec(
			`UPDATE assets SET tri_count = ? WHERE filename = ?`, tris, name); err != nil {
			t.Fatal(err)
		}
	}

	names := func(sort SortOrder) []string {
		t.Helper()
		page, err := f.ix.ListGroups(context.Background(), ListOptions{Sort: sort, Page: 1, Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, g := range page.Groups {
			out = append(out, g.Primary.Filename)
		}
		return out
	}

	asc := names(SortTrisAsc)
	if len(asc) != 4 {
		t.Fatalf("got %d groups, want 4: %v", len(asc), asc)
	}
	if asc[0] != "light.glb" || asc[1] != "middle.glb" || asc[2] != "heavy.glb" {
		t.Errorf("ascending order = %v, want light, middle, heavy first", asc)
	}
	// The sprite has no triangle count, and an unknown count is not a small one — it
	// goes last, or the first page of "cheapest models" is full of 2D art.
	if asc[3] != "sprite.png" {
		t.Errorf("ascending order = %v, want the countless asset last", asc)
	}

	desc := names(SortTrisDesc)
	if desc[0] != "heavy.glb" || desc[1] != "middle.glb" || desc[2] != "light.glb" {
		t.Errorf("descending order = %v, want heavy, middle, light first", desc)
	}

	// And it is offered in the UI, not only reachable by hand-editing a URL.
	var offered bool
	for _, s := range SortOrders() {
		if s == SortTrisAsc {
			offered = true
		}
	}
	if !offered {
		t.Error("the triangle order is not in the dropdown")
	}
	if ParseSort("tris") != SortTrisAsc || ParseSort("tris-desc") != SortTrisDesc {
		t.Error("the URL values do not round-trip")
	}
}

package index

import (
	"context"
	"sort"
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
		{"colour no-op returns all", "color:#fff", []string{"sfx/explosion.wav", "weapons/laser_gun.png", "weapons/wooden_sword.png"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			eq(t, tc.query, f.searchPaths(tc.query), tc.want)
		})
	}
}

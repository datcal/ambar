package index

import (
	"context"
	"testing"

	"github.com/datcal/ambar/internal/tags"
)

func facetCount(facets []Facet, canonical string) int {
	for _, f := range facets {
		if f.Canonical == canonical {
			return f.Count
		}
	}
	return 0
}

func TestFacetsCountDirectAndInherited(t *testing.T) {
	f := newFixture(t)
	f.write("weapons/sword.png", "a")
	f.write("weapons/axe.png", "b")
	f.write("sfx/boom.wav", "c")
	f.scan()

	ctx := context.Background()
	store := tags.NewStore(f.db)

	sword, _ := f.assetByPath("weapons/sword.png")
	axe, _ := f.assetByPath("weapons/axe.png")
	boom, _ := f.assetByPath("sfx/boom.wav")

	// Two assets carry theme:medieval directly.
	store.TagAsset(ctx, sword.ID, "theme:medieval", tags.SourceManual, nil)
	store.TagAsset(ctx, axe.ID, "theme:medieval", tags.SourceManual, nil)
	store.TagAsset(ctx, boom.ID, "theme:sci-fi", tags.SourceManual, nil)
	// A pack tag every member of the weapons pack inherits.
	store.TagPack(ctx, sword.PackID, "license:cc0", tags.SourceManual, nil)

	// Over the whole library.
	all, err := f.ix.Facets(ctx, ListOptions{}, DefaultFacetLimit)
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if got := facetCount(all, "theme:medieval"); got != 2 {
		t.Errorf("theme:medieval count = %d, want 2", got)
	}
	if got := facetCount(all, "license:cc0"); got != 2 { // inherited by both weapons
		t.Errorf("license:cc0 count = %d, want 2 (inherited)", got)
	}
	if got := facetCount(all, "theme:sci-fi"); got != 1 {
		t.Errorf("theme:sci-fi count = %d, want 1", got)
	}

	// Facets respect the current filter: within kind:audio, only sci-fi shows.
	audio, _ := f.ix.Facets(ctx, ListOptions{Query: "kind:audio"}, DefaultFacetLimit)
	if got := facetCount(audio, "theme:sci-fi"); got != 1 {
		t.Errorf("within audio, theme:sci-fi = %d, want 1", got)
	}
	if got := facetCount(audio, "theme:medieval"); got != 0 {
		t.Errorf("within audio, theme:medieval = %d, want 0", got)
	}
}

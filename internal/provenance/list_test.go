package provenance

import (
	"context"
	"testing"

	"github.com/datcal/ambar/internal/db"
)

func TestSniffURL(t *testing.T) {
	tests := []struct {
		in         string
		wantSite   string
		wantAuthor string
	}{
		{"https://kenney.itch.io/sci-fi-rts", "itch.io", "kenney"},
		{"https://www.kenney.itch.io/x", "itch.io", "kenney"},
		{"https://opengameart.org/content/foo", "OpenGameArt", ""},
		{"https://poly.pizza/m/abc", "Poly Pizza", ""},
		{"https://example.com/pack", "example.com", ""},
		{"", "", ""},
		{"not a url at all", "", ""},
	}
	for _, tc := range tests {
		got := SniffURL(tc.in)
		if got.Site != tc.wantSite || got.Author != tc.wantAuthor {
			t.Errorf("SniffURL(%q) = %+v, want site=%q author=%q", tc.in, got, tc.wantSite, tc.wantAuthor)
		}
	}
}

func TestSummariesFilters(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()

	// Pack A: fully licensed CC0 (fine).
	a := insertPack(t, database)
	cc0, _, _ := s.LicenseBySPDX(ctx, "CC0-1.0")
	s.Update(ctx, Provenance{PackID: a, SourceAuthor: "Kenney", LicenseID: &cc0.ID, State: StateComplete})

	// Pack B: needs provenance, no licence (risk).
	b := insertPack2(t, database, "other")

	needs, err := s.Summaries(ctx, FilterNeeds)
	if err != nil {
		t.Fatal(err)
	}
	if len(needs) != 1 || needs[0].PackID != b {
		t.Errorf("needs filter = %+v, want only pack B", needs)
	}

	risk, err := s.Summaries(ctx, FilterRisk)
	if err != nil {
		t.Fatal(err)
	}
	if len(risk) != 1 || risk[0].PackID != b || risk[0].AttentionReason() != "no licence" {
		t.Errorf("risk filter = %+v", risk)
	}

	all, _ := s.Summaries(ctx, FilterAll)
	if len(all) != 2 {
		t.Errorf("all = %d, want 2", len(all))
	}
}

// insertPack2 adds a second pack with a distinct rel path.
func insertPack2(t *testing.T, database *db.DB, slug string) int64 {
	t.Helper()
	res, err := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'folder', ?, 0, 0, 0, 0)`, slug, slug, slug)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

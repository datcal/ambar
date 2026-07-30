package provenance

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func newStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	return NewStore(database).WithClock(func() time.Time { return fixed }), database
}

func insertPack(t *testing.T, database *db.DB) int64 {
	t.Helper()
	now := time.Now().Unix()
	res, err := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('kenney', 'kenney', 'folder', 'itch/kenney', ?, ?, ?, ?)`, now, now, now, now)
	if err != nil {
		t.Fatalf("insert pack: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func TestLicensesSeeded(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()
	licenses, err := s.Licenses(ctx)
	if err != nil {
		t.Fatalf("licenses: %v", err)
	}
	if len(licenses) < 5 {
		t.Fatalf("expected the seed set, got %d", len(licenses))
	}
	cc0, ok, err := s.LicenseBySPDX(ctx, "CC0-1.0")
	if err != nil || !ok {
		t.Fatalf("CC0 missing: ok=%v err=%v", ok, err)
	}
	if !cc0.CommercialOK || cc0.AttributionRequired {
		t.Errorf("CC0 flags wrong: %+v", cc0)
	}
	sa, _, _ := s.LicenseBySPDX(ctx, "CC-BY-SA-4.0")
	if !sa.ShareAlike || !sa.AttributionRequired {
		t.Errorf("CC-BY-SA flags wrong: %+v", sa)
	}
}

func TestPackStartsNeedingProvenance(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	id := insertPack(t, database)

	p, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if p.State != StateNeedsProvenance {
		t.Errorf("state = %q, want needs_provenance", p.State)
	}
	if p.LicenseID != nil {
		t.Errorf("a fresh pack should have no licence")
	}
}

func TestUpdateRoundTrips(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	id := insertPack(t, database)

	cc0, _, _ := s.LicenseBySPDX(ctx, "CC0-1.0")
	price := int64(1500)
	acquired := time.Unix(1_600_000_000, 0)
	in := Provenance{
		PackID:              id,
		SourceURL:           "https://kenney.itch.io/sci-fi-rts",
		SourceSite:          "itch.io",
		SourceAuthor:        "Kenney",
		LicenseID:           &cc0.ID,
		AttributionRequired: false,
		AcquiredAt:          &acquired,
		PricePaidCents:      &price,
		Currency:            "EUR",
		OriginalArchiveName: "sci-fi-rts.zip",
		OriginalArchiveSHA:  "abc123",
		State:               StateComplete,
		Notes:               "bundle",
	}
	if err := s.Update(ctx, in); err != nil {
		t.Fatalf("update: %v", err)
	}

	got, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.State != StateComplete || got.SourceAuthor != "Kenney" || got.Currency != "EUR" {
		t.Errorf("round trip lost fields: %+v", got)
	}
	if got.LicenseID == nil || *got.LicenseID != cc0.ID {
		t.Errorf("license not stored: %v", got.LicenseID)
	}
	if got.PricePaidCents == nil || *got.PricePaidCents != 1500 {
		t.Errorf("price not stored: %v", got.PricePaidCents)
	}
	if got.AcquiredAt == nil || !got.AcquiredAt.Equal(acquired) {
		t.Errorf("acquired_at not stored: %v", got.AcquiredAt)
	}
}

func TestUpdateRejectsBadState(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	id := insertPack(t, database)
	if err := s.Update(ctx, Provenance{PackID: id, State: "bogus"}); err == nil {
		t.Error("expected an error for an invalid state")
	}
}

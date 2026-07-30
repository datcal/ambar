package projects

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func fixture(t *testing.T) (*Store, *db.DB, int64) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	// A pack with provenance + an asset to use.
	cc0 := int64(1) // CC0-1.0 is the first seeded licence
	res, _ := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, source_author, source_url,
		                   license_id, attribution_text, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('Kenney RTS', 'kenney-rts', 'folder', 'itch/kenney', 'Kenney', 'https://kenney.itch.io/rts',
		        ?, 'Made by Kenney', ?, ?, ?, ?)`, cc0, now, now, now, now)
	packID, _ := res.LastInsertId()
	res, _ = database.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, 'turret.glb', 'turret.glb', 'glb', 'model', 1, ?, 'abc', ?, ?, ?, ?)`,
		packID, now, now, now, now, now)
	assetID, _ := res.LastInsertId()
	return NewStore(database), database, assetID
}

func TestRecordUseIsIdempotent(t *testing.T) {
	s, database, assetID := fixture(t)
	ctx := context.Background()
	const uuid = "11111111-1111-1111-1111-111111111111"

	id1, err := s.RecordUse(ctx, uuid, "My Game", assetID, "res://assets/models/turret.glb", "abc")
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	// Same (project, asset, res_path) again → same row, not a duplicate.
	id2, err := s.RecordUse(ctx, uuid, "My Game", assetID, "res://assets/models/turret.glb", "abc")
	if err != nil || id2 != id1 {
		t.Fatalf("second record = %d,%v; want %d", id2, err, id1)
	}
	var n int
	database.Reader.QueryRow(`SELECT count(*) FROM project_uses WHERE removed_at IS NULL`).Scan(&n)
	if n != 1 {
		t.Errorf("%d active uses, want 1", n)
	}

	// Remove then re-add reactivates the same row.
	if err := s.RemoveUse(ctx, uuid, id1); err != nil {
		t.Fatal(err)
	}
	database.Reader.QueryRow(`SELECT count(*) FROM project_uses WHERE removed_at IS NULL`).Scan(&n)
	if n != 0 {
		t.Errorf("use not removed")
	}
	id3, _ := s.RecordUse(ctx, uuid, "My Game", assetID, "res://assets/models/turret.glb", "abc")
	if id3 != id1 {
		t.Errorf("re-add made a new row %d, want %d", id3, id1)
	}
}

func TestCreditsRendering(t *testing.T) {
	s, _, assetID := fixture(t)
	ctx := context.Background()
	const uuid = "22222222-2222-2222-2222-222222222222"
	s.RecordUse(ctx, uuid, "My Game", assetID, "res://assets/models/turret.glb", "abc")

	lines, err := s.Credits(ctx, uuid)
	if err != nil {
		t.Fatalf("credits: %v", err)
	}
	if len(lines) != 1 || lines[0].License != "CC0-1.0" || lines[0].Author != "Kenney" {
		t.Fatalf("credit line wrong: %+v", lines)
	}
	md := RenderCredits("My Game", lines)
	for _, want := range []string{"# Credits — My Game", "## CC0-1.0", "Kenney RTS", "by Kenney", "kenney.itch.io/rts", "Made by Kenney"} {
		if !strings.Contains(md, want) {
			t.Errorf("CREDITS.md missing %q\n%s", want, md)
		}
	}
}

func TestRemoveUseUnknownProject(t *testing.T) {
	s, _, _ := fixture(t)
	if err := s.RemoveUse(context.Background(), "no-such-uuid", 1); err == nil {
		t.Error("expected an error removing a use from an unknown project")
	}
}

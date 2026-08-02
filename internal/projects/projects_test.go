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

// TestRecordUseResolvesByContent covers the attribution bug that produced a CREDITS.md
// naming a pack nobody had chosen: a use recorded against one asset id while the project
// actually held a different asset's bytes.
//
// The content hash travels with the import, so it can say which asset the row is really
// about — and, just as importantly, when it cannot.
func TestRecordUseResolvesByContent(t *testing.T) {
	s, database, fields := fixture(t)
	ctx := context.Background()
	const uuid = "33333333-3333-3333-3333-333333333333"

	// A second asset, in the same pack for brevity, with different content. In the case
	// this is written for the two were in different packs — which is exactly why the wrong
	// row credited the wrong licence.
	now := time.Now().Unix()
	var packID int64
	database.Reader.QueryRow(`SELECT pack_id FROM assets WHERE id = ?`, fields).Scan(&packID)
	res, err := database.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, 'shadow.png', 'Ship2_shadow1.png', 'png', 'image', 1, ?, 'seabed-content', ?, ?, ?, ?)`,
		packID, now, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	seabed, _ := res.LastInsertId()

	// Recorded against one id while carrying the hash of what was actually downloaded: the
	// row must end up on the asset the bytes came from, or the credits name the wrong pack.
	if _, err := s.RecordUse(ctx, uuid, "My Game", fields, "res://assets/image/fields/3.png",
		"seabed-content"); err != nil {
		t.Fatal(err)
	}
	uses, err := s.UsesOfProject(ctx, uuid)
	if err != nil {
		t.Fatal(err)
	}
	if len(uses) != 1 || uses[0].AssetID != seabed {
		t.Fatalf("recorded %+v, want one row against asset %d", uses, seabed)
	}

	// An id that agrees with its hash is left exactly alone.
	if _, err := s.RecordUse(ctx, uuid, "My Game", fields, "res://assets/image/fields/ok.png",
		"abc"); err != nil {
		t.Fatal(err)
	}
	// A hash nothing in the library has is an *older* copy than the library holds — §10's
	// outdated case, and what Sync replays. Recorded as given, not rejected, not re-pointed.
	if _, err := s.RecordUse(ctx, uuid, "My Game", fields, "res://assets/image/fields/old.png",
		"a-hash-from-before"); err != nil {
		t.Fatal(err)
	}

	uses, _ = s.UsesOfProject(ctx, uuid)
	byPath := map[string]ProjectUse{}
	for _, u := range uses {
		byPath[u.ResPath] = u
	}
	if got := byPath["res://assets/image/fields/ok.png"]; got.AssetID != fields {
		t.Errorf("a matching id and hash was re-pointed to %d, want %d", got.AssetID, fields)
	}
	old := byPath["res://assets/image/fields/old.png"]
	if old.AssetID != fields {
		t.Errorf("an outdated hash re-pointed the row to asset %d, want %d", old.AssetID, fields)
	}
	if !old.Outdated() {
		t.Error("an outdated row does not report itself as outdated")
	}
}

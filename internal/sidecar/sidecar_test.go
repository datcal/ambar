package sidecar

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/provenance"
	"github.com/datcal/ambar/internal/tags"
)

// pack sets up a database with one pack ("foo" at rel "foo") and one asset
// ("foo/a.png"), returning the pack and asset ids.
func pack(t *testing.T, database *db.DB) (packID, assetID int64) {
	t.Helper()
	now := time.Now().Unix()
	res, err := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('foo', 'foo', 'folder', 'foo', ?, ?, ?, ?)`, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	packID, _ = res.LastInsertId()
	res, err = database.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, 'a.png', 'a.png', 'png', 'image', 1, ?, 'h', ?, ?, ?, ?)`,
		packID, now, now, now, now, now)
	if err != nil {
		t.Fatal(err)
	}
	assetID, _ = res.LastInsertId()
	database.Writer.Exec(`INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes) VALUES (?, 'a.png', 'foo', '', '')`, assetID)
	return packID, assetID
}

func openDB(t *testing.T) *db.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return database
}

func fixedClock() func() time.Time {
	tm := time.Unix(1_700_000_000, 0)
	return func() time.Time { return tm }
}

// TestSidecarRoundTripAcrossDatabases proves the database is disposable: metadata
// written to a sidecar by one database is recovered intact by a different one.
func TestSidecarRoundTripAcrossDatabases(t *testing.T) {
	libraryRoot := t.TempDir()
	ctx := context.Background()

	// --- database A: author the metadata and write the sidecar ---
	dbA := openDB(t)
	packID, assetID := pack(t, dbA)
	provA := provenance.NewStore(dbA)
	tagsA := tags.NewStore(dbA)

	cc0, _, _ := provA.LicenseBySPDX(ctx, "CC0-1.0")
	if err := provA.Update(ctx, provenance.Provenance{
		PackID: packID, SourceURL: "https://kenney.itch.io/foo", SourceSite: "itch.io",
		SourceAuthor: "Kenney", LicenseID: &cc0.ID, State: provenance.StateComplete, Notes: "bundle",
	}); err != nil {
		t.Fatal(err)
	}
	tagsA.TagPack(ctx, packID, "author:kenney", tags.SourceManual, nil)
	tagsA.TagAsset(ctx, assetID, "theme:sci-fi", tags.SourceManual, nil)
	// An auto tag must NOT end up in the sidecar.
	tagsA.TagAsset(ctx, assetID, "type:image", tags.SourceAutoType, nil)

	mgrA := New(dbA, Options{LibraryRoot: libraryRoot}).WithClock(fixedClock())
	if err := mgrA.Write(ctx, packID, "foo"); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The sidecar exists beside the (would-be) originals and omits auto tags.
	scPath := filepath.Join(libraryRoot, "foo", FileName)
	if _, err := os.Stat(scPath); err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	sc, err := Read(scPath)
	if err != nil {
		t.Fatal(err)
	}
	if sc.Pack.License != "CC0-1.0" || sc.Pack.SourceAuthor != "Kenney" {
		t.Errorf("pack meta wrong: %+v", sc.Pack)
	}
	if len(sc.Assets) != 1 || len(sc.Assets[0].Tags) != 1 || sc.Assets[0].Tags[0] != "theme:sci-fi" {
		t.Errorf("asset tags wrong (auto leaked?): %+v", sc.Assets)
	}

	// --- database B: fresh, same library; import recovers everything ---
	dbB := openDB(t)
	packB, _ := pack(t, dbB)
	mgrB := New(dbB, Options{LibraryRoot: libraryRoot}).WithClock(fixedClock())

	scB, ok, err := mgrB.ReadForPack("foo")
	if err != nil || !ok {
		t.Fatalf("read for pack: ok=%v err=%v", ok, err)
	}
	did, err := mgrB.ImportForPack(ctx, packB, "foo", scB)
	if err != nil || !did {
		t.Fatalf("import: did=%v err=%v", did, err)
	}

	got, err := provenance.NewStore(dbB).Get(ctx, packB)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceAuthor != "Kenney" || got.State != provenance.StateComplete || got.LicenseID == nil {
		t.Errorf("provenance not restored: %+v", got)
	}
	// The manual tags are back.
	var n int
	dbB.Reader.QueryRow(`SELECT count(*) FROM pack_tags pt JOIN tags t ON t.id=pt.tag_id
		WHERE pt.pack_id=? AND t.namespace='author'`, packB).Scan(&n)
	if n != 1 {
		t.Errorf("pack tag not restored")
	}
	dbB.Reader.QueryRow(`SELECT count(*) FROM asset_tags at JOIN tags t ON t.id=at.tag_id
		JOIN assets a ON a.id=at.asset_id WHERE a.pack_id=? AND t.namespace='theme'`, packB).Scan(&n)
	if n != 1 {
		t.Errorf("asset tag not restored")
	}
}

func TestImportSkipsWhenDatabaseHasNewerMetadata(t *testing.T) {
	libraryRoot := t.TempDir()
	ctx := context.Background()
	database := openDB(t)
	packID, _ := pack(t, database)

	// The DB already has provenance.
	provenance.NewStore(database).Update(ctx, provenance.Provenance{
		PackID: packID, SourceAuthor: "DB Author", State: provenance.StateComplete,
	})

	// A sidecar that is older than the DB row must not overwrite it.
	mgr := New(database, Options{LibraryRoot: libraryRoot})
	older := Sidecar{Version: 1, UpdatedAt: 1, Pack: PackMeta{SourceAuthor: "Sidecar Author"}}
	did, err := mgr.ImportForPack(ctx, packID, "foo", older)
	if err != nil {
		t.Fatal(err)
	}
	if did {
		t.Error("imported an older sidecar over newer database metadata")
	}
	got, _ := provenance.NewStore(database).Get(ctx, packID)
	if got.SourceAuthor != "DB Author" {
		t.Errorf("database metadata was overwritten: %q", got.SourceAuthor)
	}
}

func TestReadonlyWritesToMirror(t *testing.T) {
	libraryRoot := t.TempDir()
	dataRoot := t.TempDir()
	ctx := context.Background()
	database := openDB(t)
	packID, _ := pack(t, database)

	mgr := New(database, Options{LibraryRoot: libraryRoot, DataRoot: dataRoot, Readonly: true}).WithClock(fixedClock())
	if err := mgr.Write(ctx, packID, "foo"); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Nothing under the library; the sidecar is in the data-root mirror (§3).
	if _, err := os.Stat(filepath.Join(libraryRoot, "foo", FileName)); !os.IsNotExist(err) {
		t.Errorf("read-only mode wrote into the library")
	}
	if _, err := os.Stat(filepath.Join(dataRoot, "sidecars", "foo", FileName)); err != nil {
		t.Errorf("sidecar not written to the mirror: %v", err)
	}
}

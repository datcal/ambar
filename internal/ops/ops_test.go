package ops

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/provenance"
	"github.com/datcal/ambar/internal/sidecar"
	"github.com/datcal/ambar/internal/tags"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// buildLibrary writes a small library, indexes it, adds a manual tag and
// provenance, and writes the sidecar — the state a rebuild must recover.
func buildLibrary(t *testing.T) *config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := &config.Config{
		LibraryRoot: filepath.Join(root, "library"),
		DataRoot:    filepath.Join(root, "data"),
	}
	for _, d := range []string{cfg.LibraryRoot, cfg.DataRoot} {
		os.MkdirAll(d, 0o755)
	}
	// A pack with two files.
	os.MkdirAll(filepath.Join(cfg.LibraryRoot, "kenney-rts", "Sprites"), 0o755)
	os.WriteFile(filepath.Join(cfg.LibraryRoot, "kenney-rts", "Sprites", "turret.png"), []byte("turret"), 0o644)
	os.WriteFile(filepath.Join(cfg.LibraryRoot, "kenney-rts", "Sprites", "tank.png"), []byte("tank"), 0o644)

	database, err := db.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	indexer := index.New(database, index.Options{Root: cfg.LibraryRoot})
	if _, err := indexer.Scan(ctx, index.ScanOptions{ReadDimensions: true}); err != nil {
		t.Fatal(err)
	}

	var packID, assetID int64
	database.Reader.QueryRow(`SELECT id FROM packs WHERE library_rel_path = 'kenney-rts'`).Scan(&packID)
	database.Reader.QueryRow(`SELECT id FROM assets WHERE filename = 'turret.png'`).Scan(&assetID)

	// Human metadata: a manual tag and provenance.
	ts := tags.NewStore(database)
	if _, err := ts.TagAsset(ctx, assetID, "theme:sci-fi", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}
	pr := provenance.NewStore(database)
	cc0, _, _ := pr.LicenseBySPDX(ctx, "CC0-1.0")
	pr.Update(ctx, provenance.Provenance{
		PackID: packID, SourceAuthor: "Kenney", LicenseID: &cc0.ID, State: provenance.StateComplete,
	})

	// Write the sidecar so the metadata lives beside the files.
	if _, err := sidecar.New(database, sidecar.Options{LibraryRoot: cfg.LibraryRoot, DataRoot: cfg.DataRoot}).SyncAll(ctx); err != nil {
		t.Fatal(err)
	}
	database.Close()
	return cfg
}

func TestRebuildIndexFidelity(t *testing.T) {
	cfg := buildLibrary(t)
	ctx := context.Background()

	rep, err := RebuildIndex(ctx, cfg, discard())
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rep.Packs != 1 || rep.Assets != 2 {
		t.Errorf("rebuild report = %+v, want 1 pack / 2 assets", rep)
	}

	// Reopen the rebuilt database and check the metadata came back.
	database, err := db.Open(cfg.DatabasePath())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()

	var packs, assets int
	database.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&packs)
	database.Reader.QueryRow(`SELECT count(*) FROM assets`).Scan(&assets)
	if packs != 1 || assets != 2 {
		t.Errorf("after rebuild: %d packs / %d assets, want 1 / 2", packs, assets)
	}

	// The manual tag was recovered from the sidecar.
	var manual int
	database.Reader.QueryRow(`
		SELECT count(*) FROM asset_tags at JOIN tags t ON t.id = at.tag_id
		WHERE t.namespace = 'theme' AND at.source = 'manual'`).Scan(&manual)
	if manual != 1 {
		t.Errorf("manual tag not recovered from sidecar (%d)", manual)
	}

	// Provenance was recovered.
	var author, state string
	database.Reader.QueryRow(`SELECT source_author, provenance_state FROM packs WHERE library_rel_path = 'kenney-rts'`).
		Scan(&author, &state)
	if author != "Kenney" || state != "complete" {
		t.Errorf("provenance not recovered: author=%q state=%q", author, state)
	}

	// Auto tags were regenerated (folder:sprites from the path).
	var auto int
	database.Reader.QueryRow(`
		SELECT count(*) FROM tags WHERE namespace = 'folder' AND name = 'sprites'`).Scan(&auto)
	if auto != 1 {
		t.Errorf("auto path tag not regenerated")
	}
}

func TestVerifyDetectsBitRot(t *testing.T) {
	cfg := buildLibrary(t)
	ctx := context.Background()
	// Rebuild so we have a fresh index against the current files.
	if _, err := RebuildIndex(ctx, cfg, discard()); err != nil {
		t.Fatal(err)
	}

	// Corrupt one file's bytes without changing its path.
	victim := filepath.Join(cfg.LibraryRoot, "kenney-rts", "Sprites", "turret.png")
	os.WriteFile(victim, []byte("CORRUPTED-DIFFERENT-BYTES"), 0o644)

	database, _ := db.Open(cfg.DatabasePath())
	defer database.Close()

	rep, err := Verify(ctx, database, cfg.LibraryRoot, discard())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if rep.Checked != 2 {
		t.Errorf("checked = %d, want 2", rep.Checked)
	}
	if rep.Mismatched != 1 {
		t.Errorf("mismatched = %d, want 1", rep.Mismatched)
	}
	// The change was flagged for review.
	var flagged int
	database.Reader.QueryRow(`SELECT count(*) FROM assets WHERE content_changed_at IS NOT NULL`).Scan(&flagged)
	if flagged != 1 {
		t.Errorf("content_changed_at not set (%d)", flagged)
	}
}

func TestBackup(t *testing.T) {
	cfg := buildLibrary(t)
	ctx := context.Background()
	database, _ := db.Open(cfg.DatabasePath())
	defer database.Close()

	dest := BackupPath(filepath.Join(cfg.DataRoot, "backups"), time.Unix(1700000000, 0))
	if err := Backup(ctx, database, dest); err != nil {
		t.Fatalf("backup: %v", err)
	}
	// The backup is a real, readable database.
	backup, err := db.Open(dest)
	if err != nil {
		t.Fatalf("open backup: %v", err)
	}
	defer backup.Close()
	var n int
	if err := backup.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&n); err != nil {
		t.Fatalf("backup is not a valid database: %v", err)
	}
	if n != 1 {
		t.Errorf("backup has %d packs, want 1", n)
	}
	// A second backup to the same path refuses rather than clobbering.
	if err := Backup(ctx, database, dest); err == nil {
		t.Error("backup overwrote an existing file")
	}
}

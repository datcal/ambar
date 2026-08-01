package index

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/library"
)

// fixture is a library root plus a migrated database, with helpers for the
// filesystem manipulations reconciliation has to cope with.
type fixture struct {
	t    *testing.T
	root string
	db   *db.DB
	ix   *Indexer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}

	database, err := db.Open(filepath.Join(base, "ambar.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	return &fixture{
		t:    t,
		root: root,
		db:   database,
		ix:   New(database, Options{Root: root}),
	}
}

// write creates or overwrites a file at a library-relative path.
func (f *fixture) write(relPath, content string) {
	f.t.Helper()

	full := filepath.Join(f.root, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// touch advances a file's mtime without changing its content, which is what a
// copy or an rsync does.
func (f *fixture) touch(relPath string) {
	f.t.Helper()

	full := filepath.Join(f.root, filepath.FromSlash(relPath))
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(full, later, later); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) move(from, to string) {
	f.t.Helper()

	dst := filepath.Join(f.root, filepath.FromSlash(to))
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(f.root, filepath.FromSlash(from)), dst); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(relPath string) {
	f.t.Helper()
	if err := os.RemoveAll(filepath.Join(f.root, filepath.FromSlash(relPath))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) scan() *ScanReport {
	f.t.Helper()

	report, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		f.t.Fatalf("scan: %v", err)
	}
	for _, e := range report.Errors {
		f.t.Errorf("scan reported an error: %v", e)
	}
	return report
}

// assetCount counts rows, including missing ones, since §12 never deletes them.
func (f *fixture) assetCount() int {
	f.t.Helper()

	var n int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM assets`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// assetByPath finds a row by its library-relative path.
func (f *fixture) assetByPath(libPath string) (Asset, bool) {
	f.t.Helper()

	page, err := f.ix.List(context.Background(), ListOptions{IncludeMissing: true, Limit: MaxPageSize})
	if err != nil {
		f.t.Fatal(err)
	}
	for _, a := range page.Assets {
		if a.LibraryPath() == libPath {
			return a, true
		}
	}
	return Asset{}, false
}

// --- the required reconciliation table (CLAUDE.md) -------------------------

func TestReconcileNewFiles(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite_a.png", "a")
	f.write("pack/sprite_b.png", "b")

	r := f.scan()

	if r.Added != 2 {
		t.Errorf("Added = %d, want 2", r.Added)
	}
	if r.FilesSeen != 2 {
		t.Errorf("FilesSeen = %d, want 2", r.FilesSeen)
	}
	if r.PacksFound != 1 || r.PacksNew != 1 {
		t.Errorf("packs found/new = %d/%d, want 1/1", r.PacksFound, r.PacksNew)
	}
	if r.Hashed != 2 {
		t.Errorf("Hashed = %d, want 2", r.Hashed)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows, want 2", got)
	}

	// The stored hash must be the real one.
	a, ok := f.assetByPath("pack/sprite_a.png")
	if !ok {
		t.Fatal("sprite_a.png was not indexed")
	}
	want := sha256.Sum256([]byte("a"))
	if a.SHA256 != hex.EncodeToString(want[:]) {
		t.Errorf("sha256 = %q, want %x", a.SHA256, want)
	}
}

// TestReconcileIsIdempotent: the single most important property of a rescan.
func TestReconcileIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "a")
	f.write("pack/PNG/deep.png", "b")
	f.scan()

	r := f.scan()

	if r.Unchanged != 2 {
		t.Errorf("Unchanged = %d, want 2", r.Unchanged)
	}
	for name, got := range map[string]int{
		"Added": r.Added, "Moved": r.Moved, "MarkedMissing": r.MarkedMissing,
		"ContentChanged": r.ContentChanged, "MetadataOnly": r.MetadataOnly,
		"Reappeared": r.Reappeared, "PacksNew": r.PacksNew,
	} {
		if got != 0 {
			t.Errorf("a no-op rescan reported %s = %d, want 0", name, got)
		}
	}
	// And nothing was re-hashed: that is the whole point of the (size, mtime)
	// fast path, and why `ambar verify` exists separately (§12).
	if r.Hashed != 0 {
		t.Errorf("Hashed = %d on a no-op rescan, want 0 — the fast path is not working", r.Hashed)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows after rescan, want 2", got)
	}
}

// TestReconcileContentChanged is §12's "changed hashes flagged for review".
func TestReconcileContentChanged(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "original")
	f.scan()

	before, _ := f.assetByPath("pack/sprite.png")
	f.write("pack/sprite.png", "edited, and a different length")

	r := f.scan()

	if r.ContentChanged != 1 {
		t.Errorf("ContentChanged = %d, want 1", r.ContentChanged)
	}
	if r.Added != 0 || r.Moved != 0 {
		t.Errorf("an in-place edit was reported as Added=%d Moved=%d", r.Added, r.Moved)
	}

	after, ok := f.assetByPath("pack/sprite.png")
	if !ok {
		t.Fatal("the asset disappeared")
	}
	if after.ID != before.ID {
		t.Errorf("the row id changed from %d to %d; an edit must update in place", before.ID, after.ID)
	}
	if after.SHA256 == before.SHA256 {
		t.Error("the stored hash was not updated")
	}
	if after.ContentChangedAt == nil {
		t.Error("content_changed_at was not set; §12 wants changed hashes flagged for review")
	}
}

// TestReconcileMetadataOnly: a touched mtime with identical bytes is not a content
// change and must not raise the review flag.
func TestReconcileMetadataOnly(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "unchanging content")
	f.scan()

	f.touch("pack/sprite.png")

	r := f.scan()

	if r.MetadataOnly != 1 {
		t.Errorf("MetadataOnly = %d, want 1", r.MetadataOnly)
	}
	if r.ContentChanged != 0 {
		t.Errorf("ContentChanged = %d, want 0 — the bytes did not change", r.ContentChanged)
	}
	// It did have to be hashed to establish that.
	if r.Hashed != 1 {
		t.Errorf("Hashed = %d, want 1", r.Hashed)
	}

	a, _ := f.assetByPath("pack/sprite.png")
	if a.ContentChangedAt != nil {
		t.Error("content_changed_at was set for an unchanged file")
	}
}

// TestReconcileMoveIsNotADuplicate is §9.1 rule 2, the rule that section states
// most emphatically: "This is a move, not a duplicate. Never report it as one;
// update the row."
func TestReconcileMoveIsNotADuplicate(t *testing.T) {
	f := newFixture(t)
	f.write("pack/old/sprite.png", "the same bytes either way")
	f.scan()

	before, ok := f.assetByPath("pack/old/sprite.png")
	if !ok {
		t.Fatal("not indexed")
	}

	f.move("pack/old/sprite.png", "pack/new/sprite.png")

	r := f.scan()

	if r.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r.Moved)
	}
	if r.Added != 0 {
		t.Errorf("Added = %d, want 0 — a move must not insert a new row", r.Added)
	}
	if r.MarkedMissing != 0 {
		t.Errorf("MarkedMissing = %d, want 0 — a move must not mark the old path missing", r.MarkedMissing)
	}
	if got := f.assetCount(); got != 1 {
		t.Errorf("%d rows after a move, want 1 — a second row is the duplicate §9.1 forbids", got)
	}

	after, ok := f.assetByPath("pack/new/sprite.png")
	if !ok {
		t.Fatal("the asset is not at its new path")
	}
	// Identity must survive, because from M9 project_uses references it and §9.1
	// makes anything referenced there undeletable.
	if after.ID != before.ID {
		t.Errorf("the row id changed from %d to %d across a move", before.ID, after.ID)
	}
	if after.Missing() {
		t.Error("the moved asset is marked missing")
	}
	if _, stillThere := f.assetByPath("pack/old/sprite.png"); stillThere {
		t.Error("the old path is still in the index")
	}
}

// TestReconcileMoveAcrossPacks covers a move that also changes the owning pack.
func TestReconcileMoveAcrossPacks(t *testing.T) {
	f := newFixture(t)
	f.write("pack-a/sprite.png", "moving between packs")
	f.write("pack-b/other.png", "unrelated")
	f.scan()

	before, _ := f.assetByPath("pack-a/sprite.png")
	f.move("pack-a/sprite.png", "pack-b/sprite.png")

	r := f.scan()

	if r.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r.Moved)
	}
	after, ok := f.assetByPath("pack-b/sprite.png")
	if !ok {
		t.Fatal("not found at the new pack")
	}
	if after.ID != before.ID {
		t.Error("the row id changed across a cross-pack move")
	}
	if after.PackID == before.PackID {
		t.Error("pack_id was not updated")
	}
	if after.PackRelPath != "pack-b" {
		t.Errorf("pack path = %q, want pack-b", after.PackRelPath)
	}
}

// TestReconcileMissingIsMarkedNotDeleted is §12's catastrophic case.
func TestReconcileMissingIsMarkedNotDeleted(t *testing.T) {
	f := newFixture(t)
	f.write("pack/keep.png", "keep")
	f.write("pack/gone.png", "gone")
	f.scan()

	before, _ := f.assetByPath("pack/gone.png")
	f.remove("pack/gone.png")

	r := f.scan()

	if r.MarkedMissing != 1 {
		t.Errorf("MarkedMissing = %d, want 1", r.MarkedMissing)
	}
	// The row must survive. This is the assertion §12 is written around.
	if got := f.assetCount(); got != 2 {
		t.Fatalf("%d rows after a file went missing, want 2 — the row must never be deleted", got)
	}

	after, ok := f.assetByPath("pack/gone.png")
	if !ok {
		t.Fatal("the row for the missing file was deleted")
	}
	if after.ID != before.ID {
		t.Error("the row was replaced rather than marked")
	}
	if !after.Missing() {
		t.Error("missing_since was not set")
	}

	// And a missing asset is out of the default browse, since it is not there.
	page, err := f.ix.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("the default listing shows %d assets, want 1 (missing excluded)", page.Total)
	}
	withMissing, err := f.ix.List(context.Background(), ListOptions{IncludeMissing: true})
	if err != nil {
		t.Fatal(err)
	}
	if withMissing.Total != 2 {
		t.Errorf("IncludeMissing showed %d assets, want 2", withMissing.Total)
	}
}

// TestReconcileReappearance: an unmounted share coming back must restore the index
// rather than leave everything flagged.
func TestReconcileReappearance(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "content")
	f.write("pack/other.png", "other")
	f.scan()

	before, _ := f.assetByPath("pack/sprite.png")

	// Squirrel the file away, scan, then bring it back.
	full := filepath.Join(f.root, "pack", "sprite.png")
	saved := filepath.Join(t.TempDir(), "sprite.png")
	if err := os.Rename(full, saved); err != nil {
		t.Fatal(err)
	}
	if r := f.scan(); r.MarkedMissing != 1 {
		t.Fatalf("MarkedMissing = %d, want 1", r.MarkedMissing)
	}
	if err := os.Rename(saved, full); err != nil {
		t.Fatal(err)
	}

	r := f.scan()

	if r.Reappeared != 1 {
		t.Errorf("Reappeared = %d, want 1", r.Reappeared)
	}
	if r.Added != 0 {
		t.Errorf("Added = %d, want 0 — a returning file is the same asset", r.Added)
	}

	after, _ := f.assetByPath("pack/sprite.png")
	if after.Missing() {
		t.Error("missing_since was not cleared")
	}
	if after.ID != before.ID {
		t.Error("the returning file got a new row id")
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows, want 2", got)
	}
}

// TestReconcileGenuineDuplicatesStayTwoRows is the counterpart to move detection:
// the same bytes at two paths that BOTH exist are two assets, and §9.1 rule 1 says
// exact duplicates are reported, not silently merged.
func TestReconcileGenuineDuplicatesStayTwoRows(t *testing.T) {
	f := newFixture(t)
	f.write("pack-a/sprite.png", "identical bytes")
	f.write("pack-b/sprite.png", "identical bytes")

	r := f.scan()

	if r.Added != 2 {
		t.Errorf("Added = %d, want 2", r.Added)
	}
	if r.Moved != 0 {
		t.Errorf("Moved = %d, want 0 — neither copy is a move", r.Moved)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows, want 2 — duplicates are reported, not merged", got)
	}

	// Both rows share a hash, which is what M13's duplicate finder will key on.
	var n int
	if err := f.db.Reader.QueryRow(
		`SELECT count(*) FROM assets a WHERE (SELECT count(*) FROM assets b WHERE b.sha256 = a.sha256) > 1`,
	).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d rows share a hash, want 2", n)
	}
}

// TestReconcileMoveWithADuplicateLeftBehind is the ambiguous case: two copies
// existed, one was deleted. That is a deletion, not a move, because the content is
// still present at its original path.
func TestReconcileMoveWithADuplicateLeftBehind(t *testing.T) {
	f := newFixture(t)
	f.write("pack/a.png", "shared bytes")
	f.write("pack/b.png", "shared bytes")
	f.scan()

	f.remove("pack/b.png")

	r := f.scan()

	if r.Moved != 0 {
		t.Errorf("Moved = %d, want 0 — nothing new appeared, so nothing moved", r.Moved)
	}
	if r.MarkedMissing != 1 {
		t.Errorf("MarkedMissing = %d, want 1", r.MarkedMissing)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows, want 2", got)
	}
}

// TestReconcileRenameInPlace: same directory, new filename. Still a move.
func TestReconcileRenameInPlace(t *testing.T) {
	f := newFixture(t)
	f.write("pack/old_name.png", "bytes")
	f.scan()

	before, _ := f.assetByPath("pack/old_name.png")
	f.move("pack/old_name.png", "pack/new_name.png")

	r := f.scan()

	if r.Moved != 1 {
		t.Errorf("Moved = %d, want 1", r.Moved)
	}
	after, ok := f.assetByPath("pack/new_name.png")
	if !ok {
		t.Fatal("not found under the new name")
	}
	if after.ID != before.ID {
		t.Error("a rename changed the row id")
	}
	if after.Filename != "new_name.png" {
		t.Errorf("filename = %q, want new_name.png", after.Filename)
	}

	// The FTS row must follow, or search still finds the old name.
	page, err := f.ix.List(context.Background(), ListOptions{Query: "new_name"})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 {
		t.Errorf("searching the new name found %d assets, want 1", page.Total)
	}
	stale, err := f.ix.List(context.Background(), ListOptions{Query: "old_name"})
	if err != nil {
		t.Fatal(err)
	}
	if stale.Total != 0 {
		t.Errorf("searching the old name still found %d assets, want 0", stale.Total)
	}
}

// --- the mass-missing guard ------------------------------------------------

// TestScanRefusesWhenTheLibraryLooksUnmounted is the §12 safety rail: a share that
// has gone away must not silently flag the whole index.
func TestScanRefusesWhenTheLibraryLooksUnmounted(t *testing.T) {
	f := newFixture(t)
	f.write("pack/a.png", "a")
	f.write("pack/b.png", "b")
	f.scan()

	// Empty the library, as an unmounted bind mount would appear.
	f.remove("pack")

	_, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err == nil {
		t.Fatal("a scan of an empty library against a populated index was allowed")
	}
	for _, want := range []string{"refusing", "unmounted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}

	// And the index is untouched — nothing marked, nothing deleted.
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows, want 2", got)
	}
	page, err := f.ix.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("%d assets still listed, want 2 — the refused scan changed the index", page.Total)
	}
}

// TestScanAllowsAnEmptyLibraryOnAFreshIndex: the guard must not block a first run.
func TestScanAllowsAnEmptyLibraryOnAFreshIndex(t *testing.T) {
	f := newFixture(t)

	r, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("scanning an empty library with an empty index failed: %v", err)
	}
	if r.FilesSeen != 0 || r.Added != 0 {
		t.Errorf("report = %+v, want all zero", r)
	}
}

// --- dry run ----------------------------------------------------------------

func TestDryRunWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "a")

	report, err := f.ix.Scan(context.Background(), ScanOptions{DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DryRun {
		t.Error("the report does not say it was a dry run")
	}
	if report.Added != 1 {
		t.Errorf("Added = %d, want 1 — a dry run still reports what it would do", report.Added)
	}
	if got := f.assetCount(); got != 0 {
		t.Errorf("%d rows after a dry run, want 0", got)
	}

	var packs int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&packs); err != nil {
		t.Fatal(err)
	}
	if packs != 0 {
		t.Errorf("%d packs after a dry run, want 0", packs)
	}
}

// --- invariant 1: originals are never modified ------------------------------

// TestScanNeverModifiesTheLibrary is §16's requirement made concrete for M1:
// "Add a test that hashes the entire library before and after a full
// ingest-and-scan cycle and asserts nothing changed."
func TestScanNeverModifiesTheLibrary(t *testing.T) {
	f := newFixture(t)
	f.write("craftpix-net-695666-free-undead-tileset/PNG/tile_01.png", "tile one")
	f.write("craftpix-net-695666-free-undead-tileset/PSD/tile_01.psd", "layered")
	f.write("craftpix-net-695666-free-undead-tileset/README.txt", "docs")
	f.write("3d/kenney-sci-fi/Models/turret.glb", "model bytes")
	f.write("audio/freesound/impact.wav", "audio bytes")
	f.write("stray.png", "loose file")

	before := snapshotTree(t, f.root)

	f.scan()
	f.scan() // twice, in case a second pass behaves differently

	after := snapshotTree(t, f.root)

	if len(before) != len(after) {
		t.Fatalf("the file set changed: %d entries before, %d after", len(before), len(after))
	}
	for path, b := range before {
		a, ok := after[path]
		if !ok {
			t.Errorf("%s disappeared during the scan", path)
			continue
		}
		if a.sha256 != b.sha256 {
			t.Errorf("%s content changed during the scan", path)
		}
		if a.mode != b.mode {
			t.Errorf("%s mode changed from %v to %v", path, b.mode, a.mode)
		}
		if !a.modTime.Equal(b.modTime) {
			t.Errorf("%s mtime changed from %v to %v", path, b.modTime, a.modTime)
		}
		if a.size != b.size {
			t.Errorf("%s size changed", path)
		}
	}

	// And nothing new was written into the library — no sidecars, no lock files.
	// M4 introduces sidecar writing; M1 must be strictly read-only.
	for path := range after {
		if _, ok := before[path]; !ok {
			t.Errorf("the scan created %s inside the library", path)
		}
	}
}

type treeEntry struct {
	sha256  string
	size    int64
	mode    os.FileMode
	modTime time.Time
}

func snapshotTree(t *testing.T, root string) map[string]treeEntry {
	t.Helper()

	out := map[string]treeEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := treeEntry{size: info.Size(), mode: info.Mode(), modTime: info.ModTime()}
		if !d.IsDir() {
			content, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(content)
			entry.sha256 = hex.EncodeToString(sum[:])
		}
		out[filepath.ToSlash(rel)] = entry
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// --- broken and awkward inputs (§16) ---------------------------------------

func TestScanHandlesBrokenFiles(t *testing.T) {
	f := newFixture(t)

	f.write("pack/good.png", "fine")
	f.write("pack/zero-byte.png", "")
	f.write("pack/noextension", "content")
	f.write("pack/weird name & chars.png", "content")
	f.write("pack/PNG_Parts&Spriter_Animation/rig.scml", "spriter")
	f.write("pack/Ünïcödé.png", "unicode")

	r := f.scan()

	if r.Added != 6 {
		t.Errorf("Added = %d, want 6", r.Added)
	}
	if len(r.Errors) != 0 {
		t.Errorf("errors on legal-but-awkward files: %v", r.Errors)
	}

	// A zero-byte file has a valid, well-known hash and must still be indexed.
	if a, ok := f.assetByPath("pack/zero-byte.png"); !ok {
		t.Error("the zero-byte file was not indexed")
	} else if a.Size != 0 {
		t.Errorf("size = %d, want 0", a.Size)
	} else if a.SHA256 != "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855" {
		t.Errorf("the zero-byte hash is wrong: %s", a.SHA256)
	}

	// An extensionless file classifies as `other`, not as an error.
	if a, ok := f.assetByPath("pack/noextension"); !ok {
		t.Error("the extensionless file was not indexed")
	} else if a.Kind != "other" || a.Ext != "" {
		t.Errorf("kind/ext = %q/%q, want other/\"\"", a.Kind, a.Ext)
	}
}

// TestScanReportsUnreadableFilesWithoutAborting: §16 wants broken inputs handled,
// and one bad file must not abandon a 20,000-file scan.
func TestScanReportsUnreadableFilesWithoutAborting(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a 0000 file")
	}
	f := newFixture(t)
	f.write("pack/readable.png", "fine")
	f.write("pack/locked.png", "secret")

	if err := os.Chmod(filepath.Join(f.root, "pack", "locked.png"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.Chmod(filepath.Join(f.root, "pack", "locked.png"), 0o644) //nolint:errcheck
	})

	report, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("the scan aborted over one unreadable file: %v", err)
	}
	if report.Added != 1 {
		t.Errorf("Added = %d, want 1 — the readable file must still be indexed", report.Added)
	}
	if len(report.Errors) == 0 {
		t.Error("the unreadable file was not reported")
	}
}

// --- dimensions -------------------------------------------------------------

func TestScanReadsDimensionsWhenAsked(t *testing.T) {
	f := newFixture(t)

	full := filepath.Join(f.root, "pack", "sprite.png")
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, testPNG(t, 48, 24), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := f.ix.Scan(context.Background(), ScanOptions{ReadDimensions: true}); err != nil {
		t.Fatal(err)
	}

	a, ok := f.assetByPath("pack/sprite.png")
	if !ok {
		t.Fatal("not indexed")
	}
	if a.Width != 48 || a.Height != 24 {
		t.Errorf("dimensions = %dx%d, want 48x24", a.Width, a.Height)
	}
	if a.Dimensions() != "48×24" {
		t.Errorf("Dimensions() = %q", a.Dimensions())
	}
}

// --- junk and reserved directories -----------------------------------------

func TestScanExcludesJunkAndReserved(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "real")
	f.write("pack/__MACOSX/PNG/._sprite.png", "shadow")
	f.write("pack/.DS_Store", "junk")
	f.write("_inbox/incoming.zip", "pending")
	f.write("_trash/old/sprite.png", "deleted")

	r := f.scan()

	if r.Added != 1 {
		t.Errorf("Added = %d, want 1", r.Added)
	}
	if r.IgnoredJunk == 0 {
		t.Error("no junk was reported, so the summary cannot warn about __MACOSX")
	}
	if len(r.ReservedSkipped) != 2 {
		t.Errorf("reserved skipped = %v, want 2 entries", r.ReservedSkipped)
	}
}

// --- pack bookkeeping -------------------------------------------------------

func TestPacksAreUpsertedNotDuplicated(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "a")
	f.scan()
	f.scan()
	f.scan()

	var n int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("%d pack rows after three scans, want 1", n)
	}

	var name, slug, kind string
	if err := f.db.Reader.QueryRow(
		`SELECT name, slug, kind FROM packs`).Scan(&name, &slug, &kind); err != nil {
		t.Fatal(err)
	}
	if name != "pack" || slug != "pack" || kind != "folder" {
		t.Errorf("pack = %q/%q/%q", name, slug, kind)
	}
}

// TestPackSurvivesItsFilesGoingMissing: §12 keeps the asset rows, so the pack that
// owns them has to stay too.
func TestPackSurvivesItsFilesGoingMissing(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "a")
	f.write("other/keep.png", "b")
	f.scan()

	f.remove("pack")
	f.scan()

	var n int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("%d pack rows, want 2 — a pack whose files are missing must not be deleted", n)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d asset rows, want 2", got)
	}
}

// --- search and pagination --------------------------------------------------

func TestSearchFindsTokensInsideFilenames(t *testing.T) {
	f := newFixture(t)
	f.write("kenney-medieval/wooden_sword_01.glb", "a")
	f.write("kenney-scifi/laser_turret_a.glb", "b")
	f.write("kenney-scifi/ui_atlas.png", "c")
	f.scan()

	tests := []struct {
		query string
		want  int
	}{
		// The tokenizer splits on _ and . — this is why tokenchars '_' was rejected.
		{"sword", 1},
		{"wooden", 1},
		{"glb", 2},
		{"turret", 1},
		// Prefix matching, so a partial word works as the user types.
		{"swo", 1},
		{"lase", 1},
		// Implicit AND across tokens.
		{"wooden sword", 1},
		{"wooden turret", 0},
		// The pack name is searchable too.
		{"kenney", 3},
		{"scifi", 2},
		{"medieval", 1},
		// Case-insensitive.
		{"SWORD", 1},
		{"Kenney", 3},
		// Nothing.
		{"zzznothing", 0},
	}
	for _, tc := range tests {
		t.Run(tc.query, func(t *testing.T) {
			page, err := f.ix.List(context.Background(), ListOptions{Query: tc.query})
			if err != nil {
				t.Fatalf("search %q: %v", tc.query, err)
			}
			if page.Total != tc.want {
				got := make([]string, 0, len(page.Assets))
				for _, a := range page.Assets {
					got = append(got, a.Filename)
				}
				t.Errorf("search %q found %d, want %d: %v", tc.query, page.Total, tc.want, got)
			}
		})
	}
}

// TestSearchNeverErrorsOnHostileInput is the direct consequence of the M0 spike:
// FTS5 returns syntax errors for unbalanced quotes and stray operators, so a
// search box that passed input through would 500 on a lone double quote.
func TestSearchNeverErrorsOnHostileInput(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sword.png", "a")
	f.scan()

	hostile := []string{
		`"`, `""`, `"unterminated`, `AND`, `OR`, `NOT`, `NEAR(`, `(((`, `)))`,
		`*`, `**`, `-`, `^`, `:`, `filename:`, `{`, `}`, `sword"`, `sword OR`,
		`sword AND AND`, `NEAR(a b`, `\`, `%`, `_`, `'`, `';DROP TABLE assets;--`,
		strings.Repeat("a", 5000),
		"\x00", "sword\x00", "😀", "  ", "\t\n",
	}
	for _, q := range hostile {
		page, err := f.ix.List(context.Background(), ListOptions{Query: q})
		if err != nil {
			t.Errorf("search %q returned an error: %v", truncate(q), err)
			continue
		}
		if page == nil {
			t.Errorf("search %q returned no page", truncate(q))
		}
	}

	// And the table is still there after the injection attempt.
	if got := f.assetCount(); got != 1 {
		t.Errorf("%d rows after hostile input, want 1", got)
	}
}

func TestPaginationWalksEveryRowExactlyOnce(t *testing.T) {
	f := newFixture(t)
	const total = 250
	for i := 0; i < total; i++ {
		f.write(fmt.Sprintf("pack/sprite_%04d.png", i), fmt.Sprintf("content %d", i))
	}
	f.scan()

	seen := map[int64]bool{}
	var order []string
	cursor := ""
	pages := 0
	for {
		page, err := f.ix.List(context.Background(), ListOptions{Limit: 40, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.Total != total {
			t.Errorf("Total = %d, want %d", page.Total, total)
		}
		for _, a := range page.Assets {
			if seen[a.ID] {
				t.Fatalf("asset %d appeared on two pages", a.ID)
			}
			seen[a.ID] = true
			order = append(order, a.Filename)
		}
		pages++
		if pages > 20 {
			t.Fatal("pagination did not terminate")
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	if len(seen) != total {
		t.Errorf("paged through %d assets, want %d", len(seen), total)
	}
	if !sort.StringsAreSorted(order) {
		t.Error("results are not in filename order across pages")
	}
}

func TestPaginationRejectsMalformedCursor(t *testing.T) {
	f := newFixture(t)
	f.write("pack/a.png", "a")
	f.scan()

	for _, cursor := range []string{"nonsense", "!!!!", "YWJj"} {
		if _, err := f.ix.List(context.Background(), ListOptions{Cursor: cursor}); err == nil {
			t.Errorf("cursor %q was accepted", cursor)
		}
	}
}

func TestListFiltersByKindAndPack(t *testing.T) {
	f := newFixture(t)
	f.write("pack-a/sprite.png", "a")
	f.write("pack-a/turret.glb", "b")
	f.write("pack-b/impact.wav", "c")
	f.scan()

	byKind, err := f.ix.List(context.Background(), ListOptions{Kind: "model"})
	if err != nil {
		t.Fatal(err)
	}
	if byKind.Total != 1 || byKind.Assets[0].Filename != "turret.glb" {
		t.Errorf("kind=model returned %d assets", byKind.Total)
	}

	all, err := f.ix.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var packA int64
	for _, a := range all.Assets {
		if a.PackRelPath == "pack-a" {
			packA = a.PackID
		}
	}
	byPack, err := f.ix.List(context.Background(), ListOptions{PackID: packA})
	if err != nil {
		t.Fatal(err)
	}
	if byPack.Total != 2 {
		t.Errorf("pack filter returned %d assets, want 2", byPack.Total)
	}
}

func TestGet(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "a")
	f.scan()

	page, err := f.ix.List(context.Background(), ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	id := page.Assets[0].ID

	a, err := f.ix.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if a.Filename != "sprite.png" {
		t.Errorf("filename = %q", a.Filename)
	}
	if a.LibraryPath() != "pack/sprite.png" {
		t.Errorf("LibraryPath() = %q", a.LibraryPath())
	}

	if _, err := f.ix.Get(context.Background(), 999999); err == nil {
		t.Error("Get on a nonexistent id succeeded")
	}
}

func TestStats(t *testing.T) {
	f := newFixture(t)
	f.write("pack/sprite.png", "aaa")
	f.write("pack/turret.glb", "bbbb")
	f.write("pack/gone.wav", "ccccc")
	f.scan()
	f.remove("pack/gone.wav")
	f.scan()

	s, err := f.ix.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Assets != 3 {
		t.Errorf("Assets = %d, want 3 (missing rows still count)", s.Assets)
	}
	if s.Missing != 1 {
		t.Errorf("Missing = %d, want 1", s.Missing)
	}
	if s.Packs != 1 {
		t.Errorf("Packs = %d, want 1", s.Packs)
	}
	if s.TotalBytes != 12 {
		t.Errorf("TotalBytes = %d, want 12", s.TotalBytes)
	}
	// The kind breakdown excludes missing assets, since it drives the browse UI.
	byKind := map[string]int{}
	for _, kc := range s.ByKind {
		byKind[kc.Kind] = kc.Count
	}
	if byKind["image"] != 1 || byKind["model"] != 1 {
		t.Errorf("ByKind = %v", byKind)
	}
	if byKind["audio"] != 0 {
		t.Errorf("the missing audio asset is still in ByKind: %v", byKind)
	}
}

func truncate(s string) string {
	if len(s) > 40 {
		return s[:40] + "..."
	}
	return s
}

// testPNG builds a real PNG so DecodeConfig has a genuine header to read.
func testPNG(t *testing.T, w, h int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 0x40, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestTrashIsIgnoredAndPurgedIfPreviouslyIndexed covers both halves of the change.
//
// The target NAS had a `.Trash-1000` at the library root with a deleted pack inside it,
// and the ignore list did not know the name — so a scan indexed deleted files and served
// them as search results. Adding the glob stops new ones. It does not help the rows that
// were already there: they simply stop appearing in the walk, which the reconciler reads
// as "the file is gone" and records as missing, forever, in the same banner that warns
// about an unmounted share. So a path that is now ignored has its row dropped instead.
func TestTrashIsIgnoredAndPurgedIfPreviouslyIndexed(t *testing.T) {
	f := newFixture(t)

	// Indexed before the ignore list knew the name: an Indexer whose matcher has none
	// of the NAS rules is exactly the old behaviour.
	permissive, err := library.NewMatcher([]string{"__MACOSX"})
	if err != nil {
		t.Fatal(err)
	}
	f.write("2d/pack/hero.png", "hero")
	// A second real asset, so removing the first later does not empty the library and
	// trip the "this looks like an unmounted share" guard — which is itself the right
	// behaviour, and worth not disabling for a test.
	f.write("2d/pack/villain.png", "villain")
	f.write("2d/.Trash-1000/files/oldpack/deleted.png", "deleted art")
	f.write("3d/kenney/@eaDir/thumb.png", "synology thumbnail")

	old := New(f.db, Options{Root: f.root, Ignore: permissive})
	if _, err := old.Scan(context.Background(), ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if got := f.assetCount(); got != 4 {
		t.Fatalf("the permissive scan indexed %d assets, want 4 (the fixture is wrong)", got)
	}

	// Now scan with the real defaults.
	report := f.scan()

	if report.PurgedIgnored != 2 {
		t.Errorf("purged %d rows, want 2 (the trash file and the @eaDir thumbnail)", report.PurgedIgnored)
	}
	if report.MarkedMissing != 0 {
		t.Errorf("marked %d missing; junk must be dropped, not parked in the missing banner",
			report.MarkedMissing)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d assets remain, want 2 — only the real ones", got)
	}

	// The files themselves are untouched: invariant 1 and invariant 3 are about bytes on
	// disk, and this only ever removed rows.
	for _, rel := range []string{"2d/.Trash-1000/files/oldpack/deleted.png", "3d/kenney/@eaDir/thumb.png"} {
		if _, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s was removed from disk; nothing here may delete a file: %v", rel, err)
		}
	}

	// A real file that goes missing still goes missing — the purge must not have
	// swallowed that path.
	f.remove("2d/pack/hero.png")
	if report := f.scan(); report.MarkedMissing != 1 || report.PurgedIgnored != 0 {
		t.Errorf("a genuinely absent file: missing=%d purged=%d, want 1 and 0",
			report.MarkedMissing, report.PurgedIgnored)
	}

	// Scanning again is quiet: nothing left to purge, nothing new to report.
	if report := f.scan(); report.PurgedIgnored != 0 {
		t.Errorf("a second scan purged %d more rows; the first should have been complete",
			report.PurgedIgnored)
	}
}

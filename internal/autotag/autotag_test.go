package autotag

import (
	"context"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/tags"
)

func TestPathTags(t *testing.T) {
	got := PathTags("PNG/Environment/Rocks/idle.png")
	want := []string{"folder:environment", "folder:rocks"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PathTags = %v, want %v", got, want)
	}
}

func TestTypeTags(t *testing.T) {
	got := TypeTags("image", true, true, 4)
	want := []string{"type:image", "style:pixel-art", "has:alpha", "has:animation"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TypeTags = %v, want %v", got, want)
	}
	if got := TypeTags("model", false, false, 1); !reflect.DeepEqual(got, []string{"type:model"}) {
		t.Errorf("TypeTags(model) = %v", got)
	}
}

// newFixture opens a migrated DB and inserts one pack.
func newFixture(t *testing.T) (*db.DB, *tags.Store, int64) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "autotag.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	now := time.Now().Unix()
	res, err := database.Writer.Exec(`
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES ('pack', 'pack', 'folder', 'pack', ?, ?, ?, ?)`, now, now, now, now)
	if err != nil {
		t.Fatalf("insert pack: %v", err)
	}
	packID, _ := res.LastInsertId()
	return database, tags.NewStore(database), packID
}

// insertAsset adds an asset row and its empty FTS row, with the given analysis flags.
func insertAsset(t *testing.T, database *db.DB, packID int64, relPath, kind string, pixelArt, hasAlpha bool, frames int) int64 {
	t.Helper()
	now := time.Now().Unix()
	filename := filepath.Base(relPath)
	res, err := database.Writer.Exec(`
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    is_pixel_art, has_alpha, frame_count,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', ?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		packID, relPath, filename, kind, now, relPath+"-hash",
		boolToInt(pixelArt), boolToInt(hasAlpha), frames, now, now, now, now)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	id, _ := res.LastInsertId()
	if _, err := database.Writer.Exec(`
		INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes)
		VALUES (?, ?, 'pack', '', '')`, id, filename); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	return id
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func canonicals(ats []tags.AssetTag) []string {
	out := make([]string, 0, len(ats))
	for _, at := range ats {
		out = append(out, at.Tag.Canonical())
	}
	sort.Strings(out)
	return out
}

func TestRetagAppliesPathAndTypeTags(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	asset := insertAsset(t, database, packID, "Environment/Rocks/boulder.png", "image", true, true, 1)

	tagger := New(database, store, nil)
	rep, err := tagger.Retag(ctx)
	if err != nil {
		t.Fatalf("retag: %v", err)
	}
	if rep.Assets != 1 || rep.AssetsTagged != 1 {
		t.Errorf("report = %+v", rep)
	}

	ats, err := store.AssetTags(ctx, asset)
	if err != nil {
		t.Fatalf("asset tags: %v", err)
	}
	got := canonicals(ats)
	want := []string{"folder:environment", "folder:rocks", "has:alpha", "style:pixel-art", "type:image"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tags = %v, want %v", got, want)
	}
	for _, at := range ats {
		wantSource := tags.SourceAutoType
		if len(at.Tag.Namespace) >= 6 && at.Tag.Namespace == "folder" {
			wantSource = tags.SourceAutoPath
		}
		if at.Source != wantSource {
			t.Errorf("%s source = %q, want %q", at.Tag.Canonical(), at.Source, wantSource)
		}
	}
}

func TestRetagIsIdempotentAndPreservesManual(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	asset := insertAsset(t, database, packID, "Rocks/boulder.png", "image", false, false, 1)

	// A person manually applies the same tag the path pass would produce.
	if _, err := store.TagAsset(ctx, asset, "folder:rocks", tags.SourceManual, nil); err != nil {
		t.Fatalf("manual tag: %v", err)
	}

	tagger := New(database, store, nil)
	if _, err := tagger.Retag(ctx); err != nil {
		t.Fatalf("retag: %v", err)
	}
	// A second run must change nothing.
	rep, err := tagger.Retag(ctx)
	if err != nil {
		t.Fatalf("retag 2: %v", err)
	}
	if rep.AssetsTagged != 1 { // still applies (no-op upserts), but count is stable
		t.Errorf("second run report = %+v", rep)
	}

	ats, _ := store.AssetTags(ctx, asset)
	var found bool
	for _, at := range ats {
		if at.Tag.Canonical() == "folder:rocks" {
			found = true
			if at.Source != tags.SourceManual {
				t.Errorf("manual folder:rocks demoted to %q by auto pass", at.Source)
			}
		}
	}
	if !found {
		t.Error("folder:rocks missing after retag")
	}
}

// TestRetagPrunesStaleAutoTags is the reconcile half M3 deferred: an asset that was
// reclassified or moved must lose the automatic tags it no longer earns, while its
// manual tags survive untouched (§7).
func TestRetagPrunesStaleAutoTags(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	asset := insertAsset(t, database, packID, "2 Objects/4 Stone/rock.png", "image", false, false, 1)
	tagger := New(database, store, nil)

	if _, err := tagger.Retag(ctx); err != nil {
		t.Fatalf("first retag: %v", err)
	}
	ats, _ := store.AssetTags(ctx, asset)
	if got := canonicals(ats); !reflect.DeepEqual(got,
		[]string{"folder:objects", "folder:stone", "type:image"}) {
		t.Fatalf("first pass tags = %v", got)
	}

	// A manual tag the machine pass must never remove, and an automatic tag from an
	// earlier classification that no longer applies.
	if _, err := store.TagAsset(ctx, asset, "pipeline:needs-rework", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TagAsset(ctx, asset, "type:audio", tags.SourceAutoType, nil); err != nil {
		t.Fatal(err)
	}
	// And the file moved, so its folder tags are wrong too.
	if _, err := database.Writer.Exec(
		`UPDATE assets SET rel_path = 'Props/rock.png' WHERE id = ?`, asset); err != nil {
		t.Fatal(err)
	}

	rep, err := tagger.Retag(ctx)
	if err != nil {
		t.Fatalf("second retag: %v", err)
	}
	if rep.Pruned != 3 {
		t.Errorf("pruned = %d, want 3 (two stale folder tags and the wrong type)", rep.Pruned)
	}

	ats, _ = store.AssetTags(ctx, asset)
	got := canonicals(ats)
	want := []string{"folder:props", "pipeline:needs-rework", "type:image"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tags after reconcile = %v, want %v", got, want)
	}
}

// TestRetagKeepsAManualTagThatLooksAutomatic guards the source check: a person who
// manually applied style:pixel-art keeps it even though the automatic pass would not.
func TestRetagKeepsAManualTagThatLooksAutomatic(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	asset := insertAsset(t, database, packID, "audio/hit.wav", "audio", false, false, 1)
	tagger := New(database, store, nil)

	if _, err := store.TagAsset(ctx, asset, "style:pixel-art", tags.SourceManual, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tagger.Retag(ctx); err != nil {
		t.Fatalf("retag: %v", err)
	}

	ats, _ := store.AssetTags(ctx, asset)
	if got := canonicals(ats); !reflect.DeepEqual(got,
		[]string{"folder:audio", "style:pixel-art", "type:audio"}) {
		t.Errorf("tags = %v; the manual style tag must survive", got)
	}
}

// TestRetagContentScopesToOneHash covers the hook derive calls after an analysis:
// only the assets holding that content are touched.
func TestRetagContentScopesToOneHash(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	// insertAsset derives each hash from the path, so these differ.
	analysed := insertAsset(t, database, packID, "art/hero.png", "image", true, true, 1)
	other := insertAsset(t, database, packID, "art/photo.png", "image", false, false, 1)
	tagger := New(database, store, nil)

	rep, err := tagger.RetagContent(ctx, "art/hero.png-hash")
	if err != nil {
		t.Fatalf("retag content: %v", err)
	}
	if rep.Assets != 1 {
		t.Errorf("examined %d asset(s), want only the one with that content", rep.Assets)
	}

	ats, _ := store.AssetTags(ctx, analysed)
	if got := canonicals(ats); !reflect.DeepEqual(got,
		[]string{"folder:art", "has:alpha", "style:pixel-art", "type:image"}) {
		t.Errorf("analysed asset tags = %v", got)
	}
	ats, _ = store.AssetTags(ctx, other)
	if got := canonicals(ats); len(got) != 0 {
		t.Errorf("the other asset must be untouched, got %v", got)
	}
}

// TestRetagAssetsScopesToIDs is the same for the id-based entry point.
func TestRetagAssetsScopesToIDs(t *testing.T) {
	database, store, packID := newFixture(t)
	ctx := context.Background()
	first := insertAsset(t, database, packID, "a/one.png", "image", false, false, 1)
	second := insertAsset(t, database, packID, "b/two.png", "image", false, false, 1)
	tagger := New(database, store, nil)

	if _, err := tagger.RetagAssets(ctx, []int64{first}); err != nil {
		t.Fatalf("retag assets: %v", err)
	}
	if ats, _ := store.AssetTags(ctx, first); len(canonicals(ats)) == 0 {
		t.Error("the named asset should have been tagged")
	}
	if ats, _ := store.AssetTags(ctx, second); len(canonicals(ats)) != 0 {
		t.Error("an asset not named must be untouched")
	}

	// An empty list is a no-op rather than a full sweep, which would be a nasty
	// surprise for a caller with nothing to do.
	rep, err := tagger.RetagAssets(ctx, nil)
	if err != nil || rep.Assets != 0 {
		t.Errorf("RetagAssets(nil) = %+v, %v; want a no-op", rep, err)
	}
}

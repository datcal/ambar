package tags

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// newStore opens a migrated temp database and returns a store with a fixed
// clock, so timestamps are deterministic.
func newStore(t *testing.T) (*Store, *db.DB) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "tags.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	fixed := time.Unix(1_700_000_000, 0)
	return NewStore(database).WithClock(func() time.Time { return fixed }), database
}

// insertAsset creates a pack with one asset and its (empty) FTS row, mirroring
// what the scanner writes, and returns their ids.
func insertAsset(t *testing.T, database *db.DB, packName, filename string) (packID, assetID int64) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().Unix()

	res, err := database.Writer.ExecContext(ctx, `
		INSERT INTO packs (name, slug, kind, library_rel_path, first_seen_at, last_seen_at, created_at, updated_at)
		VALUES (?, ?, 'folder', ?, ?, ?, ?, ?)`,
		packName, packName, packName, now, now, now, now)
	if err != nil {
		t.Fatalf("insert pack: %v", err)
	}
	packID, _ = res.LastInsertId()

	res, err = database.Writer.ExecContext(ctx, `
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, 'png', 'image', 1, ?, ?, ?, ?, ?, ?)`,
		packID, filename, filename, now, filename+"-hash", now, now, now, now)
	if err != nil {
		t.Fatalf("insert asset: %v", err)
	}
	assetID, _ = res.LastInsertId()

	if _, err := database.Writer.ExecContext(ctx, `
		INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes)
		VALUES (?, ?, ?, '', '')`, assetID, filename, packName); err != nil {
		t.Fatalf("insert fts: %v", err)
	}
	return packID, assetID
}

func TestParse(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		wantNS   string
		wantName string
		wantErr  bool
	}{
		{"flat", "license:cc0", "license", "cc0", false},
		{"hierarchy", "type:sfx:impact", "type", "sfx:impact", false},
		{"lowercased", "Author:Kenney", "author", "kenney", false},
		{"trimmed", "  theme:sci-fi  ", "theme", "sci-fi", false},
		{"no namespace", "cc0", "", "", true},
		{"empty", "", "", "", true},
		{"empty namespace", ":cc0", "", "", true},
		{"empty leaf", "type:", "", "", true},
		{"whitespace in segment", "type:sci fi", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ns, name, err := Parse(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("Parse(%q) = (%q,%q), want error", tc.in, ns, name)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse(%q) errored: %v", tc.in, err)
			}
			if ns != tc.wantNS || name != tc.wantName {
				t.Errorf("Parse(%q) = (%q,%q), want (%q,%q)", tc.in, ns, name, tc.wantNS, tc.wantName)
			}
		})
	}
}

func TestEnsureCreatesHierarchyAndClosure(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	leaf, err := s.Ensure(ctx, "type:sfx:impact")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if leaf.Canonical() != "type:sfx:impact" {
		t.Errorf("canonical = %q", leaf.Canonical())
	}
	if leaf.Leaf() != "impact" {
		t.Errorf("leaf = %q, want impact", leaf.Leaf())
	}

	// The parent must have been created and linked.
	parent, ok, err := s.GetByCanonical(ctx, "type:sfx")
	if err != nil || !ok {
		t.Fatalf("parent not created: ok=%v err=%v", ok, err)
	}
	if leaf.ParentID == nil || *leaf.ParentID != parent.ID {
		t.Errorf("leaf parent = %v, want %d", leaf.ParentID, parent.ID)
	}
	if parent.ParentID != nil {
		t.Errorf("top-level tag has a parent: %v", parent.ParentID)
	}

	// Idempotent: a second Ensure returns the same id and creates nothing.
	again, err := s.Ensure(ctx, "type:sfx:impact")
	if err != nil || again.ID != leaf.ID {
		t.Fatalf("second ensure = %d,%v, want %d", again.ID, err, leaf.ID)
	}

	// Closure: descendants of the parent are {parent, leaf}.
	desc, err := s.DescendantIDs(ctx, parent.ID)
	if err != nil {
		t.Fatalf("descendants: %v", err)
	}
	if len(desc) != 2 || desc[0] != parent.ID {
		t.Errorf("descendants of parent = %v, want [%d %d]", desc, parent.ID, leaf.ID)
	}
	// The leaf has only itself beneath it.
	leafDesc, _ := s.DescendantIDs(ctx, leaf.ID)
	if len(leafDesc) != 1 || leafDesc[0] != leaf.ID {
		t.Errorf("descendants of leaf = %v, want [%d]", leafDesc, leaf.ID)
	}
}

func TestResolveCanonicalAndAlias(t *testing.T) {
	s, _ := newStore(t)
	ctx := context.Background()

	tag, err := s.SetAlias(ctx, "cc0", "license:cc0")
	if err != nil {
		t.Fatalf("set alias: %v", err)
	}

	// By canonical.
	got, ok, err := s.Resolve(ctx, "license:cc0")
	if err != nil || !ok || got.ID != tag.ID {
		t.Fatalf("resolve canonical = %d,%v,%v", got.ID, ok, err)
	}
	// By alias.
	got, ok, err = s.Resolve(ctx, "cc0")
	if err != nil || !ok || got.ID != tag.ID {
		t.Fatalf("resolve alias = %d,%v,%v", got.ID, ok, err)
	}
	// Unknown bare word is simply not found.
	_, ok, err = s.Resolve(ctx, "nonsense")
	if err != nil || ok {
		t.Fatalf("resolve unknown = ok:%v err:%v", ok, err)
	}
	// Re-pointing an alias moves it.
	other, err := s.SetAlias(ctx, "cc0", "license:cc-by")
	if err != nil {
		t.Fatalf("re-point alias: %v", err)
	}
	got, ok, _ = s.Resolve(ctx, "cc0")
	if !ok || got.ID != other.ID {
		t.Errorf("alias did not move: got %d, want %d", got.ID, other.ID)
	}
}

func TestTagAssetSourcePrecedence(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	_, assetID := insertAsset(t, database, "pack", "sprite.png")

	// auto then manual: source upgrades to manual.
	if _, err := s.TagAsset(ctx, assetID, "type:sprite", SourceAutoPath, nil); err != nil {
		t.Fatalf("auto tag: %v", err)
	}
	if _, err := s.TagAsset(ctx, assetID, "type:sprite", SourceManual, nil); err != nil {
		t.Fatalf("manual tag: %v", err)
	}
	if src := assetTagSource(t, database, assetID, "type:sprite"); src != SourceManual {
		t.Errorf("after manual, source = %q, want manual", src)
	}

	// manual then auto: manual stays.
	if _, err := s.TagAsset(ctx, assetID, "type:sprite", SourceAutoPath, nil); err != nil {
		t.Fatalf("re-auto tag: %v", err)
	}
	if src := assetTagSource(t, database, assetID, "type:sprite"); src != SourceManual {
		t.Errorf("auto demoted manual: source = %q", src)
	}
}

func TestAssetTagsIncludeInheritedPackTags(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	packID, assetID := insertAsset(t, database, "kenney-pack", "sprite.png")

	if _, err := s.TagPack(ctx, packID, "author:kenney", SourceManual, nil); err != nil {
		t.Fatalf("tag pack: %v", err)
	}
	if _, err := s.TagAsset(ctx, assetID, "type:sprite", SourceManual, nil); err != nil {
		t.Fatalf("tag asset: %v", err)
	}

	got, err := s.AssetTags(ctx, assetID)
	if err != nil {
		t.Fatalf("asset tags: %v", err)
	}
	byCanon := map[string]AssetTag{}
	for _, at := range got {
		byCanon[at.Tag.Canonical()] = at
	}
	if at, ok := byCanon["author:kenney"]; !ok || !at.Inherited {
		t.Errorf("author:kenney inherited=%v present=%v", at.Inherited, ok)
	}
	if at, ok := byCanon["type:sprite"]; !ok || at.Inherited {
		t.Errorf("type:sprite inherited=%v present=%v", at.Inherited, ok)
	}
}

func TestFTSTagTextReflectsTags(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	_, assetID := insertAsset(t, database, "pack", "sprite.png")
	s.SetAlias(ctx, "sfx", "type:sfx")

	if _, err := s.TagAsset(ctx, assetID, "type:sfx:impact", SourceManual, nil); err != nil {
		t.Fatalf("tag: %v", err)
	}

	// Searchable by leaf, by ancestor segment, and by the ancestor's alias.
	for _, term := range []string{"impact", "sfx", "type"} {
		if !ftsMatches(t, database, assetID, term) {
			t.Errorf("asset not found by tag_text term %q", term)
		}
	}

	// Untagging clears it.
	leaf, _, _ := s.GetByCanonical(ctx, "type:sfx:impact")
	if err := s.UntagAsset(ctx, assetID, leaf.ID); err != nil {
		t.Fatalf("untag: %v", err)
	}
	if ftsMatches(t, database, assetID, "impact") {
		t.Error("asset still matches 'impact' after untag")
	}
}

func TestTagPackReindexesMembers(t *testing.T) {
	s, database := newStore(t)
	ctx := context.Background()
	// A pack name that does not itself contain the term, so a match can only come
	// from the inherited tag_text and not from the pack_name FTS column.
	packID, assetID := insertAsset(t, database, "loose-sprites", "sprite.png")

	if ftsMatches(t, database, assetID, "kenney") {
		t.Fatal("member matched 'kenney' before pack was tagged")
	}
	if _, err := s.TagPack(ctx, packID, "author:kenney", SourceManual, nil); err != nil {
		t.Fatalf("tag pack: %v", err)
	}
	if !ftsMatches(t, database, assetID, "kenney") {
		t.Error("member not found by inherited pack tag after tagging")
	}
}

// assetTagSource reads the stored source of one tag on an asset.
func assetTagSource(t *testing.T, database *db.DB, assetID int64, canonical string) string {
	t.Helper()
	ns, name, err := Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	var src string
	err = database.Reader.QueryRow(`
		SELECT at.source FROM asset_tags at JOIN tags t ON t.id = at.tag_id
		WHERE at.asset_id = ? AND t.namespace = ? AND t.name = ?`, assetID, ns, name).Scan(&src)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	return src
}

// ftsMatches reports whether the asset's FTS row matches a single term.
func ftsMatches(t *testing.T, database *db.DB, assetID int64, term string) bool {
	t.Helper()
	var got int64
	err := database.Reader.QueryRow(
		`SELECT rowid FROM assets_fts WHERE rowid = ? AND assets_fts MATCH ?`, assetID, term).Scan(&got)
	if err != nil {
		return false
	}
	return got == assetID
}

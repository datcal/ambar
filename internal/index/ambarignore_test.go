package index

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/datcal/ambar/internal/library"
)

// TestAmbarIgnoreDropsRowsAndBringsThemBack: hiding a folder that is already indexed
// drops its rows instead of parking them in the missing banner, touches nothing on disk,
// and deleting the rule brings the files straight back.
func TestAmbarIgnoreDropsRowsAndBringsThemBack(t *testing.T) {
	f := newFixture(t)
	f.write("3d/Synty/POLYGON - Boss Zombies/Source/FBX/SM_Prop_Dumbbell_01.fbx", "fbx")
	f.write("3d/Synty/POLYGON - Boss Zombies/Source/Textures/Texture_01_A.png", "png")
	f.write("3d/Synty/POLYGON - Boss Zombies/Unreal/Content/SM_Prop_Dumbbell_01.uasset", "uasset")
	f.write("3d/Synty/POLYGON - Boss Zombies/Unreal/Content/T_Texture_01_A.uasset", "uasset 2")

	if r := f.scan(); r.Added != 4 {
		t.Fatalf("first scan added %d, want 4", r.Added)
	}

	f.write(library.IgnoreFileName, "Unreal/\n")
	r := f.scan()
	if r.PurgedIgnored != 2 {
		t.Errorf("PurgedIgnored = %d, want 2 (the two Unreal files)", r.PurgedIgnored)
	}
	if r.MarkedMissing != 0 {
		t.Errorf("MarkedMissing = %d; a hidden file is not a missing one", r.MarkedMissing)
	}
	if r.Hidden != 1 {
		t.Errorf("Hidden = %d, want 1 (the Unreal folder, once)", r.Hidden)
	}
	if got := f.assetCount(); got != 2 {
		t.Errorf("%d rows remain, want 2", got)
	}
	// Invariant 1: hiding is a view of the library, never a change to it.
	for _, rel := range []string{
		"3d/Synty/POLYGON - Boss Zombies/Unreal/Content/SM_Prop_Dumbbell_01.uasset",
		"3d/Synty/POLYGON - Boss Zombies/Unreal/Content/T_Texture_01_A.uasset",
	} {
		if _, err := os.Stat(filepath.Join(f.root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s is gone from disk: %v", rel, err)
		}
	}

	// A second scan with the rule still in place is quiet.
	if r := f.scan(); r.PurgedIgnored != 0 || r.Added != 0 {
		t.Errorf("second scan: purged %d, added %d; want nothing", r.PurgedIgnored, r.Added)
	}

	f.remove(library.IgnoreFileName)
	if r := f.scan(); r.Added != 2 {
		t.Errorf("after removing the rule, added %d, want the 2 Unreal files back", r.Added)
	}
}

// TestUnityPackageRowsFromAnOlderScanAreDropped: a .unitypackage indexed before it stopped
// counting as an asset has to leave the grid on the next scan — not linger as "missing",
// which is where it would land if the reconciler only knew about junk.
func TestUnityPackageRowsFromAnOlderScanAreDropped(t *testing.T) {
	f := newFixture(t)
	f.write("3d/synty/hero.fbx", "fbx")
	f.write("3d/synty/Unity/pack.dat", "a unity package")
	f.scan()

	// What an older version left behind: the same row, under the name it really has.
	f.move("3d/synty/Unity/pack.dat", "3d/synty/Unity/POLYGON_Pack.unitypackage")
	if _, err := f.db.Writer.Exec(`
		UPDATE assets SET rel_path = 'Unity/POLYGON_Pack.unitypackage',
		                  filename = 'POLYGON_Pack.unitypackage', ext = 'unitypackage'
		WHERE rel_path = 'Unity/pack.dat'`); err != nil {
		t.Fatal(err)
	}

	r := f.scan()
	if r.PurgedIgnored != 1 || r.MarkedMissing != 0 {
		t.Errorf("purged %d, missing %d; want the package dropped, not marked missing",
			r.PurgedIgnored, r.MarkedMissing)
	}
	if _, ok := f.assetByPath("3d/synty/Unity/POLYGON_Pack.unitypackage"); ok {
		t.Error("the .unitypackage row is still in the index")
	}
	if _, err := os.Stat(filepath.Join(f.root, "3d/synty/Unity/POLYGON_Pack.unitypackage")); err != nil {
		t.Errorf("the package itself must stay on disk: %v", err)
	}
}

// TestHiddenAssetUsedByAProjectIsKept: dropping a row cascades its project_uses, which
// would quietly take the asset out of a project's CREDITS.md. A view preference must not
// be able to do that, so a row a project has used stays — removed uses included, since
// project_uses keeps those for history.
func TestHiddenAssetUsedByAProjectIsKept(t *testing.T) {
	f := newFixture(t)
	f.write("3d/synty/Source/hero.fbx", "fbx")
	f.write("3d/synty/Unity/used.png", "imported into a game")
	f.write("3d/synty/Unity/unused.png", "never imported")
	f.scan()

	used, ok := f.assetByPath("3d/synty/Unity/used.png")
	if !ok {
		t.Fatal("fixture: used.png was not indexed")
	}
	if _, err := f.db.Writer.Exec(`
		INSERT INTO projects (id, uuid, name, created_at, updated_at)
		VALUES (1, '0b6a3c1e-1111-4222-8333-944455556666', 'game', 0, 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`
		INSERT INTO project_uses (project_id, asset_id, res_path, added_at, removed_at)
		VALUES (1, ?, 'res://assets/image/synty/used.png', 0, 5)`, used.ID); err != nil {
		t.Fatal(err)
	}

	f.write(library.IgnoreFileName, "Unity/\n")
	r := f.scan()
	if r.PurgedIgnored != 1 || r.KeptInUse != 1 || r.MarkedMissing != 0 {
		t.Errorf("purged %d, kept %d, missing %d; want 1, 1, 0", r.PurgedIgnored, r.KeptInUse, r.MarkedMissing)
	}
	a, ok := f.assetByPath("3d/synty/Unity/used.png")
	if !ok {
		t.Fatal("the used asset's row was dropped")
	}
	if a.Missing() {
		t.Error("the used asset was marked missing; it is still on disk")
	}
	var uses int
	if err := f.db.Reader.QueryRow(`SELECT count(*) FROM project_uses WHERE asset_id = ?`, used.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if uses != 1 {
		t.Errorf("project_uses rows = %d, want the 1 that was there", uses)
	}
}

package library

import "testing"

// TestNASJunkIsIgnored: the names a shared network volume accumulates.
//
// `.Trash-1000` is the one that prompted this — it was sitting at the root of the target
// library with a deleted pack inside, and nothing in the list matched it, so a scan would
// have indexed deleted files and served them as search results. The uid in the name is the
// reason it has to be a glob: it is 1000 here and something else on the next machine.
func TestNASJunkIsIgnored(t *testing.T) {
	m := MustMatcher()

	ignored := []string{
		".Trash-1000", ".Trash-1026", ".Trash-0", ".Trash",
		"@eaDir", "#recycle", "#snapshot", ".@__thumb",
		"$RECYCLE.BIN", "System Volume Information",
		".Spotlight-V100", ".fseventsd", ".TemporaryItems", ".DocumentRevisions-V100",
		// Case-insensitive, because these arrive from Windows and macOS volumes.
		".trash-1000", "@EADIR", "$Recycle.Bin",
	}
	for _, name := range ignored {
		if !m.Ignored(name) {
			t.Errorf("%q should be ignored", name)
		}
	}

	// And nothing that looks like it but is somebody's artwork.
	kept := []string{
		"Trash", "trash-cans", ".Trashcan-sprites", "recycle-icons",
		"eaDir", "snapshot-tool", "System", "trash-1000-tileset.png",
	}
	for _, name := range kept {
		if m.Ignored(name) {
			t.Errorf("%q is content, not junk; it must not be ignored", name)
		}
	}

	// Whole subtrees, which is the point: the trash holds directories.
	if !m.IgnoredPath("2d/.Trash-1000/files/BurakLib/hero.png") {
		t.Error("a file inside the trash should be ignored")
	}
	if !m.IgnoredPath("3d/kenney/@eaDir/model.glb") {
		t.Error("a file inside @eaDir should be ignored")
	}
}

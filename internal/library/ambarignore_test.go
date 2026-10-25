package library

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestIgnoreRuleMatching: the .gitignore subset, one rule at a time. rel is relative to
// the folder holding the .ambarignore, as the walk hands it over.
func TestIgnoreRuleMatching(t *testing.T) {
	tests := []struct {
		pattern string
		rel     string
		isDir   bool
		want    bool
	}{
		// A bare name matches at any depth, files and folders alike.
		{"*.unitypackage", "POLYGON - Boss Zombies/Unity/Boss.unitypackage", false, true},
		{"*.unitypackage", "pack/hero.png", false, false},
		{"Unreal", "POLYGON - Boss Zombies/Unreal", true, true},
		{"Unreal", "pack/Unreal", false, true},
		// Case-insensitive, like the junk list.
		{"unity/", "POLYGON - Boss Zombies/Unity", true, true},
		{"*.UNITYPACKAGE", "x/Boss.unitypackage", false, true},
		// A trailing slash: directories only.
		{"Unity/", "pack/Unity", true, true},
		{"Unity/", "pack/Unity", false, false},
		// A slash elsewhere anchors to the ignore file's folder.
		{"Source/FBX/Chr/Unreal_Characters", "Source/FBX/Chr/Unreal_Characters", true, true},
		{"Source/FBX/Chr/Unreal_Characters", "Other/Source/FBX/Chr/Unreal_Characters", true, false},
		{"/Unity", "Unity", true, true},
		{"/Unity", "pack/Unity", true, false},
		{"*/Unity", "POLYGON - Boss Zombies/Unity", true, true},
		{"*/Unity", "a/b/Unity", true, false},
		// ** stands for any number of folders, none included.
		{"**/Demo_Models", "Demo_Models", true, true},
		{"**/Demo_Models", "ANIMATION - Idles/Source/Demo_Models", true, true},
		{"Source/**/*.psd", "Source/Textures/a.psd", false, true},
		{"Source/**/*.psd", "Source/a.psd", false, true},
		{"Source/**/*.psd", "Other/a.psd", false, false},
		// Spaces inside a name are part of it; Synty names every pack that way.
		{"POLYGON - Boss Zombies/", "POLYGON - Boss Zombies", true, true},
	}
	for _, tc := range tests {
		rule, ok, err := parseIgnoreLine(tc.pattern)
		if err != nil || !ok {
			t.Fatalf("parseIgnoreLine(%q) = ok %v, err %v", tc.pattern, ok, err)
		}
		if got := rule.matches(tc.rel, tc.isDir); got != tc.want {
			t.Errorf("%q against %q (dir %v) = %v, want %v", tc.pattern, tc.rel, tc.isDir, got, tc.want)
		}
	}
}

// TestIgnoreLinesThatAreNotRules: comments and blanks are skipped quietly; a line that
// cannot be honoured is an error, so the scan summary can say so.
func TestIgnoreLinesThatAreNotRules(t *testing.T) {
	for _, line := range []string{"", "   ", "# Unity packages", "\t# indented comment", "\r"} {
		if _, ok, err := parseIgnoreLine(line); ok || err != nil {
			t.Errorf("parseIgnoreLine(%q) = ok %v, err %v; want a skipped line", line, ok, err)
		}
	}
	for _, line := range []string{"!keep.png", "/", "a//b", "../outside", "Source/./x", "[unclosed"} {
		if _, ok, err := parseIgnoreLine(line); ok || err == nil {
			t.Errorf("parseIgnoreLine(%q) = ok %v, err %v; want an error", line, ok, err)
		}
	}
	// A Windows-edited file ends every line in \r, which must not become part of a name.
	rule, ok, err := parseIgnoreLine("Unreal/\r")
	if err != nil || !ok || !rule.matches("pack/Unreal", true) {
		t.Errorf("a CRLF line did not match: ok %v, err %v", ok, err)
	}
}

// writeTree writes files with the given contents under a fresh root.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func indexedPaths(res *WalkResult) []string {
	var out []string
	for _, f := range res.Files {
		if f.PackRelPath == "." {
			out = append(out, f.RelPath)
			continue
		}
		out = append(out, f.PackRelPath+"/"+f.RelPath)
	}
	sort.Strings(out)
	return out
}

// TestWalkHonoursAmbarIgnore is the layout that prompted the feature: a Synty pack ships
// the same models three times — source FBX and textures, a Unity package, an Unreal zip
// — plus a second copy of the characters for Unreal. One rule file at the library root
// hides the engine folders everywhere; one inside a pack hides what only that pack has.
func TestWalkHonoursAmbarIgnore(t *testing.T) {
	root := writeTree(t, map[string]string{
		IgnoreFileName: "# engine copies, never previewable\nUnity/\nUnreal/\n",

		"3d/Synty/POLYGON - Boss Zombies/POLYGON_BossZombies_ICON.png":                     "icon",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/SM_Prop_Dumbbell_01.fbx":               "fbx",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/Chr/SK_Chr_ZombieBoss_Brute_01.fbx":    "fbx",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/Chr/Unreal_Characters/SK_Brute.fbx":    "fbx",
		"3d/Synty/POLYGON - Boss Zombies/Source/Textures/PolygonZombieBoss_Texture_01.png": "png",
		"3d/Synty/POLYGON - Boss Zombies/Unity/POLYGON_BossZombies.unitypackage":           "pkg",
		"3d/Synty/POLYGON - Boss Zombies/Unreal/POLYGON_BossZombies_Unreal.uasset":         "uasset",
		"3d/Synty/POLYGON - Boss Zombies/" + IgnoreFileName:                                "Source/FBX/Chr/Unreal_Characters\n",

		// The same folder name elsewhere, at a different depth, is hidden by the root's
		// unanchored rule too.
		"2d/pack/Unity/preview.png": "png",
		"2d/pack/hero.png":          "png",
	})

	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range res.Errors {
		t.Errorf("walk error: %v", e)
	}

	want := []string{
		"2d/pack/hero.png",
		"3d/Synty/POLYGON - Boss Zombies/POLYGON_BossZombies_ICON.png",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/Chr/SK_Chr_ZombieBoss_Brute_01.fbx",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/SM_Prop_Dumbbell_01.fbx",
		"3d/Synty/POLYGON - Boss Zombies/Source/Textures/PolygonZombieBoss_Texture_01.png",
	}
	if got := indexedPaths(res); !reflect.DeepEqual(got, want) {
		t.Errorf("indexed\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
	// Three folders (two Unity, one Unreal) and the pack's own Unreal_Characters, each
	// counted once however much is inside.
	if res.Hidden != 4 {
		t.Errorf("Hidden = %d, want 4", res.Hidden)
	}

	// Excluded answers for paths the walk never reached, which is what the scan asks.
	for _, p := range []string{
		"3d/Synty/POLYGON - Boss Zombies/Unity/POLYGON_BossZombies.unitypackage",
		"3d/Synty/POLYGON - Boss Zombies/Source/FBX/Chr/Unreal_Characters/SK_Brute.fbx",
		"2d/pack/Unity/preview.png",
		"2d/pack/source.zip",        // not an asset
		"2d/pack/old.unitypackage",  // not an asset, even outside a hidden folder
		"2d/pack/__MACOSX/hero.png", // junk
	} {
		if !res.Excluded(p) {
			t.Errorf("Excluded(%q) = false, want true", p)
		}
	}
	for _, p := range []string{"2d/pack/hero.png", "2d/pack/gone.png", "3d/Synty/POLYGON - Boss Zombies/Source/FBX/x.fbx"} {
		if res.Excluded(p) {
			t.Errorf("Excluded(%q) = true; an ordinary asset path must not be", p)
		}
	}
}

// TestHiddenFilesDoNotMakeAPack: pack detection must not count what is hidden.
//
// isPack only decides below pack level, which happens when a sidecar deeper down stops
// the top folder being a pack. Then `desert` is a pack only if it holds an asset
// directly — and its only file is hidden, so it must come out exactly as if that file
// were not there: not a pack, leaving `desert/cacti` to be one.
func TestHiddenFilesDoNotMakeAPack(t *testing.T) {
	tree := map[string]string{
		"collection/forest/" + SidecarName: "{}",
		"collection/forest/tree.png":       "png",
		"collection/desert/promo.mp4":      "video",
		"collection/desert/cacti/a.png":    "png",
	}
	res, err := Walk(WalkOptions{Root: writeTree(t, tree)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packPaths(res), []string{"collection/desert", "collection/forest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("without an ignore file, packs = %v, want %v (the fixture is wrong)", got, want)
	}

	tree["collection/desert/"+IgnoreFileName] = "promo.mp4\n"
	res, err = Walk(WalkOptions{Root: writeTree(t, tree)})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := packPaths(res), []string{"collection/desert/cacti", "collection/forest"}; !reflect.DeepEqual(got, want) {
		t.Errorf("with promo.mp4 hidden, packs = %v, want %v", got, want)
	}
}

// TestASidecarInAHiddenFolderDoesNotPlaceAPack: §5.1 makes a sidecar beat every
// heuristic, but a hidden folder is not part of the library at all, so its sidecar must
// not pull the pack boundary down into it.
func TestASidecarInAHiddenFolderDoesNotPlaceAPack(t *testing.T) {
	root := writeTree(t, map[string]string{
		IgnoreFileName:                          "Unity/\n",
		"3d/pack/hero.fbx":                      "fbx",
		"3d/pack/Unity/inner/" + SidecarName:    "{}",
		"3d/pack/Unity/inner/hero.unitypackage": "pkg",
	})
	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if got := packPaths(res); !reflect.DeepEqual(got, []string{"3d/pack"}) {
		t.Errorf("packs = %v, want [3d/pack]", got)
	}
}

// TestBadIgnoreLinesAreReportedNotFatal: a typo in one folder's rules is a walk error
// naming the file and line, the rest of the file still applies, and the library is
// still indexed.
func TestBadIgnoreLinesAreReportedNotFatal(t *testing.T) {
	root := writeTree(t, map[string]string{
		"2d/pack/" + IgnoreFileName: "[broken\n!keep.png\n*.psd\n",
		"2d/pack/hero.png":          "png",
		"2d/pack/hero.psd":          "psd",
	})
	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Errors) != 2 {
		t.Fatalf("errors = %v, want two (one per bad line)", res.Errors)
	}
	for i, line := range []string{"line 1", "line 2"} {
		if msg := res.Errors[i].Error(); !strings.Contains(msg, "2d/pack/"+IgnoreFileName) || !strings.Contains(msg, line) {
			t.Errorf("error %q does not name the file and %s", msg, line)
		}
	}
	if got := indexedPaths(res); !reflect.DeepEqual(got, []string{"2d/pack/hero.png"}) {
		t.Errorf("indexed %v, want only hero.png — the good line must still apply", got)
	}
}

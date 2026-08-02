package library

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// --- ignore rules (§5.1) ---------------------------------------------------

func TestIgnored(t *testing.T) {
	m := MustMatcher()

	junk := []string{
		"__MACOSX", "__macosx", // case-insensitive: these arrive from macOS
		".DS_Store", ".ds_store",
		"Thumbs.db", "thumbs.db", "THUMBS.DB",
		"desktop.ini",
		"._sprite.png", "._", // AppleDouble
		".git",
	}
	for _, name := range junk {
		if !m.Ignored(name) {
			t.Errorf("Ignored(%q) = false, want true", name)
		}
	}

	keep := []string{
		"sprite.png", "PNG", "Tiled_files", "readme.txt", ".ambar.json",
		"_inbox",          // reserved, but that is IsReserved's job, not the junk list
		"macosx.png",      // not the shadow directory
		"my_thumbs.db",    // not Thumbs.db
		"desktop.ini.bak", // not desktop.ini
		"gitignore",
	}
	for _, name := range keep {
		if m.Ignored(name) {
			t.Errorf("Ignored(%q) = true, want false", name)
		}
	}
}

func TestIgnoredPath(t *testing.T) {
	m := MustMatcher()

	// __MACOSX at any depth must exclude everything beneath it — §5.1 says it
	// "duplicates the entire tree of a pack and will double the apparent asset
	// count if not excluded".
	for _, p := range []string{
		"__MACOSX/sprite.png",
		"pack/__MACOSX/sprite.png",
		"a/b/c/__MACOSX/d/e/sprite.png",
		"pack/.git/config",
		"pack/PNG/.DS_Store",
	} {
		if !m.IgnoredPath(p) {
			t.Errorf("IgnoredPath(%q) = false, want true", p)
		}
	}
	for _, p := range []string{
		"pack/PNG/sprite.png",
		"raw/craftpix-991101-free/Tiled_files/map.tmx",
	} {
		if m.IgnoredPath(p) {
			t.Errorf("IgnoredPath(%q) = true, want false", p)
		}
	}
}

func TestNewMatcherRejectsBadGlobs(t *testing.T) {
	if _, err := NewMatcher([]string{"["}); err == nil {
		t.Error("an invalid glob was accepted; a typo in AMBAR_IGNORE_GLOBS must fail at startup")
	}
	// A trailing slash is how a human writes a directory rule.
	m, err := NewMatcher([]string{"__MACOSX/"})
	if err != nil {
		t.Fatal(err)
	}
	if !m.Ignored("__MACOSX") {
		t.Error(`the glob "__MACOSX/" did not match the directory __MACOSX`)
	}
}

func TestIsReserved(t *testing.T) {
	for _, name := range []string{"_inbox", "_archives", "_quarantine", "_trash", "_anything"} {
		if !IsReserved(name) {
			t.Errorf("IsReserved(%q) = false, want true (§17 reserves underscore prefixes)", name)
		}
	}
	for _, name := range []string{"inbox", "2d", "raw", "craftpix-net-695666-free"} {
		if IsReserved(name) {
			t.Errorf("IsReserved(%q) = true, want false", name)
		}
	}
}

// --- format folders (§5.1) -------------------------------------------------

func TestIsFormatFolder(t *testing.T) {
	// The exact list from §5.1, plus the prefix forms it calls out by name.
	yes := []string{
		"PNG", "png", "PSD", "ASEPRITE", "ASE", "SVG", "EPS", "AI", "KRA", "XCF",
		"Tiled_files", "tiled_files", "TMX",
		"FBX", "OBJ", "GLTF", "GLB", "BLEND",
		"Source", "Sources", "SourceFiles", "Vector",
		// The prefix form: a known token followed by _ or -.
		"PNG_Animations", "PNG_Parts&Spriter_Animation", "png-animations",
		"PSD_Layers", "OBJ-files",
		// The parenthesised form, which 3D packs use constantly. KayKit ships
		// Assets/fbx(unity)/ beside Assets/fbx/, Assets/gltf/ and Assets/obj/; missing
		// this left every one of those files in a group of its own.
		"fbx(unity)", "FBX(Unity)", "FBX (Unity)", "OBJ (legacy)", "PNG(transparent)",
	}
	for _, s := range yes {
		if !IsFormatFolder(s) {
			t.Errorf("IsFormatFolder(%q) = false, want true", s)
		}
	}

	// The over-matching cases. "Objects" starting with "obj" is the trap: treating
	// it as a format folder would make M2 collapse unrelated assets into one group.
	no := []string{
		"Objects", "objects", "Object",
		"Aim", "AImages", // start with "ai"
		"Pngs", "Pnguin",
		"Sourcecode", // no separator
		"Environment", "Rocks", "2 Objects", "4 Stone",
		"Characters", "Enemies", "Tiles",
		"", "  ",
		// A parenthesis does not make an ordinary folder a format folder. The first is
		// a real pack directory in this library — a duplicate download — and treating
		// it as a format folder would merge two whole packs into one.
		"craftpix-net-665895-free-pixel-dungeon-props-and-objects-asset-pack (1)",
		"Objects (large)", "Characters(new)", "(unsorted)",
		// Only a parenthesis, never a bare space: "ai generated" is not Illustrator.
		"ai generated", "png files",
	}
	for _, s := range no {
		if IsFormatFolder(s) {
			t.Errorf("IsFormatFolder(%q) = true, want false", s)
		}
	}
}

func TestStripFormatFolders(t *testing.T) {
	// This is the §5.1 asset-group key that M2 will use. Verified now so M2
	// inherits a tested building block.
	tests := []struct {
		in, want string
	}{
		{"PNG/Plant1/idle.png", "Plant1/idle.png"},
		{"PSD/Plant1/idle.psd", "Plant1/idle.psd"},
		{"ASEPRITE/Plant1/idle.aseprite", "Plant1/idle.aseprite"},
		{"PNG_Animations/Explosions/frame_01.png", "Explosions/frame_01.png"},
		{"Models/Environment/Rocks/rock.glb", "Models/Environment/Rocks/rock.glb"},
		{"idle.png", "idle.png"},
		// The final segment is never stripped, so a file named PNG survives.
		{"Plant1/PNG", "Plant1/PNG"},
		{"Source/Sources/deep.psd", "deep.psd"},
		// The 3D case: four format folders, one model.
		{"Assets/fbx/decoration/props/barrel.fbx", "Assets/decoration/props/barrel.fbx"},
		{"Assets/fbx(unity)/decoration/props/barrel.fbx", "Assets/decoration/props/barrel.fbx"},
		{"Assets/gltf/decoration/props/barrel.gltf", "Assets/decoration/props/barrel.gltf"},
		{"Assets/obj/decoration/props/barrel.obj", "Assets/decoration/props/barrel.obj"},
	}
	for _, tc := range tests {
		if got := StripFormatFolders(tc.in); got != tc.want {
			t.Errorf("StripFormatFolders(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	// The property M2 actually depends on: the three format variants of one
	// artwork must produce the same key.
	png := StripFormatFolders("PNG/Plant1/idle.png")
	psd := StripFormatFolders("PSD/Plant1/idle.psd")
	ase := StripFormatFolders("ASEPRITE/Plant1/idle.aseprite")
	base := func(s string) string { return strings.TrimSuffix(s, filepath.Ext(s)) }
	if base(png) != base(psd) || base(psd) != base(ase) {
		t.Errorf("format variants did not collapse to one key: %q %q %q", png, psd, ase)
	}

	// The same property for a 3D pack, which is where it was failing: one model
	// shipped four ways must be one asset, not two groups of two.
	fbx := StripFormatFolders("Assets/fbx/decoration/props/barrel.fbx")
	unity := StripFormatFolders("Assets/fbx(unity)/decoration/props/barrel.fbx")
	gltf := StripFormatFolders("Assets/gltf/decoration/props/barrel.gltf")
	obj := StripFormatFolders("Assets/obj/decoration/props/barrel.obj")
	if base(fbx) != base(unity) || base(unity) != base(gltf) || base(gltf) != base(obj) {
		t.Errorf("model variants did not collapse to one key: %q %q %q %q", fbx, unity, gltf, obj)
	}
}

// --- classification --------------------------------------------------------

func TestKindForExt(t *testing.T) {
	tests := map[string]Kind{
		"png": KindImage, "jpg": KindImage, "webp": KindImage,
		"psd": KindImage, "aseprite": KindImage, "kra": KindImage, "svg": KindImage,
		"hdr": KindHDRI, "exr": KindHDRI,
		"glb": KindModel, "gltf": KindModel, "obj": KindModel, "fbx": KindModel, "blend": KindModel,
		"wav": KindAudio, "ogg": KindAudio, "mp3": KindAudio, "flac": KindAudio,
		"mp4": KindVideo, "webm": KindVideo,
		"ttf": KindFont, "otf": KindFont,
		"gd": KindScript, "gdshader": KindScript,
		"tres": KindMaterial, "mtl": KindMaterial,
		// §5.1's additions.
		"tmx": KindTilemap, "tsx": KindTilemap,
		"scml": KindRig, "scon": KindRig,
		// Unknown and empty fall through to other.
		"xyz": KindOther, "": KindOther,
	}
	for ext, want := range tests {
		if got := KindForExt(ext); got != want {
			t.Errorf("KindForExt(%q) = %q, want %q", ext, got, want)
		}
	}
	// Case must not matter: vendors ship .PNG and .Wav.
	if KindForExt("PNG") != KindImage {
		t.Error("KindForExt is case-sensitive")
	}
}

func TestExt(t *testing.T) {
	tests := map[string]string{
		"sprite.png": "png", "SPRITE.PNG": "png",
		"idle.aseprite":  "aseprite",
		"archive.tar.gz": "gz",
		"noextension":    "",
		".gitignore":     "gitignore", // filepath.Ext's behaviour, documented rather than fought
		"trailing.":      "",
	}
	for in, want := range tests {
		if got := Ext(in); got != want {
			t.Errorf("Ext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestClassifyReadsDimensions covers the §5 "cheap metadata" step, and — more
// importantly — that §16's deliberately broken files degrade rather than panic.
func TestClassifyReadsDimensions(t *testing.T) {
	dir := t.TempDir()

	// A real 24x8 PNG.
	good := filepath.Join(dir, "sprite.png")
	if err := os.WriteFile(good, makePNG(t, 24, 8), 0o644); err != nil {
		t.Fatal(err)
	}
	info := Classify(good)
	if info.Kind != KindImage {
		t.Errorf("kind = %q, want image", info.Kind)
	}
	if info.Width != 24 || info.Height != 8 {
		t.Errorf("dimensions = %dx%d, want 24x8", info.Width, info.Height)
	}

	// Deliberately broken inputs (§16). None may panic, and all must still carry
	// the extension-derived kind — a corrupt file is still worth indexing.
	broken := map[string][]byte{
		"truncated.png":   makePNG(t, 16, 16)[:20],
		"empty.png":       {},
		"garbage.png":     []byte("this is not a png at all"),
		"header-only.png": []byte("\x89PNG\r\n\x1a\n"),
	}
	for name, content := range broken {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, content, 0o644); err != nil {
			t.Fatal(err)
		}
		info := Classify(p)
		if info.Kind != KindImage {
			t.Errorf("%s: kind = %q, want image (the extension still tells us)", name, info.Kind)
		}
		if info.Width != 0 || info.Height != 0 {
			t.Errorf("%s: dimensions = %dx%d, want 0x0 for an unreadable header",
				name, info.Width, info.Height)
		}
	}

	// A file whose extension promises nothing readable.
	model := filepath.Join(dir, "turret.glb")
	if err := os.WriteFile(model, []byte("glTF binary would go here"), 0o644); err != nil {
		t.Fatal(err)
	}
	if info := Classify(model); info.Kind != KindModel || info.Width != 0 {
		t.Errorf("glb classified as %q %dx%d", info.Kind, info.Width, info.Height)
	}

	// A missing file must not panic either.
	if info := Classify(filepath.Join(dir, "absent.png")); info.Kind != KindImage {
		t.Errorf("a missing file classified as %q", info.Kind)
	}
}

func TestIsAssetFile(t *testing.T) {
	for _, name := range []string{"sprite.png", "turret.glb", "impact.wav", "map.tmx", "unknown.xyz"} {
		if !IsAssetFile(name) {
			t.Errorf("IsAssetFile(%q) = false, want true", name)
		}
	}
	// Documentation and metadata do not make a directory a pack on their own.
	for _, name := range []string{
		"README", "readme.txt", "README.md", "LICENSE", "license.txt",
		".ambar.json", "notes.md", "source.url", "info.nfo",
	} {
		if IsAssetFile(name) {
			t.Errorf("IsAssetFile(%q) = true, want false", name)
		}
	}
}

func TestSlugify(t *testing.T) {
	tests := map[string]string{
		"craftpix-net-695666-free-undead-tileset-top-down-pixel-art": "craftpix-net-695666-free-undead-tileset-top-down-pixel-art",
		"Kenney Sci-Fi RTS Pack":                                     "kenney-sci-fi-rts-pack",
		"PNG_Parts&Spriter_Animation":                                "png-parts-spriter-animation",
		"2 Objects":                                                  "2-objects",
		"  spaced  out  ":                                            "spaced-out",
		"Café Ambiance":                                              "caf-ambiance",
		"---dashes---":                                               "dashes",
		"🎮":                                                          "pack", // nothing usable survives
		"":                                                           "pack",
	}
	for in, want := range tests {
		if got := Slugify(in); got != want {
			t.Errorf("Slugify(%q) = %q, want %q", in, got, want)
		}
	}
	// A slug must always be usable as a single res:// path segment (§10).
	for _, in := range []string{"a/b", "..", "with space", "Ünïcödé"} {
		got := Slugify(in)
		if strings.ContainsAny(got, "/\\ .") || got == "" {
			t.Errorf("Slugify(%q) = %q, which is not a safe path segment", in, got)
		}
	}
}

// --- pack detection and walking (§5.1) -------------------------------------

// buildLibrary writes a fixture tree. Paths ending in "/" are directories;
// everything else is a file with placeholder content.
func buildLibrary(t *testing.T, entries ...string) string {
	t.Helper()

	root := t.TempDir()
	for _, e := range entries {
		full := filepath.Join(root, filepath.FromSlash(e))
		if strings.HasSuffix(e, "/") {
			if err := os.MkdirAll(full, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("content of "+e), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func walkFixture(t *testing.T, root string) *WalkResult {
	t.Helper()
	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return res
}

func packPaths(res *WalkResult) []string {
	out := make([]string, 0, len(res.Packs))
	for _, p := range res.Packs {
		out = append(out, p.RelPath)
	}
	sort.Strings(out)
	return out
}

// TestPackDetectionIsDepthAgnostic is the §5.1 requirement stated most plainly:
// the library contains packs at depth 1 and depth 2, and assuming a fixed depth
// makes the grid unusable.
func TestPackDetectionIsDepthAgnostic(t *testing.T) {
	root := buildLibrary(t,
		// Depth 1, exactly as observed.
		"craftpix-net-695666-free-undead-tileset-top-down-pixel-art/tileset.png",
		// Depth 2 under an organisational bucket, also as observed.
		"raw/craftpix-991101-free-pixel-art-enemy-spaceship-2d-sprites/ship.png",
		// Depth 2 under a different bucket.
		"2d/some-itch-pack/sprite.png",
		"3d/kenney-sci-fi/models/turret.glb",
	)

	res := walkFixture(t, root)

	want := []string{
		"2d/some-itch-pack",
		"3d/kenney-sci-fi",
		"craftpix-net-695666-free-undead-tileset-top-down-pixel-art",
		"raw/craftpix-991101-free-pixel-art-enemy-spaceship-2d-sprites",
	}
	if got := packPaths(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("packs = %v,\nwant %v", got, want)
	}

	// The buckets themselves must not be packs (§5.1), and neither must the
	// pack's own subdirectory.
	for _, notPack := range []string{"raw", "2d", "3d", "3d/kenney-sci-fi/models"} {
		for _, p := range res.Packs {
			if p.RelPath == notPack {
				t.Errorf("%q was detected as a pack; organisational parents must not be", notPack)
			}
		}
	}
}

// TestPackDetectionFormatFolders covers §5.1's third rule and the exact tree it
// describes.
func TestPackDetectionFormatFolders(t *testing.T) {
	root := buildLibrary(t,
		"craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/PNG/Plant1/idle.png",
		"craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/PSD/Plant1/idle.psd",
		"craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/ASEPRITE/Plant1/idle.aseprite",
		"craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/Tiled_files/map.tmx",
	)

	res := walkFixture(t, root)

	want := []string{"craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack"}
	if got := packPaths(res); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("packs = %v, want %v", got, want)
	}
	// Crucially the format folders are NOT packs of their own.
	if len(res.Files) != 4 {
		t.Errorf("indexed %d files, want 4", len(res.Files))
	}
	for _, f := range res.Files {
		if f.PackRelPath != want[0] {
			t.Errorf("file %q belongs to pack %q, want %q", f.RelPath, f.PackRelPath, want[0])
		}
	}
	// rel_path is pack-relative and keeps the format folder, so M2 can compute
	// the group key from it.
	paths := map[string]bool{}
	for _, f := range res.Files {
		paths[f.RelPath] = true
	}
	for _, want := range []string{
		"PNG/Plant1/idle.png", "PSD/Plant1/idle.psd",
		"ASEPRITE/Plant1/idle.aseprite", "Tiled_files/map.tmx",
	} {
		if !paths[want] {
			t.Errorf("missing pack-relative path %q; got %v", want, paths)
		}
	}
}

func TestPackDetectionSidecarWins(t *testing.T) {
	// A directory holding only subdirectories would normally not be a pack, but
	// an explicit .ambar.json is the strongest possible signal.
	root := buildLibrary(t,
		"weird-pack/.ambar.json",
		"weird-pack/Characters/hero/idle.png",
	)
	res := walkFixture(t, root)

	if got := packPaths(res); strings.Join(got, ",") != "weird-pack" {
		t.Errorf("packs = %v, want [weird-pack]", got)
	}
	// The sidecar itself is metadata, not an asset.
	for _, f := range res.Files {
		if f.Filename == ".ambar.json" {
			t.Error(".ambar.json was indexed as an asset")
		}
	}
	if len(res.Files) != 1 {
		t.Errorf("indexed %d files, want 1", len(res.Files))
	}
}

// TestMacosxDoesNotDoubleTheAssetCount is the §5.1 warning made into a test.
func TestMacosxDoesNotDoubleTheAssetCount(t *testing.T) {
	root := buildLibrary(t,
		"pack/PNG/sprite_a.png",
		"pack/PNG/sprite_b.png",
		// The shadow tree, which mirrors the whole pack.
		"pack/__MACOSX/PNG/._sprite_a.png",
		"pack/__MACOSX/PNG/._sprite_b.png",
		"__MACOSX/pack/PNG/._sprite_a.png",
		// And the rest of the usual junk.
		"pack/.DS_Store",
		"pack/PNG/Thumbs.db",
		"pack/PNG/._sprite_a.png",
	)

	res := walkFixture(t, root)

	if len(res.Files) != 2 {
		names := make([]string, 0, len(res.Files))
		for _, f := range res.Files {
			names = append(names, f.RelPath)
		}
		t.Errorf("indexed %d files, want 2: %v", len(res.Files), names)
	}
	if res.IgnoredCount == 0 {
		t.Error("nothing was reported as ignored, so the summary would not warn about junk")
	}
	for _, p := range res.Packs {
		if strings.Contains(p.RelPath, "__MACOSX") {
			t.Errorf("__MACOSX was detected as a pack: %q", p.RelPath)
		}
	}
}

// TestReservedDirectoriesAreSkipped covers §17.
func TestReservedDirectoriesAreSkipped(t *testing.T) {
	root := buildLibrary(t,
		"pack/sprite.png",
		"_inbox/incoming.zip",
		"_trash/deleted/sprite.png",
		"_archives/original.zip",
		"_quarantine/broken.zip",
	)

	res := walkFixture(t, root)

	if len(res.Files) != 1 {
		t.Errorf("indexed %d files, want 1 — reserved directories must be skipped", len(res.Files))
	}
	for _, p := range res.Packs {
		if strings.HasPrefix(p.RelPath, "_") {
			t.Errorf("reserved directory %q was treated as a pack", p.RelPath)
		}
	}
	if len(res.ReservedSkipped) != 4 {
		t.Errorf("reported %d skipped reserved directories, want 4: %v",
			len(res.ReservedSkipped), res.ReservedSkipped)
	}

	// A deeper underscore directory is real content, not reserved: a vendor pack
	// may legitimately contain `_parts`.
	root2 := buildLibrary(t, "pack/_parts/sprite.png")
	if res2 := walkFixture(t, root2); len(res2.Files) != 1 {
		t.Errorf("a nested underscore directory was skipped; indexed %d files, want 1", len(res2.Files))
	}
}

// TestLooseFilesGetAStandalonePack: §5.1 forbids treating a bucket as a pack, and
// §4 provides `standalone` for files with nowhere else to go.
func TestLooseFilesGetAStandalonePack(t *testing.T) {
	root := buildLibrary(t,
		"stray_sprite.png",        // directly at the library root
		"realpack/PNG/sprite.png", // a proper pack alongside it
	)

	res := walkFixture(t, root)

	var standalone *Pack
	for i := range res.Packs {
		if res.Packs[i].Kind == "standalone" {
			standalone = &res.Packs[i]
		}
	}
	if standalone == nil {
		t.Fatalf("no standalone pack was created for the loose file; packs = %v", packPaths(res))
	}
	if standalone.RelPath != "." {
		t.Errorf("standalone pack path = %q, want \".\"", standalone.RelPath)
	}
	if len(res.Files) != 2 {
		t.Errorf("indexed %d files, want 2", len(res.Files))
	}
}

// TestBuckets covers the addition to §5.1's literal rules. Without it,
// 3d/kenney-sci-fi/Models/turret.glb attributes to "Models" — which is §3's own
// example pack shape, saved there only by having a sidecar.
func TestBuckets(t *testing.T) {
	root := buildLibrary(t,
		"3d/kenney-sci-fi/Models/turret.glb",
		"3d/kenney-sci-fi/Sprites/ui_atlas.png",
		"2d/craftpix-net-1234-pack/PNG/sprite.png",
		"mix/some-pack/thing.png",
		"raw/another-pack/thing.png",
		"audio/freesound-impacts/impact.wav",
	)

	res := walkFixture(t, root)

	wantBuckets := []string{"2d", "3d", "audio", "mix", "raw"}
	if strings.Join(res.Buckets, ",") != strings.Join(wantBuckets, ",") {
		t.Errorf("buckets = %v, want %v", res.Buckets, wantBuckets)
	}

	wantPacks := []string{
		"2d/craftpix-net-1234-pack",
		"3d/kenney-sci-fi",
		"audio/freesound-impacts",
		"mix/some-pack",
		"raw/another-pack",
	}
	if got := packPaths(res); strings.Join(got, ",") != strings.Join(wantPacks, ",") {
		t.Errorf("packs = %v,\nwant %v", got, wantPacks)
	}

	// Both subfolders of the Kenney pack belong to the pack, not to themselves.
	for _, f := range res.Files {
		if f.PackRelPath == "3d/kenney-sci-fi" {
			continue
		}
		if strings.HasPrefix(f.PackRelPath, "3d/kenney-sci-fi/") {
			t.Errorf("file %q was attributed to %q, want 3d/kenney-sci-fi",
				f.RelPath, f.PackRelPath)
		}
	}
	// And rel_path keeps the subfolder, so the structure is not lost.
	paths := map[string]bool{}
	for _, f := range res.Files {
		if f.PackRelPath == "3d/kenney-sci-fi" {
			paths[f.RelPath] = true
		}
	}
	for _, want := range []string{"Models/turret.glb", "Sprites/ui_atlas.png"} {
		if !paths[want] {
			t.Errorf("missing pack-relative path %q, got %v", want, paths)
		}
	}
}

// TestBucketNamesOnlyApplyAtTopLevel: a pack legitimately called "3d" nested
// inside another directory must still be a pack.
func TestBucketNamesOnlyApplyAtTopLevel(t *testing.T) {
	root := buildLibrary(t, "vendor-bundle/3d/model.glb")

	res := walkFixture(t, root)

	// "vendor-bundle" is at pack level and has assets beneath, so it is the pack;
	// the nested "3d" is an ordinary subfolder, not a skipped bucket.
	if got := packPaths(res); strings.Join(got, ",") != "vendor-bundle" {
		t.Errorf("packs = %v, want [vendor-bundle]", got)
	}
	if len(res.Files) != 1 {
		t.Fatalf("indexed %d files, want 1", len(res.Files))
	}
	if res.Files[0].RelPath != "3d/model.glb" {
		t.Errorf("rel_path = %q, want 3d/model.glb", res.Files[0].RelPath)
	}
	if len(res.Buckets) != 0 {
		t.Errorf("buckets = %v, want none", res.Buckets)
	}
}

// TestBucketsAreConfigurable keeps §17's "must not depend on this layout" honest:
// if the buckets are renamed or abandoned, the list changes and nothing else does.
func TestBucketsAreConfigurable(t *testing.T) {
	root := buildLibrary(t, "3d/kenney-sci-fi/Models/turret.glb")

	// With bucket recognition switched off, the top-level directory is the pack.
	res, err := Walk(WalkOptions{Root: root, Buckets: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if got := packPaths(res); strings.Join(got, ",") != "3d" {
		t.Errorf("with no buckets configured, packs = %v, want [3d]", got)
	}

	// With a custom name, that name is the bucket instead.
	root2 := buildLibrary(t, "assets-2d/somepack/sprite.png")
	res2, err := Walk(WalkOptions{Root: root2, Buckets: []string{"assets-2d"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := packPaths(res2); strings.Join(got, ",") != "assets-2d/somepack" {
		t.Errorf("packs = %v, want [assets-2d/somepack]", got)
	}
}

// TestSidecarBeatsTheBucketHeuristic: §5.1 lists the marker first for a reason,
// and M4's sidecar writing makes the heuristic moot wherever one exists.
func TestSidecarBeatsTheBucketHeuristic(t *testing.T) {
	root := buildLibrary(t,
		"3d/vendor/actual-pack/.ambar.json",
		"3d/vendor/actual-pack/model.glb",
	)

	res := walkFixture(t, root)

	// Without the sidecar, "vendor" would be the pack (it is at pack level under
	// the 3d bucket). The marker moves it one level deeper, where it belongs.
	if got := packPaths(res); strings.Join(got, ",") != "3d/vendor/actual-pack" {
		t.Errorf("packs = %v, want [3d/vendor/actual-pack]", got)
	}
}

func TestWalkRecordsSizeAndModTime(t *testing.T) {
	root := buildLibrary(t, "pack/sprite.png")
	res := walkFixture(t, root)

	if len(res.Files) != 1 {
		t.Fatalf("indexed %d files, want 1", len(res.Files))
	}
	f := res.Files[0]
	if f.Size == 0 {
		t.Error("size was not recorded")
	}
	if f.ModTime == 0 {
		t.Error("mtime was not recorded; the rescan fast path depends on it")
	}
	if f.Filename != "sprite.png" {
		t.Errorf("filename = %q", f.Filename)
	}
	if f.AbsPath == "" || !filepath.IsAbs(f.AbsPath) {
		t.Errorf("AbsPath = %q, want an absolute path", f.AbsPath)
	}
}

func TestWalkReadsDimensionsWhenAsked(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pack")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sprite.png"), makePNG(t, 32, 16), 0o644); err != nil {
		t.Fatal(err)
	}

	off, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	if off.Files[0].Info.Width != 0 {
		t.Error("dimensions were read even though ReadDimensions was false")
	}

	on, err := Walk(WalkOptions{Root: root, ReadDimensions: true})
	if err != nil {
		t.Fatal(err)
	}
	if on.Files[0].Info.Width != 32 || on.Files[0].Info.Height != 16 {
		t.Errorf("dimensions = %dx%d, want 32x16",
			on.Files[0].Info.Width, on.Files[0].Info.Height)
	}
}

// TestWalkSkipsSymlinks: a symlink is either a duplicate of something already
// indexed or points outside the root, and neither belongs in the index.
func TestWalkSkipsSymlinks(t *testing.T) {
	root := buildLibrary(t, "pack/sprite.png")

	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "pack", "escape.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "pack", "sprite.png"),
		filepath.Join(root, "pack", "alias.png")); err != nil {
		t.Fatal(err)
	}

	res := walkFixture(t, root)

	for _, f := range res.Files {
		if f.Filename == "escape.txt" {
			t.Error("a symlink pointing outside the library was indexed")
		}
		if f.Filename == "alias.png" {
			t.Error("a symlink to an already-indexed file was indexed again")
		}
	}
	if len(res.Files) != 1 {
		t.Errorf("indexed %d files, want 1", len(res.Files))
	}
}

func TestWalkEmptyLibrary(t *testing.T) {
	res := walkFixture(t, t.TempDir())
	if len(res.Files) != 0 || len(res.Packs) != 0 {
		t.Errorf("an empty library produced %d packs and %d files", len(res.Packs), len(res.Files))
	}
	if len(res.Errors) != 0 {
		t.Errorf("an empty library produced errors: %v", res.Errors)
	}
}

func TestWalkRejectsNoRoot(t *testing.T) {
	if _, err := Walk(WalkOptions{}); err == nil {
		t.Error("Walk with no root succeeded")
	}
}

// TestWalkDocumentationOnlyDirectoryIsNotAPack: a folder holding just a README is
// not a pack, or every vendor's docs directory becomes one.
func TestWalkDocumentationOnlyDirectoryIsNotAPack(t *testing.T) {
	root := buildLibrary(t,
		"pack/sprite.png",
		"docs-only/README.md",
		"docs-only/LICENSE.txt",
	)
	res := walkFixture(t, root)

	for _, p := range res.Packs {
		if p.RelPath == "docs-only" && p.Kind == "folder" {
			t.Error("a documentation-only directory was detected as a folder pack")
		}
	}
}

// makePNG builds a real PNG of the given size, so DecodeConfig has something
// genuine to read.
func makePNG(t *testing.T, w, h int) []byte {
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

// TestLooseRootFileDoesNotSwallowPacks is a regression test for an
// ordering-dependent pack-detection bug found against the real target library.
//
// WalkDir visits entries lexically. A loose asset file at the library root used to
// register the root in the "inside a pack" map, so every pack directory visited
// *after* it was treated as part of that synthetic standalone pack. Whether the
// library split into packs therefore depended on whether the loose file's name
// sorted before the pack directories: `TileSet.png` broke it, `zz.png` did not. The
// target library has both a `TileSet_V2.png` at the root and craftpix pack
// directories, so it hit the broken case and indexed 1500 assets as one pack.
func TestLooseRootFileDoesNotSwallowPacks(t *testing.T) {
	// Both names must behave identically: one sorts before the pack directories,
	// the other after.
	for _, loose := range []string{"TileSet.png", "zz-loose.png"} {
		t.Run(loose, func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"craftpix-a/PNG", "craftpix-b/PNG", "raw/craftpix-c"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, dir, "sprite.png"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, loose), []byte("y"), 0o644); err != nil {
				t.Fatal(err)
			}

			res, err := Walk(WalkOptions{Root: root})
			if err != nil {
				t.Fatalf("walk: %v", err)
			}

			kinds := map[string]string{}
			files := map[string]int{}
			for _, p := range res.Packs {
				kinds[p.RelPath] = p.Kind
			}
			for _, f := range res.Files {
				files[f.PackRelPath]++
			}

			// Three real packs, each owning its own file, plus the standalone pack
			// holding only the loose one.
			for _, want := range []string{"craftpix-a", "craftpix-b", "raw/craftpix-c"} {
				if kinds[want] != "folder" {
					t.Errorf("%s: kind = %q, want folder — pack detection was swallowed", want, kinds[want])
				}
				if files[want] != 1 {
					t.Errorf("%s owns %d file(s), want 1", want, files[want])
				}
			}
			if kinds["."] != "standalone" {
				t.Errorf("the loose file should sit in a standalone pack, got %q", kinds["."])
			}
			if files["."] != 1 {
				t.Errorf("the standalone pack owns %d file(s), want only the loose one", files["."])
			}
			if len(res.Packs) != 4 {
				t.Errorf("%d pack(s), want 4: %+v", len(res.Packs), res.Packs)
			}
		})
	}
}

// TestNonAssetsAreNotIndexed covers the M14 fix for a bug the real library exposed:
// IsAssetFile existed from M1 but was only consulted when *confirming* a pack, so
// readmes, licences, coupons and .gitkeep files were indexed as assets and appeared
// in the grid.
func TestNonAssetsAreNotIndexed(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		// Real assets.
		"pack/PNG/hero.png": "art",
		"pack/PSD/hero.psd": "source",
		"pack/tiles.tsx":    "<tileset/>",
		// Everything a vendor pack ships alongside them.
		"pack/readme.txt":      "how to use",
		"pack/License.txt":     "terms",
		"pack/COUPON.pdf":      "%PDF",
		"pack/Free Assets.url": "[InternetShortcut]",
		"pack/pack-source.zip": "PK",
		"pack/notes.md":        "# notes",
		// Dotfiles from git and from Godot.
		"pack/.gitkeep":       "",
		"pack/.gdignore":      "",
		"pack/.gitattributes": "* text=auto",
	}
	for rel, content := range files {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	var indexed []string
	for _, f := range res.Files {
		indexed = append(indexed, f.RelPath)
	}
	sort.Strings(indexed)
	want := []string{"PNG/hero.png", "PSD/hero.psd", "tiles.tsx"}
	if !reflect.DeepEqual(indexed, want) {
		t.Errorf("indexed %v, want only the artwork %v", indexed, want)
	}
	if res.SkippedNonAssets != 9 {
		t.Errorf("SkippedNonAssets = %d, want 9 — the count is what makes the exclusion visible",
			res.SkippedNonAssets)
	}
}

// TestDocumentationOnlyDirectoryIsNotAPack: a directory holding nothing but a readme
// and a licence has no assets in it, so it is neither a pack nor indexed. That was
// the original intent of IsAssetFile and it now holds end to end.
func TestDocumentationOnlyDirectoryIsNotAPack(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"docs/readme.txt":  "nothing here",
		"docs/License.txt": "terms",
		"art/hero.png":     "art",
	} {
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res, err := Walk(WalkOptions{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Packs {
		if p.RelPath == "docs" {
			t.Errorf("a documentation-only directory became a pack: %+v", p)
		}
	}
	if len(res.Packs) != 1 || res.Packs[0].RelPath != "art" {
		t.Errorf("packs = %+v, want only art", res.Packs)
	}
}

package library

import (
	"image"
	"io"
	"os"
	"path/filepath"
	"strings"

	// Registered for image.DecodeConfig. Header-only reads, so the cost is a few
	// dozen bytes per file, not a full decode.
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	_ "golang.org/x/image/bmp"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

// Kind is the §4 asset classification, extended by §5.1 with tilemap and rig.
type Kind string

const (
	KindImage       Kind = "image"
	KindSpritesheet Kind = "spritesheet"
	KindTexture     Kind = "texture"
	KindModel       Kind = "model"
	KindAudio       Kind = "audio"
	KindVideo       Kind = "video"
	KindFont        Kind = "font"
	KindScript      Kind = "script"
	KindMaterial    Kind = "material"
	KindHDRI        Kind = "hdri"
	KindTilemap     Kind = "tilemap" // §5.1: .tmx / .tsx Tiled maps and tilesets
	KindRig         Kind = "rig"     // §5.1: .scml / .scon Spriter projects
	KindOther       Kind = "other"
)

// extKinds maps a lowercase extension (no dot) to a kind.
//
// M1 classifies by extension alone, deliberately:
//
//   - `spritesheet` needs the §6 candidate heuristics (dimensions suggesting a
//     grid, a sibling .json/.atlas, filename patterns) and a confirmation UI.
//     That is M7, so sheets index as `image` here.
//   - `texture` needs filename-suffix heuristics (_albedo, _normal, _roughness)
//     and would produce confident wrong answers without them. Also `image` for now.
//
// Both refinements are pure reclassification of existing rows, so neither
// requires a re-walk.
var extKinds = map[string]Kind{
	// Raster images
	"png": KindImage, "jpg": KindImage, "jpeg": KindImage, "gif": KindImage,
	"bmp": KindImage, "tga": KindImage, "webp": KindImage, "tif": KindImage,
	"tiff": KindImage, "ico": KindImage, "ppm": KindImage, "pgm": KindImage,

	// Editable 2D source art (§6 treats .aseprite as first-class, decoded in M2)
	"psd": KindImage, "aseprite": KindImage, "ase": KindImage,
	"kra": KindImage, "xcf": KindImage,

	// Vector
	"svg": KindImage, "eps": KindImage, "ai": KindImage,

	// High dynamic range (§6)
	"hdr": KindHDRI, "exr": KindHDRI,

	// 3D models (§6)
	"glb": KindModel, "gltf": KindModel, "obj": KindModel, "fbx": KindModel,
	"blend": KindModel, "dae": KindModel, "stl": KindModel, "ply": KindModel,
	"3ds": KindModel, "escn": KindModel,

	// Audio (§6)
	"wav": KindAudio, "ogg": KindAudio, "mp3": KindAudio, "flac": KindAudio,
	"aiff": KindAudio, "aif": KindAudio, "m4a": KindAudio, "opus": KindAudio,
	"mod": KindAudio, "xm": KindAudio, "it": KindAudio, "s3m": KindAudio,

	// Video
	"mp4": KindVideo, "webm": KindVideo, "mov": KindVideo, "avi": KindVideo,
	"mkv": KindVideo, "ogv": KindVideo,

	// Fonts
	"ttf": KindFont, "otf": KindFont, "woff": KindFont, "woff2": KindFont,
	"fnt": KindFont, "bmfont": KindFont,

	// Scripts and shaders
	"gd": KindScript, "cs": KindScript, "lua": KindScript, "py": KindScript,
	"js": KindScript, "gdshader": KindScript, "shader": KindScript,
	"glsl": KindScript, "hlsl": KindScript, "frag": KindScript, "vert": KindScript,

	// Godot resources (§10 writes .import files; .tres/.material describe materials)
	"tres": KindMaterial, "material": KindMaterial, "mtl": KindMaterial,

	// Tiled (§5.1)
	"tmx": KindTilemap, "tsx": KindTilemap, "tiled-project": KindTilemap,

	// Spriter (§5.1)
	"scml": KindRig, "scon": KindRig,
}

// imageExts are the extensions worth attempting a header read on. Kept separate
// from extKinds because .psd and .svg are images but Go's image package cannot
// read their dimensions — attempting it would waste an open() per file.
var imageExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "gif": true,
	"bmp": true, "webp": true, "tif": true, "tiff": true,
}

// FileInfo is what classification produces for one file.
type FileInfo struct {
	Kind   Kind
	Ext    string // lowercase, no leading dot, "" if none
	Width  int    // 0 if unknown
	Height int    // 0 if unknown
}

// Ext returns the lowercase extension of a filename without its dot.
func Ext(filename string) string {
	e := filepath.Ext(filename)
	if e == "" {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(e, "."))
}

// KindForExt maps an extension to a kind, defaulting to KindOther.
func KindForExt(ext string) Kind {
	if k, ok := extKinds[strings.ToLower(ext)]; ok {
		return k
	}
	return KindOther
}

// Classify determines the kind of a file and, for readable raster images, its
// dimensions.
//
// absPath must already have been through safepath. Dimension reading uses
// image.DecodeConfig, which parses only the header — the "extract cheap metadata"
// of §5 step 4. A corrupt or truncated image is not an error: the kind is still
// known from the extension, and the file is still worth indexing. §16 requires
// deliberately broken fixtures to be handled, and this is the handling.
func Classify(absPath string) FileInfo {
	filename := filepath.Base(absPath)
	ext := Ext(filename)

	info := FileInfo{Kind: KindForExt(ext), Ext: ext}
	if !imageExts[ext] {
		return info
	}

	f, err := os.Open(absPath)
	if err != nil {
		return info
	}
	defer f.Close()

	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		// Truncated, corrupt, or a format whose decoder is not registered.
		// Deliberately silent: the row is still useful without dimensions.
		return info
	}
	// Guard against a corrupt header advertising an absurd size.
	if cfg.Width > 0 && cfg.Height > 0 && cfg.Width < 1<<20 && cfg.Height < 1<<20 {
		info.Width, info.Height = cfg.Width, cfg.Height
	}
	return info
}

// ClassifyReader is Classify for an already-open reader, used by the tests and
// available for M4's archive inspection, which sees entries rather than paths.
func ClassifyReader(filename string, r io.Reader) FileInfo {
	ext := Ext(filename)
	info := FileInfo{Kind: KindForExt(ext), Ext: ext}
	if !imageExts[ext] {
		return info
	}
	cfg, _, err := image.DecodeConfig(r)
	if err != nil {
		return info
	}
	if cfg.Width > 0 && cfg.Height > 0 && cfg.Width < 1<<20 && cfg.Height < 1<<20 {
		info.Width, info.Height = cfg.Width, cfg.Height
	}
	return info
}

// IsAssetFile reports whether a filename looks like a library asset rather than
// documentation or metadata.
//
// Used by pack detection: a directory containing only a README and a licence text
// is not "a directory containing asset files directly" (§5.1). The check is
// deliberately generous — an unrecognised extension still counts as an asset,
// because refusing to index an unknown file type would hide it entirely, and
// invariant 2 wants the filesystem fully represented in the index.
func IsAssetFile(filename string) bool {
	// Dotfiles are never assets. A library checked out or synced through git or a
	// game engine collects .gitkeep, .gdignore, .gitattributes and friends, and they
	// have no business appearing in a grid of artwork. The sidecar is a dotfile too
	// and is excluded here for the same reason — it is metadata about assets, read by
	// §3's importer rather than indexed as one.
	if strings.HasPrefix(filename, ".") {
		return false
	}
	switch strings.ToLower(filename) {
	case "readme", "readme.txt", "readme.md", "license", "license.txt",
		"licence", "licence.txt", "license.md", "copyright", "credits.txt":
		return false
	}
	switch Ext(filename) {
	case "txt", "md", "url", "nfo", "pdf", "rtf", "doc", "docx":
		return false
	// A downloaded pack archive sitting in the library is not artwork. §5 routes
	// archives through _inbox and keeps the originals in _archives; one that was
	// dropped straight into the library is provenance at best, and in the grid it is
	// a tile you can neither preview nor use.
	case "zip", "rar", "7z", "tar", "gz", "bz2", "xz", "tgz":
		return false
	// Companion files of a model, meaningless on their own: an .obj's material
	// library, a glTF's binary buffer. They are *served* — the 3D viewer fetches them
	// beside the model through /assets/{id}/file/, which resolves against the
	// filesystem rather than the index — but a tile for `building.mtl` is noise. One
	// real 3D pack contributed 484 such tiles before this rule existed.
	case "mtl", "bin":
		return false
	// Engine-side metadata that sits beside an asset: Godot writes `hero.png.import`
	// next to every imported file, Unity writes `.meta`. They describe an import, not
	// artwork, and they reappear the moment the engine reimports.
	case "import", "meta":
		return false
	}
	return true
}

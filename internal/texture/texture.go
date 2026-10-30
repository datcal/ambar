// Package texture finds the texture a model meant: for the 3D viewer and the browser's
// thumbnail renderer through the server's /assets/{id}/texture route, and for a worker's
// Blender, which has the library mounted and asks directly.
//
// A model names its textures however its author's machine had them, and a vendor's
// source files are the worst case. Synty's FBX files — two hundred packs of them in the
// target library — name Photoshop working files that are not shipped
// (`..\Textures\SimpleTrainsTextures.psd` where the pack has SimpleTrains_Texture_01.png),
// carry typos (`DPolygonZombieBoss_Texture_04_A.png`), and use relative paths whose `..`
// a browser resolves right out of /assets/{id}/file/ before the server ever sees them. A
// measured sample of 741 of them rendered textured four times in a hundred.
//
// So a model's textures are never fetched by the path it carries. Whatever the model named
// goes, verbatim, to Resolve, which finds it nearest first:
//
//  1. exactly as named, relative to the model — a glTF's `textures/wood.png`;
//  2. the same file name, beside the model or in a Textures folder on the way up to the
//     pack root — an FBX's absolute Windows path reduced to its name;
//  3. the same name with an extension a browser can show — the .psd the artist exported
//     the shipped .png from;
//  4. the closest-named colour texture in the nearest folder that has textures at all —
//     the atlas a renamed working file became;
//  5. the colour texture named like the product the model belongs to — its atlas (Atlas).
//
// Only an image a browser can decode is ever returned, and steps 4 and 5 never stand in for a
// normal, emissive or other non-colour map: a wrong base colour still reads as the right
// object, a colour atlas used as a normal map does not. Every candidate goes through
// safepath (invariant 9) and the search never leaves the model's own pack.

package texture

import (
	"os"
	"path"
	"strings"

	"github.com/datcal/ambar/internal/safepath"
)

// browserImageExts are the formats an <img> decodes, which is what a texture must be
// once it reaches three.js.
var browserImageExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "webp": true, "gif": true, "bmp": true,
}

// textureDirNames are the folders beside a model's directory, or one of its parents, that
// textures are kept in.
var textureDirNames = []string{"", "Textures", "textures", "Texture", "texture", "_Textures"}

// nonColourWords mark a texture that is not a base colour. A request naming one is never
// answered by similarity, and a candidate naming one never answers a colour request.
var nonColourWords = []string{
	"normal", "emiss", "rough", "metal", "specular", "gloss", "occlusion", "height",
	"bump", "mask", "alpha", "opacity", "decal",
}

// notTextureWords mark images that sit among textures but are not one: a pack's icon or
// a preview render would otherwise be the "closest name" to anything named after the pack.
var notTextureWords = []string{"icon", "preview", "thumbnail", "screenshot", "promo", "banner"}

// Match is how a texture was found, reported so a wrong guess can be traced.
type Match string

const (
	MatchExact   Match = "exact"
	MatchName    Match = "name"
	MatchStem    Match = "stem"
	MatchSimilar Match = "similar"
	MatchAtlas   Match = "atlas"
)

// Resolve finds the file a model's texture reference means. libraryRoot is the library on
// this machine; modelPath and packRoot are library-relative and slash-separated, the
// model's own path and the root of the pack it belongs to, which the search never leaves.
// It returns the absolute path and how it was found, or false when nothing was meant.
func Resolve(libraryRoot, modelPath, packRoot, ref string) (string, Match, bool) {
	r := resolver{root: libraryRoot}
	ref = strings.ReplaceAll(ref, "\\", "/")
	modelDir := strings.Trim(path.Dir(modelPath), "/")
	if modelDir == "." {
		modelDir = ""
	}

	// 1. Exactly as named, relative to the model, when that stays beneath it.
	clean := strings.TrimPrefix(path.Clean("/"+ref), "/")
	if !strings.HasPrefix(ref, "/") && !strings.Contains(ref, ":") &&
		!strings.HasPrefix(path.Clean(ref), "..") && browserImageExts[extOf(clean)] {
		if abs, ok := r.libraryFile(joinRel(modelDir, clean)); ok {
			return abs, MatchExact, true
		}
	}

	name := path.Base(clean)
	if name == "" || name == "." || name == "/" {
		return "", "", false
	}
	stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))

	// From the model's directory up to the pack root, nearest first. Each level is listed
	// once, with its texture folders.
	levels := r.levels(packRoot, modelDir)

	// 2 and 3: the same name, then the same stem in a format a browser shows.
	wantExact := browserImageExts[extOf(name)]
	for _, level := range levels {
		var stemHit string
		for _, f := range level {
			base := path.Base(f)
			if wantExact && strings.EqualFold(base, name) {
				if abs, ok := r.libraryFile(f); ok {
					return abs, MatchName, true
				}
			}
			if stemHit == "" && browserImageExts[extOf(base)] &&
				strings.ToLower(strings.TrimSuffix(base, path.Ext(base))) == stem {
				stemHit = f
			}
		}
		if stemHit != "" {
			if abs, ok := r.libraryFile(stemHit); ok {
				return abs, MatchStem, true
			}
		}
	}

	// 4. The closest-named colour texture in the nearest level that has any.
	if containsAny(stem, nonColourWords) {
		return "", "", false
	}
	if abs, ok := r.closest(levels, normaliseName(stem)); ok {
		return abs, MatchSimilar, true
	}

	// 5. The product's colour atlas.
	if abs, ok := r.atlas(levels, packRoot, modelDir); ok {
		return abs, MatchAtlas, true
	}
	return "", "", false
}

// Atlas is the colour texture a model is drawn with when it names none that can be found:
// the one named most like the product it belongs to — the folder just below the pack root,
// or the pack itself — in the nearest level that has textures.
//
// Synty ships each product with one colour atlas (PolygonSamurai_Texture_01_A.png) that
// every model in it is UV-mapped to, while its FBX files name working files, some of
// another product's (PolygonElven_Texture_01.png in the Casino), and its OBJ files carry no
// materials at all. Step 4 cannot match those names to anything; the product's name
// matches its atlas.
func Atlas(libraryRoot, modelPath, packRoot string) (string, bool) {
	r := resolver{root: libraryRoot}
	modelDir := strings.Trim(path.Dir(modelPath), "/")
	if modelDir == "." {
		modelDir = ""
	}
	return r.atlas(r.levels(packRoot, modelDir), packRoot, modelDir)
}

func (r resolver) atlas(levels [][]string, packRoot, modelDir string) (string, bool) {
	packRoot = strings.Trim(packRoot, "/")
	var names []string
	if rest, ok := strings.CutPrefix(modelDir+"/", packRoot+"/"); ok && packRoot != "" && rest != "" {
		names = append(names, strings.SplitN(rest, "/", 2)[0])
	}
	if packRoot != "" {
		names = append(names, path.Base(packRoot))
	}
	for _, name := range names {
		if abs, ok := r.closest(levels, normaliseName(name)); ok {
			return abs, true
		}
	}
	return "", false
}

// closest is the colour texture named most like want, in the nearest level that has any.
func (r resolver) closest(levels [][]string, want string) (string, bool) {
	for _, level := range levels {
		best, bestScore := "", 0
		for _, f := range level {
			base := path.Base(f)
			if !browserImageExts[extOf(base)] {
				continue
			}
			candStem := strings.ToLower(strings.TrimSuffix(base, path.Ext(base)))
			if containsAny(candStem, nonColourWords) || containsAny(candStem, notTextureWords) {
				continue
			}
			score := longestCommonSubstring(want, normaliseName(candStem))
			// Shorter, then alphabetically first, among equals: Texture_01 before _02,
			// _01_A before _01_B — the base atlas rather than a variant of it.
			if score > bestScore || (score == bestScore && best != "" && lessName(base, path.Base(best))) {
				best, bestScore = f, score
			}
		}
		if best == "" {
			continue
		}
		// A level with textures answers or the search ends: going further up only finds
		// another product's textures. Half the requested name in common, and at least five
		// characters, is the bar for "this is the one it meant".
		if bestScore >= 5 && bestScore*2 >= len(want) {
			return r.libraryFile(best)
		}
		return "", false
	}
	return "", false
}

// resolver is one resolution's view of the library.
type resolver struct {
	root string
}

// levels lists the image files at each level from the model's directory up to its pack
// root: the directory itself and its texture folders, as library-relative paths. Levels
// with no files are kept, empty, so "nearest" stays a position in the walk.
func (r resolver) levels(packRoot, modelDir string) [][]string {
	packRoot = strings.Trim(packRoot, "/")
	if packRoot == "." {
		packRoot = ""
	}
	var levels [][]string
	dir := modelDir
	for {
		var files []string
		for _, sub := range textureDirNames {
			// The model's own directory is listed flat — its subfolders are other models'.
			// A texture folder is read two levels down, because that is where packs keep
			// their atlases: Synty's Source/Textures/Alts/PolygonCasino_Texture_01_A.png.
			depth := textureDirDepth
			if sub == "" {
				depth = 0
			}
			files = r.listImages(joinRel(dir, sub), depth, files)
		}
		levels = append(levels, files)

		if dir == packRoot || dir == "" {
			return levels
		}
		parent := path.Dir(dir)
		if parent == "." {
			parent = ""
		}
		dir = parent
	}
}

// textureDirDepth is how far below a texture folder images are still "in" it.
const textureDirDepth = 2

// listImages appends the image files under the library-relative directory rel, descending
// depth levels of subfolders.
func (r resolver) listImages(rel string, depth int, into []string) []string {
	abs, err := safepath.ResolveExisting(r.root, rel)
	if err != nil {
		return into
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return into
	}
	for _, e := range entries {
		switch {
		case e.IsDir():
			if depth > 0 {
				into = r.listImages(joinRel(rel, e.Name()), depth-1, into)
			}
		case isImageName(e.Name()):
			into = append(into, joinRel(rel, e.Name()))
		}
	}
	return into
}

// libraryFile resolves a library-relative path to an existing regular file.
func (r resolver) libraryFile(rel string) (string, bool) {
	abs, err := safepath.ResolveExisting(r.root, rel)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return abs, true
}

func joinRel(dir, name string) string {
	switch {
	case dir == "":
		return name
	case name == "":
		return dir
	default:
		return dir + "/" + name
	}
}

func extOf(name string) string {
	return strings.ToLower(strings.TrimPrefix(path.Ext(name), "."))
}

// isImageName admits anything that could be a texture, including the formats a browser
// cannot show: those still count as "a texture is here" for the name and stem steps.
func isImageName(name string) bool {
	switch extOf(name) {
	case "png", "jpg", "jpeg", "webp", "gif", "bmp", "tga", "psd", "tif", "tiff", "dds", "exr":
		return true
	}
	return false
}

func containsAny(s string, words []string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

// normaliseName keeps letters and digits, lowercased, so `SimpleTrains_Texture_01` and
// `SimpleTrainsTextures` compare on what they say rather than how they are punctuated.
func normaliseName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// longestCommonSubstring is the length of the longest run two strings share.
func longestCommonSubstring(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	best := 0
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			if a[i-1] == b[j-1] {
				cur[j] = prev[j-1] + 1
				if cur[j] > best {
					best = cur[j]
				}
			} else {
				cur[j] = 0
			}
		}
		prev, cur = cur, prev
	}
	return best
}

// lessName orders equally good candidates: shorter first, then alphabetically.
func lessName(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return strings.ToLower(a) < strings.ToLower(b)
}

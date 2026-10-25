package server

import (
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/safepath"
)

// Finding the texture a model meant (the 3D viewer and the grid's thumbnail renderer).
//
// A model names its textures however its author's machine had them, and a vendor's
// source files are the worst case. Synty's FBX files — two hundred packs of them in the
// target library — name Photoshop working files that are not shipped
// (`..\Textures\SimpleTrainsTextures.psd` where the pack has SimpleTrains_Texture_01.png),
// carry typos (`DPolygonZombieBoss_Texture_04_A.png`), and use relative paths whose `..`
// a browser resolves right out of /assets/{id}/file/ before the server ever sees them. A
// measured sample of 741 of them rendered textured four times in a hundred.
//
// So the viewer no longer asks for a texture by path. It sends whatever the model named,
// verbatim, to /assets/{id}/texture?ref=…, and this resolves it, nearest first:
//
//  1. exactly as named, relative to the model — a glTF's `textures/wood.png`;
//  2. the same file name, beside the model or in a Textures folder on the way up to the
//     pack root — an FBX's absolute Windows path reduced to its name;
//  3. the same name with an extension a browser can show — the .psd the artist exported
//     the shipped .png from;
//  4. the closest-named colour texture in the nearest folder that has textures at all —
//     the atlas a renamed working file became.
//
// Only an image a browser can decode is ever returned, and step 4 never stands in for a
// normal, emissive or other non-colour map: a wrong base colour still reads as the right
// object, a colour atlas used as a normal map does not. Every candidate goes through
// safepath (invariant 9) and the search never leaves the model's own pack.

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

// textureMatch is how a texture was found, reported in a header so a wrong guess can be
// traced.
type textureMatch string

const (
	matchExact   textureMatch = "exact"
	matchName    textureMatch = "name"
	matchStem    textureMatch = "stem"
	matchSimilar textureMatch = "similar"
)

// handleAssetTexture serves the texture a model named, found as described above.
func (s *Server) handleAssetTexture(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan.", http.StatusGone)
		return
	}
	ref := r.URL.Query().Get("ref")
	if ref == "" || len(ref) > 1024 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	absPath, how, ok := s.resolveTexture(asset, ref)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	file, err := os.Open(absPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer file.Close() //nolint:errcheck
	info, err := file.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// §11: library bytes are never served inline, whatever they are — exactly as the
	// /file/ route serves a model's companions. An <img> does not mind either header.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Ambar-Texture-Match", string(how))
	http.ServeContent(w, r, path.Base(absPath), info.ModTime(), file)
}

// resolveTexture finds the file a model's texture reference means.
func (s *Server) resolveTexture(asset index.Asset, ref string) (string, textureMatch, bool) {
	ref = strings.ReplaceAll(ref, "\\", "/")
	modelDir := strings.Trim(path.Dir(asset.LibraryPath()), "/")
	if modelDir == "." {
		modelDir = ""
	}

	// 1. Exactly as named, relative to the model, when that stays beneath it.
	clean := strings.TrimPrefix(path.Clean("/"+ref), "/")
	if !strings.HasPrefix(ref, "/") && !strings.Contains(ref, ":") &&
		!strings.HasPrefix(path.Clean(ref), "..") && browserImageExts[extOf(clean)] {
		if abs, ok := s.libraryFile(joinRel(modelDir, clean)); ok {
			return abs, matchExact, true
		}
	}

	name := path.Base(clean)
	if name == "" || name == "." || name == "/" {
		return "", "", false
	}
	stem := strings.ToLower(strings.TrimSuffix(name, path.Ext(name)))

	// From the model's directory up to the pack root, nearest first. Each level is listed
	// once, with its texture folders.
	levels := s.textureLevels(asset, modelDir)

	// 2 and 3: the same name, then the same stem in a format a browser shows.
	wantExact := browserImageExts[extOf(name)]
	for _, level := range levels {
		var stemHit string
		for _, f := range level {
			base := path.Base(f)
			if wantExact && strings.EqualFold(base, name) {
				if abs, ok := s.libraryFile(f); ok {
					return abs, matchName, true
				}
			}
			if stemHit == "" && browserImageExts[extOf(base)] &&
				strings.ToLower(strings.TrimSuffix(base, path.Ext(base))) == stem {
				stemHit = f
			}
		}
		if stemHit != "" {
			if abs, ok := s.libraryFile(stemHit); ok {
				return abs, matchStem, true
			}
		}
	}

	// 4. The closest-named colour texture in the nearest level that has any.
	if containsAny(stem, nonColourWords) {
		return "", "", false
	}
	want := normaliseName(stem)
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
			if abs, ok := s.libraryFile(best); ok {
				return abs, matchSimilar, true
			}
		}
		return "", "", false
	}
	return "", "", false
}

// textureLevels lists the image files at each level from the model's directory up to its
// pack root: the directory itself and its texture folders, as library-relative paths.
// Levels with no files are kept, empty, so "nearest" stays a position in the walk.
func (s *Server) textureLevels(asset index.Asset, modelDir string) [][]string {
	packRoot := strings.Trim(asset.PackRelPath, "/")
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
			files = s.listImages(joinRel(dir, sub), depth, files)
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
func (s *Server) listImages(rel string, depth int, into []string) []string {
	abs, err := safepath.ResolveExisting(s.cfg.LibraryRoot, rel)
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
				into = s.listImages(joinRel(rel, e.Name()), depth-1, into)
			}
		case isImageName(e.Name()):
			into = append(into, joinRel(rel, e.Name()))
		}
	}
	return into
}

// libraryFile resolves a library-relative path to an existing regular file.
func (s *Server) libraryFile(rel string) (string, bool) {
	abs, err := safepath.ResolveExisting(s.cfg.LibraryRoot, rel)
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

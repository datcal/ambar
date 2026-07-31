package server

import (
	"errors"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/safepath"
)

// Serving a model and the files it references (M14).
//
// §8's 3D viewer originally loaded a normalised `preview.glb` produced by derive,
// which meant a format derive could not normalise — `.obj`, `.fbx` — had no viewer
// at all and the page said "this needs Blender". That is the wrong answer twice
// over: three.js can read both formats directly in the browser, and a library of
// 3D assets you cannot look at is not a library.
//
// So the viewer now loads the *original* file for those formats. That needs one
// thing this application did not have: a way to fetch the companion files a model
// references. An `.obj` names its `.mtl`, an `.mtl` names its textures, a `.gltf`
// names its `.bin` and its images — all as paths relative to the model.
//
// handleAssetFile is that route, and it is deliberately narrow:
//
//   - The name is resolved relative to the model's own directory and then checked
//     with safepath, so it cannot leave the library (invariant 9) and cannot leave
//     the model's directory subtree either.
//   - Only the extensions a model legitimately references are served.
//   - Served exactly like a download: `Content-Disposition: attachment` and
//     `X-Content-Type-Options: nosniff` (§11), which three.js's loaders do not mind
//     because they fetch bytes rather than navigate.
//
// Because the URL keeps the model's own basename, relative resolution inside the
// loaders just works: /assets/12/file/hero.obj resolves `hero.mtl` to
// /assets/12/file/hero.mtl without the viewer having to rewrite anything.
//
// One thing does have to be looser than "beside the model": textures. An FBX records
// the absolute path the artist had on their own machine —
//
//   C:\Users\Kay Lousberg\...\Export Files\Textures\hexagons_medieval.png
//
// — which every loader reduces to the basename, and the basename is not beside the
// model. In this library the file is real; it is four directories up, in the pack's
// shared `Textures/`. Refusing it means the barrel renders untextured, so a texture
// that is not beside its model is looked up within the model's own *pack*: each
// directory from the model up to the pack root, plus a `Textures/` beside each. Still
// bounded (a fixed handful of stat calls, never a walk), still inside one pack, still
// through safepath.

// modelCompanionExts are the files a model may pull in. Kept as an allow-list
// rather than a deny-list: this route reads arbitrary names out of a model file,
// and the set of things worth serving from that is small and knowable.
var modelCompanionExts = map[string]bool{
	// Geometry and scene files.
	"obj": true, "mtl": true, "fbx": true, "gltf": true, "glb": true, "bin": true,
	// Textures.
	"png": true, "jpg": true, "jpeg": true, "webp": true, "bmp": true, "tga": true,
	"ktx2": true, "dds": true, "hdr": true, "exr": true,
}

// companionTextureExts is the subset of the allow-list the pack-wide lookup applies
// to. Geometry and material files (.mtl, .bin) are genuinely relative to the model and
// a same-named file elsewhere in the pack would be the wrong file, so those stay
// strictly beside the model; only textures get the wider search.
var companionTextureExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "webp": true, "bmp": true, "tga": true,
	"ktx2": true, "dds": true, "hdr": true, "exr": true,
}

// handleAssetFile serves a file from the same directory as a model asset, by name.
func (s *Server) handleAssetFile(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan.", http.StatusGone)
		return
	}

	// The requested name, as the loader wrote it. It may contain a subdirectory
	// ("textures/wood.png"), which is normal in a glTF, but nothing above the model.
	name := strings.TrimPrefix(r.PathValue("name"), "/")
	if name == "" {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// URL-encoded separators and dot segments are resolved by path.Clean before the
	// check below, so "a/../../b" cannot survive as a traversal.
	clean := path.Clean("/" + name)
	rel := strings.TrimPrefix(clean, "/")
	if rel == "" || rel == "." || strings.HasPrefix(rel, "..") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if !modelCompanionExts[strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))] {
		http.Error(w, "this file type is not served to the viewer", http.StatusForbidden)
		return
	}

	// Resolved against the model's directory, then against the library root. Two
	// checks rather than one: the first keeps a model from reading its neighbours'
	// directories, the second is invariant 9.
	dir := path.Dir(asset.LibraryPath())
	if dir == "." {
		dir = ""
	}
	libRel := rel
	if dir != "" {
		libRel = dir + "/" + rel
	}

	absPath, err := safepath.ResolveExisting(s.cfg.LibraryRoot, libRel)
	if err != nil {
		if errors.Is(err, safepath.ErrEscapes) || errors.Is(err, safepath.ErrUnsafeInput) {
			// A model file asking for something outside the library is either broken or
			// hostile; either way it is logged and refused.
			s.log.WarnContext(r.Context(), "refusing a model companion outside the library",
				"asset_id", asset.ID, "requested", name, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Not beside the model. For a texture, look through the pack before giving up.
		found, ok := s.findPackTexture(asset, rel)
		if !ok {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		absPath = found
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

	// §11: library bytes are never served inline, whatever they are.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, path.Base(rel), info.ModTime(), file)
}

// handleAssetFont serves a font's bytes so the detail page can set text in it (M15).
//
// A font's only real preview is the font itself, and the interesting question — "does
// my UI text look right in this?" — can only be answered by typing. The bytes are
// served like any other library content (§11: attachment + nosniff), which the
// FontFace API does not mind because it fetches rather than navigates.
func (s *Server) handleAssetFont(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if asset.Kind != "font" {
		http.Error(w, "not a font", http.StatusNotFound)
		return
	}
	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan.", http.StatusGone)
		return
	}

	absPath, err := safepath.ResolveExisting(s.cfg.LibraryRoot, asset.LibraryPath())
	if err != nil {
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

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Disposition", "attachment")
	w.Header().Set("Cache-Control", "private, max-age=300")
	http.ServeContent(w, r, path.Base(asset.LibraryPath()), info.ModTime(), file)
}

// findPackTexture looks for a texture by basename inside the asset's own pack.
//
// Called only when the name was not found beside the model. The search is a fixed set
// of candidates rather than a directory walk: each directory from the model up to the
// pack root, and a `Textures/` (or `textures/`) beside each of them. That covers how
// every pack in this library is actually laid out, costs a dozen stat calls at worst,
// and cannot wander into another pack — the walk stops at the pack root, and every
// candidate still goes through safepath (invariant 9).
func (s *Server) findPackTexture(asset index.Asset, rel string) (string, bool) {
	if !companionTextureExts[strings.ToLower(strings.TrimPrefix(path.Ext(rel), "."))] {
		return "", false
	}
	base := path.Base(rel)
	if base == "" || base == "." || base == ".." {
		return "", false
	}

	// From the model's directory up to (and including) the pack root, expressed
	// library-relative. A loose asset has no pack directory of its own, in which case
	// packRoot is empty and this is just the model's own directory.
	packRoot := strings.Trim(asset.PackRelPath, "/")
	dir := strings.Trim(path.Dir(asset.LibraryPath()), "/")
	if dir == "." {
		dir = ""
	}

	for {
		for _, candidate := range []string{base, "Textures/" + base, "textures/" + base} {
			libRel := candidate
			if dir != "" {
				libRel = dir + "/" + candidate
			}
			abs, err := safepath.ResolveExisting(s.cfg.LibraryRoot, libRel)
			if err != nil {
				continue
			}
			if info, err := os.Stat(abs); err != nil || info.IsDir() {
				continue
			}
			return abs, true
		}
		if dir == "" || dir == packRoot {
			return "", false
		}
		parent := path.Dir(dir)
		if parent == "." || parent == "/" || parent == dir {
			// Above the pack root: stop rather than search the whole library.
			if packRoot == "" {
				return "", false
			}
			dir = ""
			continue
		}
		dir = parent
	}
}

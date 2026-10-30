package server

import (
	"net/http"
	"os"
	"path"

	"github.com/datcal/ambar/internal/texture"
)

// Finding the texture a model meant, for the 3D viewer and the grid's thumbnail renderer.
//
// The viewer never asks for a texture by the path a model carries: a browser resolves its
// `..` right out of /assets/{id}/file/ before the server sees it, and a vendor's source
// files name Photoshop working files that are not shipped. It sends whatever the model
// named, verbatim, to /assets/{id}/texture?ref=…, and internal/texture finds the file
// that was meant — the same search a worker's Blender uses for its thumbnails.

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

	absPath, how, ok := texture.Resolve(s.cfg.LibraryRoot, asset.LibraryPath(), asset.PackRelPath, ref)
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

package server

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/safepath"
)

// handlePeaks serves the waveform peaks JSON the audio viewer draws (§8).
func (s *Server) handlePeaks(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derive.FilePeaks, "application/json")
}

// handleModelPreview serves the normalised preview.glb the §8 3D viewer loads.
// A generated derivative, so — like the thumbnails — it is safe to serve inline.
func (s *Server) handleModelPreview(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derive.FileModelPreview, "model/gltf-binary")
}

// audioContentType maps an audio extension to its MIME type.
func audioContentType(ext string) string {
	switch ext {
	case "wav":
		return "audio/wav"
	case "mp3":
		return "audio/mpeg"
	case "ogg", "oga":
		return "audio/ogg"
	case "flac":
		return "audio/flac"
	default:
		return "application/octet-stream"
	}
}

// handleAudio streams the original sound file for in-page playback (§8).
//
// Unlike the download handler this serves inline, with an explicit audio MIME
// type — which is safe only because the nosniff header (set globally, §11) stops
// the browser from re-interpreting the bytes as HTML. An <audio> element cannot
// play an attachment, and audio is not an executable content type, so this is the
// one library-original path served inline. ServeContent supplies Range, so
// seeking in a long track does not re-download it.
func (s *Server) handleAudio(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if !asset.IsAudio() {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan.", http.StatusGone)
		return
	}

	absPath, err := safepath.ResolveExisting(s.cfg.LibraryRoot, asset.LibraryPath())
	if err != nil {
		if errors.Is(err, safepath.ErrEscapes) || errors.Is(err, safepath.ErrUnsafeInput) {
			s.log.ErrorContext(r.Context(), "refusing to serve audio outside the root",
				"asset_id", asset.ID, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "This file is no longer readable. Run `ambar scan`.", http.StatusGone)
		return
	}

	file, err := os.Open(absPath)
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", audioContentType(asset.Ext))
	w.Header().Set("ETag", `"`+asset.SHA256+`"`)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	http.ServeContent(w, r, filepath.Base(absPath), info.ModTime(), file)
}

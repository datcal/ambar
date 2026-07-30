package server

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"

	"github.com/HugoSmits86/nativewebp"
	"github.com/datcal/ambar/internal/derive"
)

// Grid thumbnails for 3D models, rendered by the browser (M15).
//
// The problem: a model is viewable on its detail page (three.js reads OBJ, FBX and
// glTF directly), but the *grid* had nothing to show for it — rendering a thumbnail
// server-side needs a renderer, which is exactly why §6 made Blender optional and
// why the grid was full of extension chips.
//
// The browser already has a renderer, and it is already loading these models. So the
// grid asks it: when a model tile has no thumbnail, the page loads that model once
// off-screen, snapshots the canvas, and POSTs the PNG here. One render per model,
// ever — after that it is an ordinary cached derivative like any other.
//
// What this route will not do:
//
//   - Store what the client sent. The PNG is decoded, bounds-checked and re-encoded
//     as WebP by our own encoder, so a malformed or hostile file cannot be handed back
//     to another user later.
//   - Accept anything but a plausible thumbnail: a size cap, a pixel cap, and PNG only.
//   - Overwrite a thumbnail that already exists. A derivative that is present was
//     produced by derive (or by an earlier render) and is not the client's to replace.

// maxBrowserThumbBytes caps the upload. A 512×512 PNG of a model is tens of
// kilobytes; a megabyte is generous and still bounded.
const maxBrowserThumbBytes = 1 << 20

// maxBrowserThumbPixels bounds the decode, the same defence §5 applies to archives
// and §6 to images.
const maxBrowserThumbPixels = 4096 * 4096

// handleModelThumbUpload stores a browser-rendered thumbnail for a model.
func (s *Server) handleModelThumbUpload(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if s.cfg.LibraryReadonly {
		// The derivative cache lives under the data root, not the library, so this is
		// not strictly a library write — but a read-only deployment is saying "do not
		// change anything", and that is worth honouring literally.
		http.Error(w, "this instance is read-only", http.StatusForbidden)
		return
	}
	if asset.ViewerFormat() == "" {
		http.Error(w, "this asset is not a model the browser can render", http.StatusBadRequest)
		return
	}

	relDir, err := derive.Dir(asset.SHA256)
	if err != nil {
		s.log.ErrorContext(r.Context(), "bad content hash for thumbnail", "asset_id", asset.ID, "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	outDir := filepath.Join(s.cfg.DataRoot, relDir)
	thumbPath := filepath.Join(outDir, derive.FileThumb)

	// Already thumbnailed — by derive, by Blender, or by an earlier render. Not the
	// client's to replace, and saying so plainly stops a page from looping.
	if _, err := os.Stat(thumbPath); err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	body := http.MaxBytesReader(w, r.Body, maxBrowserThumbBytes)
	raw, err := io.ReadAll(body)
	if err != nil {
		http.Error(w, "the thumbnail is too large", http.StatusRequestEntityTooLarge)
		return
	}

	img, err := decodeThumbPNG(raw)
	if err != nil {
		s.log.WarnContext(r.Context(), "rejected a browser thumbnail",
			"asset_id", asset.ID, "error", err)
		http.Error(w, "that is not a usable thumbnail", http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		s.log.ErrorContext(r.Context(), "could not create the derivative directory", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Re-encoded with our own encoder: the bytes served to anyone else are ours.
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, img, nil); err != nil {
		s.log.ErrorContext(r.Context(), "could not encode a browser thumbnail", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := writeFileAtomic(thumbPath, buf.Bytes()); err != nil {
		s.log.ErrorContext(r.Context(), "could not write a browser thumbnail", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// The same image doubles as the preview, so opening the asset does not re-render.
	if err := writeFileAtomic(filepath.Join(outDir, derive.FilePreview), buf.Bytes()); err != nil {
		s.log.WarnContext(r.Context(), "could not write the browser preview", "error", err)
	}

	// The grid decides what to show from derive_state, so record that a preview now
	// exists for this content. Keyed by hash, like every other derivative: every copy
	// of these bytes gets the thumbnail.
	if _, err := s.db.Writer.ExecContext(r.Context(), `
		UPDATE assets
		SET derive_state = 'ok', derive_error = '', derive_version = ?, updated_at = unixepoch()
		WHERE sha256 = ? AND derive_state != 'ok'`, derive.Version, asset.SHA256); err != nil {
		s.log.ErrorContext(r.Context(), "could not record a browser thumbnail", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	s.log.InfoContext(r.Context(), "stored a browser-rendered model thumbnail",
		"asset_id", asset.ID, "bytes", buf.Len())
	w.WriteHeader(http.StatusNoContent)
}

// decodeThumbPNG accepts only a PNG of plausible thumbnail dimensions.
func decodeThumbPNG(raw []byte) (image.Image, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("not a PNG: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 {
		return nil, fmt.Errorf("empty image")
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxBrowserThumbPixels {
		return nil, fmt.Errorf("%dx%d is larger than a thumbnail", cfg.Width, cfg.Height)
	}
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("decode PNG: %w", err)
	}
	return img, nil
}

// writeFileAtomic writes through a temporary file, so a crash cannot leave a
// half-written derivative that later decodes as garbage.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-thumb-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

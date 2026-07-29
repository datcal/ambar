package server

import (
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/safepath"
)

// handleAssets renders the grid (§8, M1: filenames and metadata, no thumbnails).
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	opts := index.ListOptions{
		Query:          strings.TrimSpace(q.Get("q")),
		Kind:           q.Get("kind"),
		Cursor:         q.Get("cursor"),
		IncludeMissing: q.Get("missing") == "1",
	}
	if raw := q.Get("pack"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			opts.PackID = id
		}
	}

	// Groups, not individual files: §5.1's collapsing is what makes the grid usable.
	page, err := s.index.ListGroups(r.Context(), opts)
	if err != nil {
		// A malformed cursor is the user's URL being wrong, not a server fault.
		if strings.Contains(err.Error(), "cursor") {
			http.Redirect(w, r, "/assets", http.StatusSeeOther)
			return
		}
		s.log.ErrorContext(r.Context(), "listing assets failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	stats, err := s.index.Stats(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "asset stats failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Page = page
	data.Stats = &stats
	data.Search = opts.Query
	data.Kind = opts.Kind
	data.IncludeMissing = opts.IncludeMissing
	data.NextURL = nextPageURL(r, page.NextCursor)

	s.render(w, r, "assets.html", http.StatusOK, data)
}

// handleAsset renders one asset, plus its format variants.
//
// §5.1: "The grid shows one entry per group; the detail panel lists variants with
// download links per format."
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	data := s.newPageData(r)
	data.Asset = &asset

	// A missing group is not fatal — an asset indexed but not yet grouped still has a
	// usable detail page.
	if group, err := s.index.GroupOf(r.Context(), asset.ID); err == nil {
		data.Group = &group
		if variants, err := s.index.Variants(r.Context(), group.ID); err == nil {
			data.Variants = variants
		}
	}

	s.render(w, r, "asset.html", http.StatusOK, data)
}

// handleAssetDownload serves the original bytes.
//
// This is the first handler that reads from the library, so it is where §11's
// serving rules apply:
//
//   - The path is built only from the stored rel_path, never from request input,
//     and still goes through safepath — a rel_path that somehow escaped the root
//     (a bad migration, a hand-edited database) must not be served.
//   - Content-Disposition: attachment, because an .html or .svg served inline from
//     the app origin is stored XSS. The nosniff header from the middleware is the
//     other half of that defence.
//
// ServeContent supplies ETag and Range handling, which §10 needs for the API in
// M8 — "a 200 MB model download that drops should resume, not restart".
func (s *Server) handleAssetDownload(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan. "+
			"Its index entry is kept, but there is nothing to download.", http.StatusGone)
		return
	}

	absPath, err := safepath.ResolveExisting(s.cfg.LibraryRoot, asset.LibraryPath())
	if err != nil {
		// Either the file vanished since the last scan, or the stored path is not
		// under the root. The first is ordinary; the second is a bug or tampering,
		// so both are logged and neither is described to the client.
		s.log.ErrorContext(r.Context(), "refusing to serve asset",
			"asset_id", asset.ID, "rel_path", asset.LibraryPath(), "error", err)
		if errors.Is(err, safepath.ErrEscapes) || errors.Is(err, safepath.ErrUnsafeInput) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "This file is no longer readable. Run `ambar scan` to update the index.",
			http.StatusGone)
		return
	}

	file, err := os.Open(absPath)
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not open asset", "asset_id", asset.ID, "error", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not stat asset", "asset_id", asset.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Attachment, always. The filename is quoted and escaped so a name containing
	// a quote or a newline cannot break out of the header.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": asset.Filename}))
	// An explicit generic type: letting Go sniff one would undermine nosniff.
	w.Header().Set("Content-Type", "application/octet-stream")
	// The content hash is a perfect validator, and the index already has it.
	w.Header().Set("ETag", `"`+asset.SHA256+`"`)
	// Library content is immutable in practice (invariant 1), but a private cache
	// is as far as this should go: the response is behind auth.
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")

	http.ServeContent(w, r, asset.Filename, info.ModTime(), file)
}

// lookupAsset parses the id and fetches the asset, writing the error response
// itself if either fails.
func (s *Server) lookupAsset(w http.ResponseWriter, r *http.Request) (index.Asset, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return index.Asset{}, false
	}

	asset, err := s.index.Get(r.Context(), id)
	if errors.Is(err, index.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return index.Asset{}, false
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "fetching asset failed", "asset_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return index.Asset{}, false
	}
	return asset, true
}

// nextPageURL preserves the current filters while advancing the cursor, so paging
// through a filtered search does not silently drop the filter.
func nextPageURL(r *http.Request, cursor string) string {
	if cursor == "" {
		return ""
	}
	q := r.URL.Query()
	q.Set("cursor", cursor)
	return r.URL.Path + "?" + q.Encode()
}

// FormatBytes renders a size the way a human reads it. Used by the templates.
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	value := float64(b)
	units := []string{"KB", "MB", "GB", "TB"}
	var suffix string
	for _, u := range units {
		value /= unit
		suffix = u
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}

// libraryDir is the directory part of an asset's path, for display.
func libraryDir(libPath string) string {
	dir := filepath.Dir(libPath)
	if dir == "." {
		return ""
	}
	return dir
}

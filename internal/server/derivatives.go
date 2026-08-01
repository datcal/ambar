package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/safepath"
)

// handleThumb serves a generated thumbnail.
//
// Unlike the original-file download, this may be served *inline*: the bytes came from
// our own encoder rather than from the library, so there is no stored-XSS surface for
// §11 to worry about. That is the whole reason preview.webp exists — the 2D viewer needs
// an inline image, and the original can never be one.
func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	name := derive.FileThumb
	switch r.URL.Query().Get("size") {
	case "1024", "2x":
		name = derive.FileThumb2x
	case "alpha":
		// The transparent version §6 wants kept alongside the composited one.
		name = derive.FileThumbAlpha
	}
	s.serveDerivative(w, r, asset, name, "image/webp")
}

// handlePreview serves the full-size preview the 2D viewer loads.
func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derive.FilePreview, "image/webp")
}

// handleAnimation serves the animated GIF preview, for §6's "hover plays animated
// previews" in the grid.
func (s *Server) handleAnimation(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derive.FileAnimation, "image/gif")
}

// serveDerivative sends one file from the derivative directory.
//
// The path is assembled from the content hash and a fixed filename, never from request
// input — and it still goes through safepath against the data root, because a hash that
// somehow escaped validation must not become a path traversal.
func (s *Server) serveDerivative(w http.ResponseWriter, r *http.Request,
	asset index.Asset, filename, contentType string) {

	relDir, err := derive.Dir(asset.SHA256)
	if err != nil {
		// A malformed hash in the database is a bug, not a client error.
		s.log.ErrorContext(r.Context(), "asset has an unusable content hash",
			"asset_id", asset.ID, "sha256", asset.SHA256, "error", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	absPath, err := safepath.ResolveExisting(s.cfg.DataRoot, filepath.Join(relDir, filename))
	if err != nil {
		if errors.Is(err, safepath.ErrEscapes) || errors.Is(err, safepath.ErrUnsafeInput) {
			s.log.ErrorContext(r.Context(), "refusing to serve a derivative outside the data root",
				"asset_id", asset.ID, "error", err)
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Not generated yet, or generation failed. A 404 with an explanation beats a
		// broken image with no clue why.
		s.derivativeMissing(w, asset, filename)
		return
	}

	file, err := os.Open(absPath)
	if err != nil {
		s.derivativeMissing(w, asset, filename)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", contentType)
	// Derivatives are immutable for a given content hash and derive version, so a
	// long private cache is safe and makes the grid feel instant on a second visit.
	// Private rather than public because the response is behind authentication.
	w.Header().Set("Cache-Control", "private, max-age=31536000, immutable")
	w.Header().Set("ETag", fmt.Sprintf(`"%s-%d-%s"`, asset.SHA256[:16], derive.Version, filename))

	http.ServeContent(w, r, filename, info.ModTime(), file)
}

// derivativeMissing explains why there is no image, using the asset's derive state.
//
// §12 wants failures visible rather than silent; a thumbnail that just 404s tells the
// user nothing about whether it is still queued, unsupported, or broken.
func (s *Server) derivativeMissing(w http.ResponseWriter, asset index.Asset, filename string) {
	w.Header().Set("Cache-Control", "no-store")

	switch asset.DeriveState {
	case derive.StatePending:
		// Retry-After so a polling grid does not hammer the endpoint.
		w.Header().Set("Retry-After", "5")
		http.Error(w, "This preview has not been generated yet.", http.StatusServiceUnavailable)
	case derive.StateUnsupported:
		http.Error(w, "No preview is available for this format: "+asset.DeriveError,
			http.StatusNotFound)
	case derive.StateFailed:
		http.Error(w, "Generating this preview failed: "+asset.DeriveError, http.StatusNotFound)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// --- the jobs page (§12) ----------------------------------------------------

// handleJobs lists recent jobs.
//
// §12: "Job failures inspectable in the UI, not only in container logs — a silently
// failing derivative pipeline is easy to miss for weeks." This page is that.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")

	recent, err := s.jobs.Recent(r.Context(), state, 200)
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing jobs failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jobStats, err := s.jobs.Stats(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "job stats failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	deriveStats, err := derive.LoadStats(r.Context(), s.db)
	if err != nil {
		s.log.ErrorContext(r.Context(), "derive stats failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Nav = "jobs"
	data.Jobs = recent
	data.JobStats = &jobStats
	data.DeriveStats = &deriveStats
	data.JobState = state
	s.render(w, r, "jobs.html", http.StatusOK, data)
}

// The old handleScan lived here and redirected to /jobs. M16 replaced it with handleScanNow
// (jobstatus.go), which acknowledges in place and leaves the user on the page they were on.

// handleRetryFailed requeues failed work.
//
// §6: "Failures recorded in derive_state with a 'retry failed derivatives' action in
// the UI." Both halves are reset — the asset rows back to pending, and the job rows
// back to queued — because either alone would leave the other stuck.
func (s *Server) handleRetryFailed(w http.ResponseWriter, r *http.Request) {
	assets, err := derive.ResetFailed(r.Context(), s.db)
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not reset failed derivatives", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	jobsRequeued, err := s.jobs.RetryFailed(r.Context(), "")
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not requeue failed jobs", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Re-enqueue derives for everything just reset, since resetting the asset rows
	// alone would leave nothing scheduled.
	enqueued, err := derive.EnqueueStale(r.Context(), s.db, s.jobs)
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not enqueue reset derivatives", "error", err)
	}

	s.log.InfoContext(r.Context(), "retrying failed work",
		"assets_reset", assets, "jobs_requeued", jobsRequeued, "derives_enqueued", enqueued)

	http.Redirect(w, r, "/jobs", http.StatusSeeOther)
}

// formatJobAge renders a timestamp as a short relative age for the jobs table.
func formatJobAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

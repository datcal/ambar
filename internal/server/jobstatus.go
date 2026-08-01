package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
)

// Live job status (M16).
//
// §12 asked for "pollable status" and what existed was a page you reloaded by hand. Worse, two
// other pages *told you to*: junk and trash both said "watch background work, then reload".
// This endpoint is the thing that makes those sentences unnecessary — the sidebar line and the
// jobs page both poll it, and only while there is something to poll for.

// activeJob is one running job, flattened for the browser.
type activeJob struct {
	ID      int64  `json:"id"`
	Type    string `json:"type"`
	Note    string `json:"note,omitempty"`
	Done    int64  `json:"done,omitempty"`
	Total   int64  `json:"total,omitempty"`
	Percent int    `json:"percent"`
}

// jobStatus is the whole answer: enough to decide whether to keep polling, and enough to
// render a line about it without a second request.
type jobStatus struct {
	Queued  int         `json:"queued"`
	Running int         `json:"running"`
	Failed  int         `json:"failed"`
	Active  []activeJob `json:"active"`
	// Idle is the signal the poller actually cares about: nothing running, nothing queued.
	Idle bool `json:"idle"`
	// LastScan is the sidebar's line, so a finished scan updates it without a page load.
	LastScanAgo    string `json:"last_scan_ago,omitempty"`
	LastScanAssets int    `json:"last_scan_assets,omitempty"`
}

func (s *Server) handleJobStatus(w http.ResponseWriter, r *http.Request) {
	stats, err := s.jobs.Stats(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "job stats failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	out := jobStatus{
		Queued:  stats.Queued,
		Running: stats.Running,
		Failed:  stats.Failed,
		Idle:    stats.Pending() == 0,
	}

	if active, err := s.jobs.Active(r.Context()); err != nil {
		s.log.ErrorContext(r.Context(), "listing active jobs failed", "error", err)
	} else {
		for _, job := range active {
			out.Active = append(out.Active, activeJob{
				ID:      job.ID,
				Type:    job.Type,
				Note:    job.ProgressNote,
				Done:    job.ProgressDone,
				Total:   job.ProgressTotal,
				Percent: job.Percent(),
			})
		}
	}

	// The scan line comes from the same cached snapshot the sidebar rendered, so a poll costs
	// nothing while the TTL holds — and the invalidation on a finished scan is what makes the
	// number move.
	if snap, err := s.nav.get(r.Context(), s.buildSidebar); err == nil && snap.LastScan != nil {
		out.LastScanAgo = snap.LastScan.Ago()
		out.LastScanAssets = snap.LastScan.Assets
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		s.log.DebugContext(r.Context(), "client went away mid-response", "error", err)
	}
}

// handleScanNow enqueues a scan and answers in place.
//
// The old handler redirected to /jobs, which threw away the grid, the search and the scroll
// position to show a table that did not refresh either. A scan is a background job: the button
// should acknowledge it and leave you where you were.
func (s *Server) handleScanNow(w http.ResponseWriter, r *http.Request) {
	var username string
	if u, ok := auth.UserFromContext(r.Context()); ok {
		username = u.Username
	}

	id, err := index.EnqueueScan(r.Context(), s.jobs, index.ScanJobPayload{ReadDimensions: true})
	switch {
	case errors.Is(err, jobs.ErrDuplicate):
		// Already queued or running. Pressing the button twice is not an error, and telling
		// the user it is would only teach them to distrust the button.
		s.log.InfoContext(r.Context(), "scan requested but one is already pending")
	case err != nil:
		s.log.ErrorContext(r.Context(), "could not enqueue a scan", "error", err)
		http.Error(w, "could not start the scan", http.StatusInternalServerError)
		return
	default:
		s.log.InfoContext(r.Context(), "scan enqueued", "job_id", id, "user", username)
	}

	// The counts and the "scanned N ago" line are about to change.
	s.nav.invalidate()

	// A browser without JavaScript posted an ordinary form, so it gets an ordinary redirect —
	// back where it came from rather than to /jobs.
	if r.Header.Get("X-Requested-With") == "" {
		back := r.Header.Get("Referer")
		if back == "" {
			back = "/"
		}
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"job_id": id, "queued": true})
}

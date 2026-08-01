package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/junk"
)

// handleJunk shows the junk report (§9.1, M12).
//
// Reporting only: the page lists candidates and how many bytes each finding would
// reclaim, sorted by largest win, and never offers a destructive action. The
// deliberate, human-selected removal path — with trash staging and the safety
// invariants — is M13. Building selection-for-deletion UI before that safety net
// exists would be exactly the hazard §9.1 warns against.
//
// The report is produced by a background sweep (invariant 8: no library walk inside
// a GET). The page reads the cached result and offers a button to run a fresh sweep.
func (s *Server) handleJunk(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Nav = "junk"
	data.Flash = r.URL.Query().Get("msg")

	stored, err := junk.LoadReport(s.cfg.DataRoot)
	if err != nil {
		s.log.ErrorContext(r.Context(), "loading junk report failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.Junk = stored
	data.JunkRunning = s.junkSweepPending(r)

	s.render(w, r, "junk.html", http.StatusOK, data)
}

// handleJunkScan enqueues a junk sweep and returns to the page. It changes no
// library data — it only schedules a read-only report — but it is still a POST so a
// browser refresh cannot silently re-trigger the walk, and CSRF still applies.
func (s *Server) handleJunkScan(w http.ResponseWriter, r *http.Request) {
	_, err := s.jobs.Enqueue(r.Context(), junk.JobType, struct{}{}, jobs.EnqueueOptions{
		// A user-triggered sweep should not wait behind a queue of derive jobs.
		Priority: 5,
		// One pending sweep at a time; a double-click coalesces rather than stacking.
		DedupeKey: junk.JobType,
	})
	msg := "Scanning for junk in the background — this page will show the result when it finishes."
	switch {
	case errors.Is(err, jobs.ErrDuplicate):
		msg = "A junk scan is already running."
	case err != nil:
		s.log.ErrorContext(r.Context(), "enqueue junk sweep failed", "error", err)
		http.Error(w, "could not start the scan", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/junk?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// junkSweepPending reports whether a sweep is queued or running, so the page can say
// so rather than looking idle after the button was pressed.
func (s *Server) junkSweepPending(r *http.Request) bool {
	var n int
	err := s.db.Reader.QueryRowContext(r.Context(),
		`SELECT count(*) FROM jobs WHERE type = ? AND state IN ('queued', 'running')`,
		junk.JobType).Scan(&n)
	if err != nil {
		s.log.ErrorContext(r.Context(), "checking for a running junk sweep failed", "error", err)
		return false
	}
	return n > 0
}

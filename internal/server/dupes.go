package server

import (
	"errors"
	"net/http"
	"net/url"

	"github.com/datcal/ambar/internal/dupes"
	"github.com/datcal/ambar/internal/jobs"
)

// handleDupes shows the duplicate report (§9.1, M13).
//
// Everything on the page starts unselected. §9.1 rules out "any pre-checked
// checkbox, pre-selected row, or default-selected recommendation", so the keep
// hints are labels next to rows and nothing more — no input carries a checked
// attribute, and no form is submitted without a person choosing rows and then
// confirming a preview.
//
// The report itself comes from a background sweep (invariant 8): comparing every
// pack's hash set and every image's perceptual hash is not GET work.
func (s *Server) handleDupes(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Nav = "dupes"
	data.Flash = r.URL.Query().Get("msg")

	stored, err := dupes.LoadReport(s.cfg.DataRoot)
	if err != nil {
		s.log.ErrorContext(r.Context(), "loading dupes report failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.Dupes = stored
	data.DupesRunning = s.jobPending(r, dupes.JobType)
	data.LinkMode = s.cfg.DedupeLinkMode
	data.LinkSupport = s.linkSupport()

	s.render(w, r, "dupes.html", http.StatusOK, data)
}

// handleDupesScan enqueues a duplicate sweep. A POST, so a refresh cannot
// re-trigger the work and CSRF applies, even though the sweep itself only reads.
func (s *Server) handleDupesScan(w http.ResponseWriter, r *http.Request) {
	_, err := s.jobs.Enqueue(r.Context(), dupes.JobType, struct{}{}, jobs.EnqueueOptions{
		Priority:  5,
		DedupeKey: dupes.JobType,
	})
	msg := "Looking for duplicates in the background — reload this page when the job finishes."
	switch {
	case errors.Is(err, jobs.ErrDuplicate):
		msg = "A duplicate scan is already running."
	case err != nil:
		s.log.ErrorContext(r.Context(), "enqueue dupes sweep failed", "error", err)
		http.Error(w, "could not start the scan", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/dupes?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// jobPending reports whether a job of this type is queued or running, so a page
// can say so rather than looking idle after the button was pressed.
func (s *Server) jobPending(r *http.Request, jobType string) bool {
	var n int
	err := s.db.Reader.QueryRowContext(r.Context(),
		`SELECT count(*) FROM jobs WHERE type = ? AND state IN ('queued', 'running')`,
		jobType).Scan(&n)
	if err != nil {
		s.log.ErrorContext(r.Context(), "checking for a running job failed", "type", jobType, "error", err)
		return false
	}
	return n > 0
}

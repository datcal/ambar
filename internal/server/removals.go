package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/httpx"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/removal"
)

// The removal flow, shared by the junk view and the duplicate view (§9.1).
//
// Three steps, and the middle one is not skippable:
//
//	POST /removals/plan    → a preview: every affected path, the total bytes, and
//	                         every refusal with its reason. Nothing has moved.
//	POST /removals/apply   → enqueues the job that carries the plan out.
//	POST /removals/script  → the same plan as a shell script to read and run by hand.
//
// The plan is not carried through the flow as a token or a session value. Each step
// re-reads the selected paths from the form and re-plans from scratch, so the
// safety rules are enforced against the library as it is at that moment rather than
// as it was when the page was rendered. The apply job then plans a *third* time, in
// the worker, for the same reason.

// removalForm is one submitted selection. Every finding renders its own form, so
// root, action and finding are uniform per submission and only the paths vary —
// which is what keeps the encoding free of delimiters that a filename could
// contain.
type removalForm struct {
	Root     removal.Root
	Action   removal.Action
	Finding  string
	Reason   string
	Paths    []string
	KeepPath string
	Transfer *removal.Transfer
}

// parseRemovalForm reads the selection. It validates the shape only: whether any
// of it may actually happen is the Planner's decision.
func parseRemovalForm(r *http.Request) (removalForm, error) {
	if err := r.ParseForm(); err != nil {
		return removalForm{}, fmt.Errorf("could not read the form: %w", err)
	}

	form := removalForm{
		Root:     removal.Root(strings.TrimSpace(r.PostFormValue("root"))),
		Action:   removal.Action(strings.TrimSpace(r.PostFormValue("action"))),
		Finding:  strings.TrimSpace(r.PostFormValue("finding")),
		Reason:   strings.TrimSpace(r.PostFormValue("reason")),
		KeepPath: strings.TrimSpace(r.PostFormValue("keep")),
	}
	if form.Root == "" {
		form.Root = removal.RootLibrary
	}
	if form.Action == "" {
		form.Action = removal.ActionTrash
	}
	if !form.Root.Valid() || !form.Action.Valid() {
		return form, fmt.Errorf("unknown root or action")
	}

	for _, p := range r.PostForm["path"] {
		p = strings.TrimSpace(p)
		if p != "" {
			form.Paths = append(form.Paths, p)
		}
	}
	if len(form.Paths) == 0 {
		return form, errors.New("nothing was selected")
	}
	if form.Action == removal.ActionLink && form.KeepPath == "" {
		return form, errors.New("choose which copy to keep before linking")
	}

	// A pack-subset removal carries the curation transfer §9.1 requires first.
	from, fromErr := strconv.ParseInt(r.PostFormValue("transfer_from"), 10, 64)
	to, toErr := strconv.ParseInt(r.PostFormValue("transfer_to"), 10, 64)
	if fromErr == nil && toErr == nil && from > 0 && to > 0 && from != to {
		form.Transfer = &removal.Transfer{
			FromPackID: from, ToPackID: to,
			What: strings.TrimSpace(r.PostFormValue("transfer_what")),
		}
	}
	return form, nil
}

// targets turns the form into planner input.
func (f removalForm) targets() []removal.Target {
	targets := make([]removal.Target, 0, len(f.Paths))
	for _, p := range f.Paths {
		targets = append(targets, removal.Target{
			Root: f.Root, Path: p, Action: f.Action,
			KeepPath: f.KeepPath, Finding: f.Finding,
		})
	}
	return targets
}

// plan builds the preview for a submitted form.
func (s *Server) planFor(r *http.Request, form removalForm) (*removal.Plan, error) {
	reason := form.Reason
	if reason == "" {
		reason = "manual selection"
	}
	plan, err := s.removals.Plan(r.Context(), reason, form.targets())
	if err != nil {
		return nil, err
	}
	if form.Transfer != nil && len(plan.Ops) > 0 {
		plan.Transfers = []removal.Transfer{*form.Transfer}
	}
	return plan, nil
}

// handleRemovalPlan renders the confirmation step. §9.1: "Always preview. Show the
// complete list of affected paths and the total bytes before anything moves."
func (s *Server) handleRemovalPlan(w http.ResponseWriter, r *http.Request) {
	form, err := parseRemovalForm(r)
	if err != nil {
		s.renderRemovalError(w, r, err)
		return
	}
	plan, err := s.planFor(r, form)
	if err != nil {
		s.log.ErrorContext(r.Context(), "planning a removal failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Plan = plan
	data.PlanForm = form
	data.LinkMode = s.cfg.DedupeLinkMode
	data.LinkSupport = s.linkSupport()
	data.TrashDir = s.cfg.TrashDir
	s.render(w, r, "removal_confirm.html", http.StatusOK, data)
}

// handleRemovalApply enqueues the planned work. The plan travels in the payload,
// and the worker re-plans before touching anything.
func (s *Server) handleRemovalApply(w http.ResponseWriter, r *http.Request) {
	form, err := parseRemovalForm(r)
	if err != nil {
		s.renderRemovalError(w, r, err)
		return
	}
	if s.cfg.LibraryReadonly && form.Root == removal.RootLibrary {
		s.renderRemovalError(w, r, errors.New("the library is mounted read-only, so nothing can be removed from it"))
		return
	}

	plan, err := s.planFor(r, form)
	if err != nil {
		s.log.ErrorContext(r.Context(), "planning a removal failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if plan.Empty() {
		s.renderRemovalError(w, r, errors.New("nothing in that selection can be removed; see the reasons on the previous page"))
		return
	}

	actor := removal.Actor{IP: httpx.ClientIPString(r.Context())}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		id := u.ID
		actor.UserID, actor.Username = &id, u.Username
	}

	if _, err := s.jobs.Enqueue(r.Context(), removal.JobType,
		removal.JobPayload{Plan: *plan, Actor: actor}, jobs.EnqueueOptions{Priority: 8}); err != nil {
		s.log.ErrorContext(r.Context(), "enqueue removal failed", "error", err)
		http.Error(w, "could not start the removal", http.StatusInternalServerError)
		return
	}

	// Asset counts are about to change, and the sidebar shows them from a cached
	// snapshot (sidebar.go).
	s.nav.invalidate()

	msg := fmt.Sprintf("Removing %d item(s) in the background. Files move to the trash, where they can be restored.",
		len(plan.Ops))
	http.Redirect(w, r, "/trash?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleRemovalScript exports the selection as a shell script (§9.1: "expect this
// to be the primary path rather than a fallback").
func (s *Server) handleRemovalScript(w http.ResponseWriter, r *http.Request) {
	form, err := parseRemovalForm(r)
	if err != nil {
		s.renderRemovalError(w, r, err)
		return
	}
	plan, err := s.planFor(r, form)
	if err != nil {
		s.log.ErrorContext(r.Context(), "planning a removal failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	actor := ""
	if u, ok := auth.UserFromContext(r.Context()); ok {
		actor = u.Username
	}

	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ambar-removal.sh"`)
	w.Header().Set("Cache-Control", "no-store")
	if err := removal.WriteScript(w, plan, removal.ScriptOptions{
		LibraryRoot: s.cfg.LibraryRoot,
		DataRoot:    s.cfg.DataRoot,
		TrashDir:    s.cfg.TrashDir,
		LinkMode:    s.cfg.DedupeLinkMode,
		GeneratedAt: time.Now(),
		Actor:       actor,
	}); err != nil {
		s.log.ErrorContext(r.Context(), "writing the removal script failed", "error", err)
	}
}

// renderRemovalError reports a bad or empty selection as a page rather than a bare
// 400: the user is mid-flow and needs to know what to do next.
func (s *Server) renderRemovalError(w http.ResponseWriter, r *http.Request, err error) {
	data := s.newPageData(r)
	data.Flash = err.Error()
	data.Plan = &removal.Plan{}
	data.LinkMode = s.cfg.DedupeLinkMode
	data.TrashDir = s.cfg.TrashDir
	s.render(w, r, "removal_confirm.html", http.StatusBadRequest, data)
}

// --- the trash --------------------------------------------------------------

// handleTrash lists the trash batches, newest first, with what each holds and how
// to get it back.
func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)
	data.Nav = "trash"
	data.Flash = r.URL.Query().Get("msg")
	data.TrashDir = s.cfg.TrashDir
	data.TrashRetention = s.cfg.TrashRetention
	data.RemovalRunning = s.jobPending(r, removal.JobType)

	batches, err := s.trash.ListBatches()
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing trash batches failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.Trash = batches
	for _, b := range batches {
		data.TrashBytes += b.Bytes()
	}
	s.render(w, r, "trash.html", http.StatusOK, data)
}

// handleTrashRestore puts selected paths back. Restoring never overwrites, so a
// path that has been re-created since is reported rather than clobbered.
func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "could not read the form", http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")

	actor := removal.Actor{IP: httpx.ClientIPString(r.Context())}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		uid := u.ID
		actor.UserID, actor.Username = &uid, u.Username
	}

	restored, failures, err := s.trash.Restore(r.Context(), id, r.PostForm["path"], actor)
	var msg string
	switch {
	case err != nil:
		s.log.ErrorContext(r.Context(), "restore failed", "batch", id, "error", err)
		msg = "Restore failed: " + err.Error()
	case restored == 0 && len(failures) == 0:
		msg = "Nothing to restore in that batch."
	default:
		msg = fmt.Sprintf("Restored %d item(s).", restored)
		s.nav.invalidate()
		if len(failures) > 0 {
			msg += fmt.Sprintf(" %d could not be restored — see the batch for details.", len(failures))
		}
	}
	http.Redirect(w, r, "/trash?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// handleTrashPurge deletes trash batches older than the retention window,
// permanently.
//
// This is the only irreversible operation in Ambar. It runs when a person presses
// the button and never otherwise: §9.1 rules out "any automatic purging of trash,
// including under low-disk conditions". With no retention configured the button
// refuses rather than defaulting to "everything".
func (s *Server) handleTrashPurge(w http.ResponseWriter, r *http.Request) {
	if s.cfg.TrashRetention <= 0 {
		http.Redirect(w, r, "/trash?msg="+url.QueryEscape(
			"AMBAR_TRASH_RETENTION is not set, so there is no age at which purging is safe. "+
				"Set it, or purge by hand with `ambar trash purge --older-than`."), http.StatusSeeOther)
		return
	}

	actor := removal.Actor{IP: httpx.ClientIPString(r.Context())}
	if u, ok := auth.UserFromContext(r.Context()); ok {
		uid := u.ID
		actor.UserID, actor.Username = &uid, u.Username
	}

	cutoff := time.Now().Add(-s.cfg.TrashRetention)
	report, err := s.trash.Purge(r.Context(), cutoff, actor)
	if err != nil {
		s.log.ErrorContext(r.Context(), "purge failed", "error", err)
		http.Redirect(w, r, "/trash?msg="+url.QueryEscape("Purge failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	s.audit.Record(r.Context(), audit.Entry{
		UserID: actor.UserID, Action: audit.ActionTrashPurged, Entity: "trash",
		EntityID: "manual", IP: actor.IP,
		Detail: map[string]any{"batches": report.Batches, "bytes": report.Bytes, "kept": report.Kept},
	})

	msg := fmt.Sprintf("Purged %d batch(es), %s. %d batch(es) are younger than the retention window and were kept.",
		len(report.Batches), FormatBytes(report.Bytes), report.Kept)
	http.Redirect(w, r, "/trash?msg="+url.QueryEscape(msg), http.StatusSeeOther)
}

// linkSupport probes the configured dedupe link mode against the library
// filesystem (§9.1: "Probe support at startup and report it in the health
// endpoint").
func (s *Server) linkSupport() removal.LinkSupport {
	s.linkOnce.Do(func() {
		s.linkProbe = removal.ProbeLinkSupport(s.cfg.DedupeLinkMode, s.cfg.LibraryRoot)
	})
	return s.linkProbe
}

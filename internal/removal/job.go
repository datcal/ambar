package removal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/datcal/ambar/internal/jobs"
)

// JobType is the queue type for applying a plan. Invariant 8: a batch can be two
// hundred moves across a NAS share, and no HTTP handler waits for that.
//
// The queue is also what makes the operation observable: the /jobs page shows it
// running, and the trash record shows what it did.
const JobType = "maintenance.removal"

// JobPayload is a planned removal on its way to a worker. The plan is re-validated
// by the worker before anything moves — a payload is data at rest that a person
// could edit, and the safety rules belong to the code, not to the message.
type JobPayload struct {
	Plan  Plan  `json:"plan"`
	Actor Actor `json:"actor"`
}

// TransferFunc performs the §9.1 curation transfer: "If the user chooses to remove
// the subset, transfer its tags and provenance onto the superset first so nothing
// curated is lost". It is injected rather than imported so this package stays
// unaware of tags, provenance and duplicate detection — it moves files and nothing
// else.
type TransferFunc func(ctx context.Context, fromPackID, toPackID int64) (string, error)

// Runner applies plans from the queue.
type Runner struct {
	exec     *Executor
	planner  *Planner
	transfer TransferFunc
	log      *slog.Logger
}

// NewRunner builds a Runner. transfer may be nil, in which case a plan that asks
// for a curation transfer is refused rather than silently losing the curation.
func NewRunner(planner *Planner, exec *Executor, transfer TransferFunc, log *slog.Logger) *Runner {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Runner{exec: exec, planner: planner, transfer: transfer, log: log}
}

// Register attaches the handler to a queue.
func (r *Runner) Register(q *jobs.Queue) { q.Register(JobType, r.Handle) }

// Handle applies one plan.
//
// Order matters and is not negotiable: curation is transferred *before* any file
// moves. If the transfer fails, the job fails and nothing is removed — which is the
// whole point of doing it first.
func (r *Runner) Handle(ctx context.Context, raw []byte) error {
	var payload JobPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return fmt.Errorf("decode removal payload: %w", err)
	}
	if payload.Plan.Empty() {
		return fmt.Errorf("removal payload carries no operations")
	}

	// Re-plan from the targets the payload describes. Anything the Planner refuses
	// now is dropped, even if it was allowed when the preview was rendered: the
	// library may have changed in between, and a stale plan must never be able to
	// widen what gets removed.
	targets := make([]Target, 0, len(payload.Plan.Ops))
	for _, op := range payload.Plan.Ops {
		targets = append(targets, Target{
			Root: op.Root, Path: op.Path, Action: op.Action,
			KeepPath: op.KeepPath, Finding: op.Finding,
		})
	}
	plan, err := r.planner.Plan(ctx, payload.Plan.Reason, targets)
	if err != nil {
		return err
	}
	plan.Transfers = payload.Plan.Transfers
	for _, blocked := range plan.Blocked {
		r.log.WarnContext(ctx, "removal target refused at apply time",
			"path", blocked.Path, "reason", blocked.Reason)
	}
	if plan.Empty() {
		return fmt.Errorf("nothing is left to apply: every target was refused on re-check")
	}

	for _, t := range plan.Transfers {
		if r.transfer == nil {
			return fmt.Errorf("this plan needs a curation transfer (pack %d to pack %d) "+
				"but no transfer function is wired; refusing to remove and lose it", t.FromPackID, t.ToPackID)
		}
		summary, err := r.transfer(ctx, t.FromPackID, t.ToPackID)
		if err != nil {
			return fmt.Errorf("transfer curation from pack %d to pack %d: %w", t.FromPackID, t.ToPackID, err)
		}
		r.log.InfoContext(ctx, "transferred curation before removal",
			"from_pack", t.FromPackID, "to_pack", t.ToPackID, "summary", summary)
	}

	result, err := r.exec.Apply(ctx, plan, payload.Actor)
	if err != nil {
		return err
	}
	r.log.InfoContext(ctx, "removal batch applied",
		"batch", result.BatchID, "applied", result.Applied, "failed", result.Failed, "bytes", result.Bytes)
	if result.Applied == 0 {
		return fmt.Errorf("every operation in batch %s failed", result.BatchID)
	}
	return nil
}

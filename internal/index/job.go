package index

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/datcal/ambar/internal/jobs"
)

// ScanJobType is the queue type for a full library scan.
const ScanJobType = "library.scan"

// ScanPriority puts a scan above derivative work.
//
// A scan enqueues thousands of derive jobs, so without this a second user-triggered
// scan would sit behind every thumbnail the first one produced.
const ScanPriority = 100

// ScanJobPayload lets a queued scan carry the same options as the CLI.
type ScanJobPayload struct {
	DryRun         bool `json:"dry_run,omitempty"`
	ReadDimensions bool `json:"read_dimensions,omitempty"`
}

// RegisterScanJob wires scanning into the queue, which is what makes §12's "runnable
// from the UI" possible without violating invariant 8: the HTTP handler enqueues and
// returns, and the walk happens on a worker.
//
// onComplete runs after a successful non-dry-run scan. In practice it enqueues the
// derive jobs for whatever the scan found; passing it in rather than importing the
// derive package keeps this package free of that dependency.
func (ix *Indexer) RegisterScanJob(q *jobs.Queue, onComplete func(context.Context) error) {
	q.Register(ScanJobType, func(ctx context.Context, raw []byte) error {
		var p ScanJobPayload
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p); err != nil {
				return fmt.Errorf("unmarshal scan payload: %w", err)
			}
		}

		// A queued scan reports its progress; the CLI's does not, because there is nobody
		// polling a terminal. jobs.Reporter throttles the writes and is a no-op outside a job.
		report, err := ix.Scan(ctx, ScanOptions{
			DryRun:         p.DryRun,
			ReadDimensions: p.ReadDimensions,
			Progress:       q.Reporter(ctx),
		})
		if err != nil {
			return err
		}

		ix.log.InfoContext(ctx, "scan finished",
			"packs", report.PacksFound,
			"files", report.FilesSeen,
			"added", report.Added,
			"moved", report.Moved,
			"content_changed", report.ContentChanged,
			"marked_missing", report.MarkedMissing,
			"groups", report.Groups,
			"duration_ms", report.Duration.Milliseconds(),
		)

		// Per-file problems are reported but do not fail the job: one unreadable file
		// must not make a 20,000-file scan look like a failure.
		for _, e := range report.Errors {
			ix.log.WarnContext(ctx, "scan error", "error", e)
		}

		if p.DryRun || onComplete == nil {
			return nil
		}
		return onComplete(ctx)
	})
}

// EnqueueScan queues a library scan.
//
// The dedupe key is the job type itself, so only one scan can be pending or running at
// a time. Two people pressing the button does not walk the library twice.
func EnqueueScan(ctx context.Context, q *jobs.Queue, payload ScanJobPayload) (int64, error) {
	return q.Enqueue(ctx, ScanJobType, payload, jobs.EnqueueOptions{
		Priority:  ScanPriority,
		DedupeKey: ScanJobType,
	})
}

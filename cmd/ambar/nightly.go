package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
)

// The nightly scan (M16).
//
// Asked for in these words: "gece süper olur sabaha karşı 5 6 7 arası. sürekli arkada bir şey
// çalışmasın. ben basarım scan düğmesine scan lazımsa." So: one scan, once a day, in the small
// hours — and nothing else running in the background ever.
//
// Only a scan is enqueued, never a removal or a cleanup of any kind. Invariant 3 is that the
// application never deletes anything on its own, and a scheduled job is exactly the shape of
// thing that would quietly break it. A scan only reads the filesystem and writes index rows;
// the derives it triggers write to the derivative cache, which is rebuildable by definition.
//
// Deliberately not cron. One goroutine with a timer is the whole requirement, it needs no
// dependency, and "did it run last night" is answerable from the jobs table like everything
// else.
func runNightlyScan(ctx context.Context, queue *jobs.Queue, at time.Duration, log *slog.Logger) {
	if at < 0 {
		return // disabled
	}

	for {
		wait := untilNext(time.Now(), at)
		log.Info("nightly scan scheduled", "in", wait.Round(time.Minute), "at", clockOf(at))

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		// A scan that is already queued or running is left alone: ScanJobPayload's dedupe key
		// makes a second one a no-op, and the point of the schedule is that the library is
		// indexed by morning, not that a job row exists.
		id, err := index.EnqueueScan(ctx, queue, index.ScanJobPayload{ReadDimensions: true})
		switch {
		case errors.Is(err, jobs.ErrDuplicate):
			log.Info("nightly scan skipped: one is already pending")
		case err != nil:
			log.Error("nightly scan could not be enqueued", "error", err)
		default:
			log.Info("nightly scan enqueued", "job_id", id)
		}
	}
}

// untilNext is how long from now until the next occurrence of a time of day.
//
// Local time on purpose: "05:00" means five in the morning where the NAS is, which is where the
// person who wants the library scanned by breakfast also is. Computed by walking to tomorrow
// rather than by adding 24h, so a DST change shifts the run by an hour instead of drifting.
func untilNext(now time.Time, at time.Duration) time.Duration {
	target := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).Add(at)
	if !target.After(now) {
		target = time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, now.Location()).Add(at)
	}
	return target.Sub(now)
}

// clockOf renders a time-of-day offset as HH:MM, for the log line.
func clockOf(at time.Duration) string {
	h := int(at.Hours())
	m := int(at.Minutes()) % 60
	return time.Date(2000, 1, 1, h, m, 0, 0, time.UTC).Format("15:04")
}

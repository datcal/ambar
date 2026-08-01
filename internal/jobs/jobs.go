// Package jobs is the background work queue required by CLAUDE.md invariant 8:
// "No long-running HTTP handlers. All ingest, scan, and derivative work goes through
// the job queue with pollable status."
//
// It is deliberately a database table rather than an in-memory channel. Three
// properties come from that and matter more than the simplicity of a channel:
//
//   - Status is pollable, which is what makes §12's "runnable from the UI" possible
//     without a long-lived request.
//   - Failures survive the process. §12 wants job failures inspectable in the UI, not
//     only in container logs, because "a silently failing derivative pipeline is easy
//     to miss for weeks".
//   - Work survives a restart. A NAS reboots.
package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// Job states, matching the CHECK constraint in 0003_derive.sql.
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Tuning. These are constants rather than configuration because they are engineering
// details, not operator choices — AMBAR_WORKERS is the knob that matters.
const (
	// MaxAttempts before a job is parked as failed. Three is enough to ride out a
	// transient problem without hammering a genuinely broken input.
	MaxAttempts = 3

	// backoffBase is the first retry delay; it doubles per attempt.
	backoffBase = 10 * time.Second
	backoffMax  = 10 * time.Minute

	// pollInterval is the fallback for a worker that was not nudged. Enqueue nudges
	// directly, so this only covers retries becoming due and other processes writing
	// to the table.
	pollInterval = 2 * time.Second

	// maxErrorLen keeps one pathological error from bloating the row.
	maxErrorLen = 2000
)

// ErrDuplicate means an identical job is already queued or running. Callers that
// enqueue speculatively — the scan enqueuing a derive for every asset — treat this as
// success, because the work is already scheduled.
var ErrDuplicate = errors.New("an identical job is already pending")

// Handler runs one job. Returning an error schedules a retry, or parks the job as
// failed once attempts are exhausted.
//
// A handler must respect ctx: on shutdown it is cancelled, and the job is left
// running so the next startup requeues it.
type Handler func(ctx context.Context, payload []byte) error

// Job is one row, as the UI displays it.
type Job struct {
	ID         int64
	Type       string
	Payload    string
	State      string
	Attempts   int
	LastError  string
	Priority   int
	RunAfter   time.Time
	CreatedAt  time.Time
	StartedAt  *time.Time
	FinishedAt *time.Time

	// Progress is what a long job reports about itself (M16). Total is 0 when the job has
	// not said, which is every job type that finishes quickly enough not to need to.
	ProgressDone  int64
	ProgressTotal int64
	ProgressNote  string
}

// Percent is the completion percentage, or -1 when the job has not reported a total.
func (j Job) Percent() int {
	if j.ProgressTotal <= 0 {
		return -1
	}
	if j.ProgressDone >= j.ProgressTotal {
		return 100
	}
	return int(float64(j.ProgressDone) / float64(j.ProgressTotal) * 100)
}

// Duration is how long the job took, or how long it has been running.
func (j Job) Duration() time.Duration {
	if j.StartedAt == nil {
		return 0
	}
	if j.FinishedAt == nil {
		return time.Since(*j.StartedAt)
	}
	return j.FinishedAt.Sub(*j.StartedAt)
}

// Queue dispatches jobs to registered handlers.
type Queue struct {
	db      *db.DB
	log     *slog.Logger
	workers int
	now     func() time.Time

	mu       sync.RWMutex
	handlers map[string]Handler

	// nudge wakes an idle worker the moment something is enqueued, so the queue is
	// responsive without polling tightly.
	nudge chan struct{}
}

// Options configures a Queue.
type Options struct {
	// Workers is the concurrency. §12: "Worker concurrency configurable, defaulting
	// low. This is a NAS with a weak CPU running other services; derivative
	// generation must not starve them or freeze the UI."
	Workers int
	Log     *slog.Logger
}

func New(database *db.DB, opts Options) *Queue {
	workers := opts.Workers
	if workers < 1 {
		workers = 1
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Queue{
		db:       database,
		log:      log,
		workers:  workers,
		now:      time.Now,
		handlers: map[string]Handler{},
		nudge:    make(chan struct{}, 1),
	}
}

// WithClock replaces the clock, for tests.
func (q *Queue) WithClock(now func() time.Time) *Queue {
	q.now = now
	return q
}

// Register attaches a handler to a job type. Call before Run.
func (q *Queue) Register(jobType string, h Handler) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.handlers[jobType] = h
}

// EnqueueOptions are the per-job knobs.
type EnqueueOptions struct {
	// Priority: higher runs first. A scan the user just asked for should outrank the
	// thousands of derive jobs it will itself enqueue.
	Priority int
	// DedupeKey collapses identical work. While a job with this key is queued or
	// running, another Enqueue with the same key returns ErrDuplicate.
	DedupeKey string
	// Delay holds the job back, for scheduling rather than retrying.
	Delay time.Duration
}

// Enqueue adds a job. payload is marshalled to JSON.
func (q *Queue) Enqueue(ctx context.Context, jobType string, payload any, opts EnqueueOptions) (int64, error) {
	if jobType == "" {
		return 0, fmt.Errorf("enqueue: no job type given")
	}

	encoded := []byte("{}")
	if payload != nil {
		var err error
		if encoded, err = json.Marshal(payload); err != nil {
			return 0, fmt.Errorf("enqueue %s: marshal payload: %w", jobType, err)
		}
	}

	now := q.now()
	var dedupe any
	if opts.DedupeKey != "" {
		dedupe = opts.DedupeKey
	}

	var id int64
	err := q.db.Writer.QueryRowContext(ctx, `
		INSERT INTO jobs (type, payload_json, state, priority, run_after, dedupe_key,
		                  created_at, updated_at)
		VALUES (?, ?, 'queued', ?, ?, ?, ?, ?)
		RETURNING id`,
		jobType, string(encoded), opts.Priority, now.Add(opts.Delay).Unix(), dedupe,
		now.Unix(), now.Unix(),
	).Scan(&id)
	if err != nil {
		// The partial unique index on dedupe_key is what enforces idempotency, so
		// this is the expected path for already-scheduled work rather than a fault.
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, fmt.Errorf("%w: %s", ErrDuplicate, opts.DedupeKey)
		}
		return 0, fmt.Errorf("enqueue %s: %w", jobType, err)
	}

	q.Nudge()
	return id, nil
}

// Progress records how far a running job has got.
//
// Best-effort by design: a failed progress write must never fail the work it is describing, so
// the error is returned for logging and callers ignore it. The write is one UPDATE on the
// single writer connection, which is why callers are expected to throttle — see
// index.ScanOptions.Progress for the "every 250 files or half a second" rule.
func (q *Queue) Progress(ctx context.Context, jobID, done, total int64, note string) error {
	_, err := q.db.Writer.ExecContext(ctx, `
		UPDATE jobs SET progress_done = ?, progress_total = ?, progress_note = ?, updated_at = ?
		WHERE id = ? AND state = 'running'`,
		done, total, note, q.now().Unix(), jobID)
	if err != nil {
		return fmt.Errorf("record job progress: %w", err)
	}
	return nil
}

// Active lists the jobs that are running right now, with their progress. This is what the UI
// polls: a handful of rows, not the 200-row history the jobs page shows.
func (q *Queue) Active(ctx context.Context) ([]Job, error) {
	return q.Recent(ctx, StateRunning, 20)
}

// Nudge wakes a worker. Non-blocking: a full channel already means "there is work".
func (q *Queue) Nudge() {
	select {
	case q.nudge <- struct{}{}:
	default:
	}
}

// Run starts the workers and blocks until ctx is cancelled.
//
// Requeues anything left running by a previous process first — §12's "finish or
// requeue in-flight jobs". A row in 'running' at startup can only mean the process
// died mid-job, since a live worker always transitions it.
func (q *Queue) Run(ctx context.Context) error {
	if n, err := q.RequeueStale(ctx); err != nil {
		return err
	} else if n > 0 {
		q.log.InfoContext(ctx, "requeued jobs interrupted by a previous shutdown", "count", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < q.workers; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			q.work(ctx, worker)
		}(i)
	}

	q.log.InfoContext(ctx, "job workers started", "workers", q.workers)
	// A nudge at startup so pending work is picked up without waiting a tick.
	q.Nudge()

	wg.Wait()
	q.log.InfoContext(ctx, "job workers stopped")
	return nil
}

// RequeueStale returns jobs stuck in 'running' to the queue.
func (q *Queue) RequeueStale(ctx context.Context) (int64, error) {
	now := q.now().Unix()
	res, err := q.db.Writer.ExecContext(ctx, `
		UPDATE jobs
		SET state = 'queued', started_at = NULL, updated_at = ?,
		    last_error = 'interrupted by shutdown; requeued'
		WHERE state = 'running'`, now)
	if err != nil {
		return 0, fmt.Errorf("requeue interrupted jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // the update succeeded; the count is only for logging
	}
	return n, nil
}

// work is one worker's loop.
func (q *Queue) work(ctx context.Context, worker int) {
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		// Drain available work before going back to sleep, so a queue of ten
		// thousand derive jobs is not paced by the ticker.
		for {
			if ctx.Err() != nil {
				return
			}
			ran, err := q.runOne(ctx, worker)
			if err != nil {
				q.log.ErrorContext(ctx, "job queue error", "worker", worker, "error", err)
				break
			}
			if !ran {
				break
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-q.nudge:
			// Pass the nudge along: several workers may be idle and there may be
			// more than one job waiting.
			q.Nudge()
		case <-ticker.C:
		}
	}
}

// runOne claims and runs a single job, reporting whether one was found.
func (q *Queue) runOne(ctx context.Context, worker int) (bool, error) {
	job, ok, err := q.claim(ctx)
	if err != nil || !ok {
		return false, err
	}

	q.mu.RLock()
	handler, known := q.handlers[job.Type]
	q.mu.RUnlock()

	if !known {
		// An unregistered type is a programming error, not a transient one, so it is
		// parked immediately rather than retried three times.
		q.log.ErrorContext(ctx, "no handler registered for job type",
			"job_id", job.ID, "type", job.Type)
		return true, q.finishFailed(ctx, job.ID, fmt.Sprintf("no handler registered for type %q", job.Type))
	}

	start := q.now()
	q.log.DebugContext(ctx, "job started",
		"job_id", job.ID, "type", job.Type, "attempt", job.Attempts, "worker", worker)

	err = q.runHandler(ctx, handler, job)

	switch {
	case err == nil:
		q.log.InfoContext(ctx, "job done",
			"job_id", job.ID, "type", job.Type,
			"duration_ms", q.now().Sub(start).Milliseconds())
		return true, q.finishDone(ctx, job.ID)

	case ctx.Err() != nil:
		// Shutdown, not failure. Leave the row 'running' so the next startup
		// requeues it, and do not burn an attempt on it.
		q.log.InfoContext(ctx, "job interrupted by shutdown", "job_id", job.ID, "type", job.Type)
		return false, nil

	case job.Attempts >= MaxAttempts:
		q.log.ErrorContext(ctx, "job failed permanently",
			"job_id", job.ID, "type", job.Type, "attempts", job.Attempts, "error", err)
		return true, q.finishFailed(ctx, job.ID, err.Error())

	default:
		delay := backoff(job.Attempts)
		q.log.WarnContext(ctx, "job failed, will retry",
			"job_id", job.ID, "type", job.Type, "attempt", job.Attempts,
			"retry_in", delay, "error", err)
		return true, q.scheduleRetry(ctx, job.ID, err.Error(), delay)
	}
}

// runHandler isolates a panicking handler. §16 treats a recovered panic as a bug to
// fix rather than a handled case — but one bad image must not take down the worker
// pool and stall every other job.
func (q *Queue) runHandler(ctx context.Context, handler Handler, job Job) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			q.log.ErrorContext(ctx, "panic recovered in job handler",
				"job_id", job.ID, "type", job.Type, "panic", rec)
			err = fmt.Errorf("handler panicked: %v", rec)
		}
	}()
	// The job's identity travels in the context rather than in the handler signature, so a
	// handler that wants to report progress can ask for a reporter and every handler that
	// does not stays exactly as it was.
	return handler(withJobID(ctx, job.ID), []byte(job.Payload))
}

// jobIDKey carries the running job's id to its handler.
type jobIDKey struct{}

func withJobID(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, jobIDKey{}, id)
}

// JobIDFrom returns the id of the job whose handler is running, if any.
func JobIDFrom(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(jobIDKey{}).(int64)
	return id, ok
}

// progressInterval throttles progress writes. Every update is one write on the single writer
// connection, and a scan that reported every file would spend more time describing itself than
// walking — while starving the connection every other write shares.
const progressInterval = 400 * time.Millisecond

// Reporter returns a function a handler can call as often as it likes to report progress.
//
// Throttling lives here rather than in each caller: there is one rule, and every long job
// should get it for free. A completed report (done >= total) always writes, so the last number
// the UI sees is the real one.
//
// Outside a job — a CLI scan, a test — the returned function does nothing, which is what lets
// the same scan code run in both places.
func (q *Queue) Reporter(ctx context.Context) func(done, total int64, note string) {
	id, ok := JobIDFrom(ctx)
	if !ok {
		return func(int64, int64, string) {}
	}

	var (
		mu   sync.Mutex
		last time.Time
	)
	return func(done, total int64, note string) {
		mu.Lock()
		final := total > 0 && done >= total
		if !final && q.now().Sub(last) < progressInterval {
			mu.Unlock()
			return
		}
		last = q.now()
		mu.Unlock()

		if err := q.Progress(ctx, id, done, total, note); err != nil {
			q.log.DebugContext(ctx, "could not record job progress", "job_id", id, "error", err)
		}
	}
}

// claim atomically takes the next runnable job.
//
// One statement, which is what makes it safe: §4 routes every write through a single
// connection, so two workers cannot both claim the same row. No SKIP LOCKED, no
// advisory locks, no transaction to get wrong.
func (q *Queue) claim(ctx context.Context) (Job, bool, error) {
	now := q.now()

	var job Job
	err := q.db.Writer.QueryRowContext(ctx, `
		UPDATE jobs
		SET state = 'running', attempts = attempts + 1, started_at = ?, updated_at = ?
		WHERE id = (
		    SELECT id FROM jobs
		    WHERE state = 'queued' AND run_after <= ?
		    ORDER BY priority DESC, id
		    LIMIT 1
		)
		RETURNING id, type, payload_json, attempts`,
		now.Unix(), now.Unix(), now.Unix(),
	).Scan(&job.ID, &job.Type, &job.Payload, &job.Attempts)

	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, fmt.Errorf("claim job: %w", err)
	}
	job.State = StateRunning
	return job, true, nil
}

func (q *Queue) finishDone(ctx context.Context, id int64) error {
	// WithoutCancel: the handler has already succeeded, so recording that must not be
	// abandoned because a shutdown started a moment later. Without this, a job that
	// completed during shutdown stays in 'running', gets requeued at next startup and
	// runs a second time — harmless for an idempotent derive, but wrong, and the kind
	// of thing that becomes a real bug the first time a handler is not idempotent.
	ctx = context.WithoutCancel(ctx)

	now := q.now().Unix()
	// dedupe_key is cleared so the same work can be scheduled again later — after a
	// derive_version bump, for instance. The partial index only covers pending rows,
	// but clearing it also keeps the index small.
	if _, err := q.db.Writer.ExecContext(ctx, `
		UPDATE jobs SET state = 'done', finished_at = ?, updated_at = ?,
		                last_error = '', dedupe_key = NULL
		WHERE id = ?`, now, now, id); err != nil {
		return fmt.Errorf("mark job %d done: %w", id, err)
	}
	return nil
}

func (q *Queue) finishFailed(ctx context.Context, id int64, message string) error {
	// As in finishDone: the outcome is already decided, so it must be recorded.
	ctx = context.WithoutCancel(ctx)

	now := q.now().Unix()
	if _, err := q.db.Writer.ExecContext(ctx, `
		UPDATE jobs SET state = 'failed', finished_at = ?, updated_at = ?,
		                last_error = ?, dedupe_key = NULL
		WHERE id = ?`, now, now, truncate(message), id); err != nil {
		return fmt.Errorf("mark job %d failed: %w", id, err)
	}
	return nil
}

func (q *Queue) scheduleRetry(ctx context.Context, id int64, message string, delay time.Duration) error {
	ctx = context.WithoutCancel(ctx)

	now := q.now()
	if _, err := q.db.Writer.ExecContext(ctx, `
		UPDATE jobs SET state = 'queued', started_at = NULL, run_after = ?,
		                updated_at = ?, last_error = ?
		WHERE id = ?`, now.Add(delay).Unix(), now.Unix(), truncate(message), id); err != nil {
		return fmt.Errorf("schedule retry for job %d: %w", id, err)
	}
	return nil
}

// backoff is exponential in the attempt count, capped.
func backoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := time.Duration(math.Pow(2, float64(attempts-1))) * backoffBase
	if d > backoffMax || d <= 0 {
		return backoffMax
	}
	return d
}

func truncate(s string) string {
	if len(s) <= maxErrorLen {
		return s
	}
	return s[:maxErrorLen] + " …(truncated)"
}

// --- reporting --------------------------------------------------------------

// Stats is the queue summary the health endpoint and the jobs page show (§12).
type Stats struct {
	Queued  int
	Running int
	Done    int
	Failed  int
}

// Pending is the queue depth: work still to do.
func (s Stats) Pending() int { return s.Queued + s.Running }

// Runnable counts the jobs that could start right now — queued, with their run_after
// already passed.
//
// Distinct from Pending because a job serving out a retry backoff is pending but not
// runnable. A long-lived server does not care about the difference; a one-shot command
// like `ambar derive` very much does, since waiting out a ten-minute backoff for a file
// that will never decode is not useful work.
func (q *Queue) Runnable(ctx context.Context) (int, error) {
	var n int
	if err := q.db.Reader.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs WHERE state = 'queued' AND run_after <= ?`,
		q.now().Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count runnable jobs: %w", err)
	}
	return n, nil
}

// Backlogged counts queued jobs that are waiting on a retry backoff.
func (q *Queue) Backlogged(ctx context.Context) (int, error) {
	var n int
	if err := q.db.Reader.QueryRowContext(ctx, `
		SELECT count(*) FROM jobs WHERE state = 'queued' AND run_after > ?`,
		q.now().Unix()).Scan(&n); err != nil {
		return 0, fmt.Errorf("count backlogged jobs: %w", err)
	}
	return n, nil
}

func (q *Queue) Stats(ctx context.Context) (Stats, error) {
	rows, err := q.db.Reader.QueryContext(ctx, `SELECT state, count(*) FROM jobs GROUP BY state`)
	if err != nil {
		return Stats{}, fmt.Errorf("job stats: %w", err)
	}
	defer rows.Close()

	var s Stats
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return Stats{}, err
		}
		switch state {
		case StateQueued:
			s.Queued = n
		case StateRunning:
			s.Running = n
		case StateDone:
			s.Done = n
		case StateFailed:
			s.Failed = n
		}
	}
	return s, rows.Err()
}

// Recent returns the newest jobs, for the /jobs page.
func (q *Queue) Recent(ctx context.Context, state string, limit int) ([]Job, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	query := `
		SELECT id, type, payload_json, state, attempts, last_error, priority,
		       run_after, created_at, started_at, finished_at,
		       progress_done, progress_total, progress_note
		FROM jobs`
	var args []any
	if state != "" {
		query += ` WHERE state = ?`
		args = append(args, state)
	}
	// Unfinished work first — that is what someone opening this page is looking for —
	// then newest.
	query += ` ORDER BY (state IN ('queued','running')) DESC, created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := q.db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()

	var out []Job
	for rows.Next() {
		var (
			j                     Job
			runAfter, createdAt   int64
			startedAt, finishedAt sql.NullInt64
		)
		if err := rows.Scan(&j.ID, &j.Type, &j.Payload, &j.State, &j.Attempts,
			&j.LastError, &j.Priority, &runAfter, &createdAt, &startedAt, &finishedAt,
			&j.ProgressDone, &j.ProgressTotal, &j.ProgressNote); err != nil {
			return nil, err
		}
		j.RunAfter = time.Unix(runAfter, 0)
		j.CreatedAt = time.Unix(createdAt, 0)
		if startedAt.Valid {
			t := time.Unix(startedAt.Int64, 0)
			j.StartedAt = &t
		}
		if finishedAt.Valid {
			t := time.Unix(finishedAt.Int64, 0)
			j.FinishedAt = &t
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// RetryFailed requeues every failed job of the given type, or all of them when type
// is empty. This is §6's "retry failed derivatives" action.
//
// Attempts are reset, because a human asking for a retry is new information — usually
// that whatever was broken has been fixed.
func (q *Queue) RetryFailed(ctx context.Context, jobType string) (int64, error) {
	now := q.now().Unix()

	query := `
		UPDATE jobs
		SET state = 'queued', attempts = 0, run_after = ?, updated_at = ?,
		    started_at = NULL, finished_at = NULL
		WHERE state = 'failed'`
	args := []any{now, now}
	if jobType != "" {
		query += ` AND type = ?`
		args = append(args, jobType)
	}

	res, err := q.db.Writer.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("retry failed jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	if n > 0 {
		q.Nudge()
	}
	return n, nil
}

// Prune deletes finished job rows older than a cutoff.
//
// Only ever called from an explicit operator action, never on a timer: CLAUDE.md
// invariant 3 says the application never deletes anything on its own. These are the
// application's own bookkeeping rows rather than user data, but the habit is worth
// keeping — and a jobs table that quietly forgets its history is exactly what §12
// complains about.
func (q *Queue) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := q.db.Writer.ExecContext(ctx,
		`DELETE FROM jobs WHERE state IN ('done','failed') AND finished_at < ?`,
		olderThan.Unix())
	if err != nil {
		return 0, fmt.Errorf("prune jobs: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// Package remote lets another machine do the derive work: a worker on a desktop with a
// fast CPU and its own mount of the library takes asset.derive jobs from the server over
// HTTP, decodes and renders locally, and sends the results back. The server keeps the
// database, the queue and every write; the worker never opens SQLite.
//
// This is a deliberate exception to spec §0's "no agent/server split". The reason is
// measured, not speculative: the target NAS is an Intel N100, a 92,000-file import queued
// derive work it chews through at a few seconds an image, and the desktop beside it has
// twenty threads and Blender. What is kept from §0 is what it protects: one database with
// one writer, and a server that works on its own — with no worker connected, or with the
// mode off, nothing changes.
//
// The worker cannot share the database by mounting the data directory instead: SQLite's
// WAL does not survive a network filesystem, which docker-compose.yml already warns about.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/jobs"
)

// Modes for AMBAR_REMOTE_DERIVE.
const (
	// ModeOff refuses workers; the server derives everything itself, as before.
	ModeOff = "off"
	// ModeAuto lets the server's own workers step aside from derive jobs while a remote
	// worker is connected, and take them back when it goes quiet.
	ModeAuto = "auto"
	// ModeOnly leaves derive jobs to remote workers entirely; they wait for one.
	ModeOnly = "only"
)

// ValidMode reports whether m is one of the modes.
func ValidMode(m string) bool { return m == ModeOff || m == ModeAuto || m == ModeOnly }

const (
	// LeaseDuration is how long a worker holds a job before the server takes it back.
	// Longer than the worst case for one job — a three-minute Blender conversion and the
	// upload of its result — so a slow job is not handed out twice.
	LeaseDuration = 15 * time.Minute
	// ActiveWindow is how recently a worker must have asked for work to count as
	// connected. A worker with nothing to do asks every few seconds, so this only lapses
	// when it has stopped.
	ActiveWindow = 90 * time.Second
	// maxSkipsPerClaim bounds the jobs one claim request settles without handing out —
	// rows deleted since they were queued, or already derived — so the request stays short.
	maxSkipsPerClaim = 64
)

// Errors a handler maps to HTTP statuses.
var (
	ErrDisabled        = errors.New("remote derive workers are turned off on this server (AMBAR_REMOTE_DERIVE=off)")
	ErrVersionMismatch = errors.New("the worker's derive version does not match the server's")
	ErrNoLease         = errors.New("this job is not leased to this worker; it was taken back or never handed out")
)

// ClaimRequest is what a worker sends when it wants work.
type ClaimRequest struct {
	// Worker names the machine, for the server's logs.
	Worker string `json:"worker"`
	// Session is random per worker process, so a result can only be submitted by the
	// process that was given the job.
	Session string `json:"session"`
	// DeriveVersion must equal the server's derive.Version.
	DeriveVersion int `json:"derive_version"`
	// Blender says whether this worker can convert FBX and .blend.
	Blender bool `json:"blender"`
}

// ClaimResponse is one job handed out.
type ClaimResponse struct {
	JobID        int64       `json:"job_id"`
	LeaseExpires time.Time   `json:"lease_expires"`
	Task         derive.Task `json:"task"`
}

// ReleaseRequest hands jobs back, from a worker that is stopping.
type ReleaseRequest struct {
	Session string  `json:"session"`
	JobIDs  []int64 `json:"job_ids"`
}

// lease is one job out on a worker.
type lease struct {
	job     jobs.Job
	task    derive.Task
	session string
	worker  string
	since   time.Time
	expires time.Time
}

// Coordinator is the server's side: it hands out derive jobs, takes back the ones a
// worker abandons, and records what comes back.
type Coordinator struct {
	db      *db.DB
	queue   *jobs.Queue
	deriver *derive.Deriver
	mode    string
	log     *slog.Logger
	now     func() time.Time

	mu       sync.Mutex
	leases   map[int64]*lease
	lastSeen time.Time
	worker   string
	// connected is what was last announced in the log, so a connection and its end are
	// each logged once.
	connected bool
	// blenderOffered records that the needs_blender backlog was queued for the worker
	// connected now; it resets when the worker goes quiet, so a reconnect offers again.
	blenderOffered bool
}

// Options configures a Coordinator.
type Options struct {
	Mode string
	Log  *slog.Logger
}

// NewCoordinator builds the server side. The deriver must be the one registered on the
// queue, so remote and local results are recorded by the same code.
func NewCoordinator(database *db.DB, queue *jobs.Queue, deriver *derive.Deriver, opts Options) *Coordinator {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mode := opts.Mode
	if !ValidMode(mode) {
		mode = ModeOff
	}
	return &Coordinator{
		db:      database,
		queue:   queue,
		deriver: deriver,
		mode:    mode,
		log:     log,
		now:     time.Now,
		leases:  map[int64]*lease{},
	}
}

// WithClock replaces the clock, for tests.
func (c *Coordinator) WithClock(now func() time.Time) *Coordinator {
	c.now = now
	return c
}

// Mode is the configured mode.
func (c *Coordinator) Mode() string { return c.mode }

// SkipLocal is jobs.Options.Skip for the server's own queue: the job types its workers
// should leave to remote workers right now.
func (c *Coordinator) SkipLocal() []string {
	switch c.mode {
	case ModeOnly:
		return []string{derive.JobType}
	case ModeAuto:
		if c.Active() {
			return []string{derive.JobType}
		}
	}
	return nil
}

// Active reports whether a worker has asked for work recently.
func (c *Coordinator) Active() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeLocked()
}

func (c *Coordinator) activeLocked() bool {
	return !c.lastSeen.IsZero() && c.now().Sub(c.lastSeen) < ActiveWindow
}

// Claim hands out the next derive job, or nil when there is none.
func (c *Coordinator) Claim(ctx context.Context, req ClaimRequest) (*ClaimResponse, error) {
	if c.mode == ModeOff {
		return nil, ErrDisabled
	}
	if req.DeriveVersion != derive.Version {
		return nil, fmt.Errorf("%w: worker %d, server %d — rebuild the worker from the same version",
			ErrVersionMismatch, req.DeriveVersion, derive.Version)
	}
	if req.Session == "" {
		return nil, errors.New("a claim needs a session")
	}
	c.touch(ctx, req)

	for i := 0; i < maxSkipsPerClaim; i++ {
		job, ok, err := c.queue.ClaimTypes(ctx, []string{derive.JobType})
		if err != nil || !ok {
			return nil, err
		}

		var payload derive.Payload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			_ = c.queue.Settle(ctx, job, fmt.Errorf("unmarshal derive payload: %w", err))
			continue
		}
		task, done, err := c.deriver.Prepare(ctx, payload.AssetID)
		if err != nil {
			_ = c.queue.Settle(ctx, job, err)
			continue
		}
		if done {
			// Nothing to decode — the asset is gone or current, or its outcome was
			// recorded here. Finished, exactly as a local worker would finish it.
			if err := c.queue.Settle(ctx, job, nil); err != nil {
				return nil, err
			}
			continue
		}

		now := c.now()
		l := &lease{job: job, task: task, session: req.Session, worker: req.Worker,
			since: now, expires: now.Add(LeaseDuration)}
		c.mu.Lock()
		c.leases[job.ID] = l
		c.mu.Unlock()
		return &ClaimResponse{JobID: job.ID, LeaseExpires: l.expires, Task: task}, nil
	}
	return nil, nil
}

// touch records a worker asking for work, announces a new connection, and offers the
// needs_blender backlog to a worker that has Blender.
func (c *Coordinator) touch(ctx context.Context, req ClaimRequest) {
	c.mu.Lock()
	announce := !c.connected
	c.connected = true
	c.lastSeen = c.now()
	c.worker = req.Worker
	offer := req.Blender && !c.blenderOffered
	if offer {
		c.blenderOffered = true
	}
	c.mu.Unlock()

	if announce {
		c.log.InfoContext(ctx, "remote derive worker connected",
			"worker", req.Worker, "blender", req.Blender, "mode", c.mode)
	}
	if offer {
		// Tens of thousands of enqueues on a library that has them, so not on the
		// request: the worker will find the jobs on its next claims.
		go func() {
			n, err := derive.EnqueueNeedsBlender(context.WithoutCancel(ctx), c.db, c.queue)
			if err != nil {
				c.log.Warn("could not queue the assets waiting on Blender", "error", err)
				return
			}
			if n > 0 {
				c.log.Info("queued assets waiting on Blender for the remote worker", "count", n)
			}
		}()
	}
}

// Submit records the result of a leased job. staging holds the uploaded derivative
// files; the caller removes it afterwards.
func (c *Coordinator) Submit(ctx context.Context, jobID int64, session string, outcome derive.Outcome, staging string) error {
	c.mu.Lock()
	l, ok := c.leases[jobID]
	if ok && l.session == session {
		delete(c.leases, jobID)
	}
	c.mu.Unlock()
	if !ok || l.session != session {
		return ErrNoLease
	}

	acceptErr := c.deriver.Accept(ctx, l.task, outcome, staging)
	if err := c.queue.Settle(ctx, l.job, acceptErr); err != nil {
		return err
	}
	if acceptErr != nil && outcome.State == derive.StateOK {
		c.log.WarnContext(ctx, "refused a remote worker's result",
			"job_id", jobID, "worker", l.worker, "error", acceptErr)
	}
	if acceptErr == nil {
		c.log.InfoContext(ctx, "remote job done",
			"job_id", jobID, "worker", l.worker, "state", outcome.State,
			"duration_ms", c.now().Sub(l.since).Milliseconds())
	}
	// A failed derive is the worker reporting honestly, already settled as a retry; the
	// worker has nothing to do differently, so it is not an error to the request.
	if acceptErr != nil && outcome.State == derive.StateOK {
		return acceptErr
	}
	return nil
}

// Release hands a stopping worker's jobs back to the queue without costing them an
// attempt.
func (c *Coordinator) Release(ctx context.Context, req ReleaseRequest) {
	for _, id := range req.JobIDs {
		c.mu.Lock()
		l, ok := c.leases[id]
		if ok && l.session == req.Session {
			delete(c.leases, id)
		}
		c.mu.Unlock()
		if !ok || l.session != req.Session {
			continue
		}
		if err := c.queue.Requeue(ctx, l.job, "released by remote worker "+l.worker, true); err != nil {
			c.log.WarnContext(ctx, "could not requeue a released job", "job_id", id, "error", err)
		}
	}
}

// Run takes back expired leases and notices a worker going quiet, until ctx ends.
func (c *Coordinator) Run(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.expire(ctx)
		}
	}
}

// expire requeues every lease past its deadline, keeping the attempt it cost: a worker
// that vanishes in the middle of the same file twice should run that file out of tries.
func (c *Coordinator) expire(ctx context.Context) {
	now := c.now()
	var expired []*lease

	c.mu.Lock()
	for id, l := range c.leases {
		if now.After(l.expires) {
			expired = append(expired, l)
			delete(c.leases, id)
		}
	}
	quiet := c.connected && !c.activeLocked()
	if quiet {
		c.connected = false
		c.blenderOffered = false
	}
	worker := c.worker
	c.mu.Unlock()

	for _, l := range expired {
		if err := c.queue.Requeue(ctx, l.job, "remote worker "+l.worker+" did not finish in time", false); err != nil {
			c.log.WarnContext(ctx, "could not requeue an expired job", "job_id", l.job.ID, "error", err)
		}
	}
	if len(expired) > 0 {
		c.log.WarnContext(ctx, "took back jobs from a remote worker that stopped answering", "count", len(expired))
	}
	if quiet {
		c.log.InfoContext(ctx, "remote derive worker went quiet", "worker", worker, "mode", c.mode)
	}
}

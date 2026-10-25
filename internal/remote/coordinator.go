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
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/jobs"
)

// Where background work runs: the jobs page's choice, with AMBAR_REMOTE_DERIVE as the
// default until someone makes one.
const (
	// ModeOff is "this server": it runs everything itself and refuses remote workers.
	ModeOff = "off"
	// ModeAuto: this server runs everything, except that it leaves derive jobs to a remote
	// worker while one is connected and takes them back when it goes quiet.
	ModeAuto = "auto"
	// ModeOnly is "remote workers": this server's own workers stop entirely. Derive jobs
	// go to remote workers; everything else — scans, sweeps — waits until the choice
	// changes back. For a NAS that should be quiet while a desktop does the work.
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
	// Concurrency is how many jobs the worker runs at once, for the jobs page.
	Concurrency int `json:"concurrency,omitempty"`
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

// WorkerStatus is one worker as the jobs page shows it.
type WorkerStatus struct {
	Name        string    `json:"name"`
	Connected   bool      `json:"connected"`
	Blender     bool      `json:"blender"`
	Concurrency int       `json:"concurrency,omitempty"`
	Since       time.Time `json:"since"`
	LastSeen    time.Time `json:"last_seen"`
	// Outcomes recorded for this worker since it connected.
	Done         int `json:"done"`
	OK           int `json:"ok"`
	Unsupported  int `json:"unsupported"`
	NeedsBlender int `json:"needs_blender"`
	Failed       int `json:"failed"`
	// PerMinute is how many results arrived in the last sixty seconds.
	PerMinute int         `json:"per_minute"`
	Current   []WorkerJob `json:"current"`
}

// WorkerJob is one job a worker holds right now.
type WorkerJob struct {
	JobID int64     `json:"job_id"`
	File  string    `json:"file"`
	Since time.Time `json:"since"`
}

// workerState is what the server remembers about one worker process, by session.
type workerState struct {
	name        string
	blender     bool
	concurrency int
	since       time.Time
	lastSeen    time.Time
	outcomes    map[string]int
	// recent holds the arrival time of each result in the last minute.
	recent []time.Time
}

// forgetAfter is how long a worker that has stopped stays on the jobs page, so "it was
// here, it stopped an hour ago" is answerable.
const forgetAfter = time.Hour

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
	db        *db.DB
	queue     *jobs.Queue
	deriver   *derive.Deriver
	stateFile string
	log       *slog.Logger
	now       func() time.Time

	mu sync.Mutex
	// mode is the current choice; see SetMode.
	mode     string
	leases   map[int64]*lease
	workers  map[string]*workerState
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
	// Mode is the default: AMBAR_REMOTE_DERIVE, used until the jobs page records a choice.
	Mode string
	// StateFile remembers the jobs page's choice across restarts. Empty keeps it in memory.
	StateFile string
	Log       *slog.Logger
}

// NewCoordinator builds the server side. The deriver must be the one registered on the
// queue, so remote and local results are recorded by the same code.
func NewCoordinator(database *db.DB, queue *jobs.Queue, deriver *derive.Deriver, opts Options) *Coordinator {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	mode := opts.Mode
	if opts.StateFile != "" {
		if raw, err := os.ReadFile(opts.StateFile); err == nil {
			mode = strings.TrimSpace(string(raw))
		}
	}
	if !ValidMode(mode) {
		mode = ModeOff
	}
	c := &Coordinator{
		db:        database,
		queue:     queue,
		deriver:   deriver,
		stateFile: opts.StateFile,
		mode:      mode,
		log:       log,
		now:       time.Now,
		leases:    map[int64]*lease{},
		workers:   map[string]*workerState{},
	}
	queue.SetPaused(mode == ModeOnly)
	return c
}

// SetMode changes where background work runs, from the jobs page, and remembers it.
func (c *Coordinator) SetMode(mode string) error {
	if !ValidMode(mode) {
		return fmt.Errorf("unknown mode %q", mode)
	}
	if c.stateFile != "" {
		tmp := c.stateFile + ".tmp"
		if err := os.WriteFile(tmp, []byte(mode+"\n"), 0o644); err != nil {
			return fmt.Errorf("remember the choice: %w", err)
		}
		if err := os.Rename(tmp, c.stateFile); err != nil {
			return fmt.Errorf("remember the choice: %w", err)
		}
	}
	c.mu.Lock()
	c.mode = mode
	c.mu.Unlock()
	c.queue.SetPaused(mode == ModeOnly)
	return nil
}

// WithClock replaces the clock, for tests.
func (c *Coordinator) WithClock(now func() time.Time) *Coordinator {
	c.now = now
	return c
}

// Mode is the current choice.
func (c *Coordinator) Mode() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.mode
}

// SkipLocal is jobs.Options.Skip for the server's own queue: the job types its workers
// should leave to remote workers right now.
func (c *Coordinator) SkipLocal() []string {
	switch c.Mode() {
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
	if c.Mode() == ModeOff {
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
	ws := c.workers[req.Session]
	if ws == nil {
		ws = &workerState{since: c.lastSeen, outcomes: map[string]int{}}
		c.workers[req.Session] = ws
	}
	ws.name, ws.blender, ws.concurrency, ws.lastSeen = req.Worker, req.Blender, req.Concurrency, c.lastSeen
	offer := req.Blender && !c.blenderOffered
	if offer {
		c.blenderOffered = true
	}
	c.mu.Unlock()

	if announce {
		c.log.InfoContext(ctx, "remote derive worker connected",
			"worker", req.Worker, "blender", req.Blender, "mode", c.Mode())
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
	state := outcome.State
	if acceptErr != nil {
		state = derive.StateFailed
	}
	c.recordOutcome(session, state)
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

// recordOutcome counts one result against the worker that sent it.
func (c *Coordinator) recordOutcome(session, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.workers[session]
	if ws == nil {
		return
	}
	now := c.now()
	ws.outcomes[state]++
	ws.recent = append(trimBefore(ws.recent, now.Add(-time.Minute)), now)
}

// trimBefore drops the times older than cutoff from a slice kept in arrival order.
func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	return times[i:]
}

// Workers reports every worker seen in the last hour, longest-connected first.
func (c *Coordinator) Workers() []WorkerStatus {
	c.mu.Lock()
	defer c.mu.Unlock()

	now := c.now()
	current := map[string][]WorkerJob{}
	for _, l := range c.leases {
		current[l.session] = append(current[l.session],
			WorkerJob{JobID: l.job.ID, File: l.task.LibraryPath, Since: l.since})
	}

	out := make([]WorkerStatus, 0, len(c.workers))
	for session, ws := range c.workers {
		if now.Sub(ws.lastSeen) > forgetAfter {
			delete(c.workers, session)
			continue
		}
		ws.recent = trimBefore(ws.recent, now.Add(-time.Minute))
		jobs := current[session]
		sort.Slice(jobs, func(i, j int) bool { return jobs[i].Since.Before(jobs[j].Since) })
		st := WorkerStatus{
			Name:         ws.name,
			Connected:    now.Sub(ws.lastSeen) < ActiveWindow,
			Blender:      ws.blender,
			Concurrency:  ws.concurrency,
			Since:        ws.since,
			LastSeen:     ws.lastSeen,
			OK:           ws.outcomes[derive.StateOK],
			Unsupported:  ws.outcomes[derive.StateUnsupported],
			NeedsBlender: ws.outcomes[derive.StateNeedsBlender],
			Failed:       ws.outcomes[derive.StateFailed],
			PerMinute:    len(ws.recent),
			Current:      jobs,
		}
		st.Done = st.OK + st.Unsupported + st.NeedsBlender + st.Failed
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out
}

// Holders maps each job out on a worker to that worker's name, so a list of running
// jobs can say where each one is running.
func (c *Coordinator) Holders() map[int64]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[int64]string, len(c.leases))
	for id, l := range c.leases {
		out[id] = l.worker
	}
	return out
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
	mode := c.mode
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
		c.log.InfoContext(ctx, "remote derive worker went quiet", "worker", worker, "mode", mode)
	}
}

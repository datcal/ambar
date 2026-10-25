package remote

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/derive"
)

// The worker's side: ask for a job, derive it against this machine's mount of the
// library, send the result back, repeat.

// Paths of the worker API, relative to the server's base URL.
const (
	ClaimPath   = "/api/v1/worker/claim"
	ReleasePath = "/api/v1/worker/release"
	// ResultPath is formatted with the job id.
	ResultPath = "/api/v1/worker/jobs/%d/result"
)

// WorkerOptions configures a worker.
type WorkerOptions struct {
	// Server is the ambar server's base URL, e.g. http://nas.local:8973.
	Server string
	// Token is an API token with the write scope.
	Token string
	// LibraryRoot is this machine's mount of the same library the server indexes.
	LibraryRoot string
	// WorkDir holds one scratch directory per job while it runs. It must be somewhere
	// Blender can read and write, which rules out /tmp for a sandboxed (flatpak) Blender.
	WorkDir string
	// BlenderBin converts FBX and .blend; empty leaves them needs_blender, as on a server
	// without Blender.
	BlenderBin string
	// Concurrency is how many jobs run at once.
	Concurrency int
	// Name identifies this machine in the server's logs.
	Name string
	Log  *slog.Logger
	// Client defaults to one with a generous timeout: a result upload can be a large GLB.
	Client *http.Client
	// IdleWait is how long to wait before asking again when there was no work.
	IdleWait time.Duration
}

// errFatal marks an error no retry will fix: a version mismatch or a rejected token.
type errFatal struct{ error }

// errNotAccepting is a server whose jobs page says background work runs on the server
// itself. Not fatal: the choice can change at any moment, and the worker should pick up
// work again when it does without anyone restarting it.
var errNotAccepting = errors.New("the server is not taking remote workers right now")

func (e errFatal) Unwrap() error { return e.error }

// RunWorker runs until ctx ends or the server refuses the worker outright.
func RunWorker(ctx context.Context, opts WorkerOptions) error {
	w, err := newWorker(opts)
	if err != nil {
		return err
	}
	return w.run(ctx)
}

type worker struct {
	opts    WorkerOptions
	session string
	blender bool
	log     *slog.Logger

	mu       sync.Mutex
	inFlight map[int64]bool
	counts   map[string]int
	// waiting is whether "the server is not taking workers" has been logged, so eight
	// slots polling a switched-off server say so once rather than every half minute.
	waiting bool
}

func newWorker(opts WorkerOptions) (*worker, error) {
	opts.Server = strings.TrimRight(opts.Server, "/")
	if opts.Server == "" {
		return nil, errors.New("no server URL given")
	}
	if opts.Token == "" {
		return nil, errors.New("no API token given; create one with `ambar token add <user> --scope write`")
	}
	if info, err := os.Stat(opts.LibraryRoot); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("the library %q is not a readable directory on this machine", opts.LibraryRoot)
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, fmt.Errorf("create the work directory: %w", err)
	}
	if opts.Concurrency < 1 {
		opts.Concurrency = 1
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 10 * time.Minute}
	}
	if opts.IdleWait <= 0 {
		opts.IdleWait = 5 * time.Second
	}
	if opts.Name == "" {
		opts.Name, _ = os.Hostname()
	}

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, err
	}
	blender := false
	if opts.BlenderBin != "" {
		if info, err := os.Stat(opts.BlenderBin); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			blender = true
		} else {
			return nil, fmt.Errorf("blender %q is not an executable file", opts.BlenderBin)
		}
	}
	return &worker{
		opts:     opts,
		session:  hex.EncodeToString(raw),
		blender:  blender,
		log:      opts.Log,
		inFlight: map[int64]bool{},
		counts:   map[string]int{},
	}, nil
}

func (w *worker) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	w.log.Info("remote derive worker starting",
		"server", w.opts.Server, "library", w.opts.LibraryRoot, "concurrency", w.opts.Concurrency,
		"blender", w.blender, "derive_version", derive.Version)

	fatal := make(chan error, w.opts.Concurrency)
	var wg sync.WaitGroup
	for i := 0; i < w.opts.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := w.loop(ctx); err != nil {
				fatal <- err
				cancel()
			}
		}()
	}
	go w.report(ctx)

	var err error
	select {
	case <-ctx.Done():
	case err = <-fatal:
	}
	cancel()

	// Hand back whatever is still running, so the server does not wait out the lease.
	w.release()

	// A job in the middle of a Blender conversion can take minutes; its result would be
	// refused now anyway, so the wait is short.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
	w.log.Info("remote derive worker stopped")
	return err
}

// loop is one slot: claim, derive, submit, until ctx ends.
func (w *worker) loop(ctx context.Context) error {
	for ctx.Err() == nil {
		claim, err := w.claim(ctx)
		var fe errFatal
		switch {
		case errors.As(err, &fe):
			return fe.error
		case errors.Is(err, errNotAccepting):
			w.setWaiting(true)
			sleep(ctx, 30*time.Second)
			continue
		case err != nil:
			if ctx.Err() == nil {
				w.log.Warn("could not reach the server; retrying", "error", err)
			}
			sleep(ctx, 10*time.Second)
			continue
		case claim == nil:
			w.setWaiting(false)
			sleep(ctx, w.opts.IdleWait)
			continue
		}
		w.setWaiting(false)
		w.process(ctx, claim)
	}
	return nil
}

func (w *worker) process(ctx context.Context, claim *ClaimResponse) {
	w.track(claim.JobID, true)
	defer w.track(claim.JobID, false)

	scratch, err := os.MkdirTemp(w.opts.WorkDir, "job-")
	if err != nil {
		w.log.Error("could not create a scratch directory", "error", err)
		return
	}
	defer os.RemoveAll(scratch) //nolint:errcheck

	start := time.Now()
	outcome := derive.RunTask(claim.Task, w.opts.LibraryRoot, scratch, w.opts.BlenderBin)
	if ctx.Err() != nil {
		return // released by run
	}

	for attempt := 1; ; attempt++ {
		err = w.submit(ctx, claim, outcome, scratch)
		if err == nil || errors.Is(err, ErrNoLease) || attempt == 3 || ctx.Err() != nil {
			break
		}
		w.log.Warn("could not send a result; retrying", "job_id", claim.JobID, "error", err)
		sleep(ctx, 5*time.Second)
	}
	switch {
	case errors.Is(err, ErrNoLease):
		w.log.Warn("the server took this job back before it finished", "job_id", claim.JobID,
			"file", claim.Task.LibraryPath)
	case err != nil:
		w.log.Error("giving up on a result; the server will hand the job out again",
			"job_id", claim.JobID, "error", err)
	default:
		w.count(outcome.State)
		if outcome.State == derive.StateFailed {
			w.log.Warn("derive failed", "file", claim.Task.LibraryPath, "error", outcome.Message)
		}
		w.log.Debug("job done", "job_id", claim.JobID, "file", claim.Task.LibraryPath,
			"state", outcome.State, "duration_ms", time.Since(start).Milliseconds())
	}
}

func (w *worker) claim(ctx context.Context) (*ClaimResponse, error) {
	body, _ := json.Marshal(ClaimRequest{
		Worker: w.opts.Name, Session: w.session, DeriveVersion: derive.Version, Blender: w.blender,
		Concurrency: w.opts.Concurrency,
	})
	resp, err := w.do(ctx, http.MethodPost, ClaimPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusOK:
		var claim ClaimResponse
		if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
			return nil, fmt.Errorf("decode claim: %w", err)
		}
		return &claim, nil
	case http.StatusNoContent:
		return nil, nil
	case http.StatusServiceUnavailable:
		return nil, errNotAccepting
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict:
		return nil, errFatal{fmt.Errorf("the server refused this worker: %s", errorBody(resp))}
	default:
		return nil, fmt.Errorf("claim: %s", errorBody(resp))
	}
}

// submit uploads the outcome and its files as one multipart request, streamed so a large
// GLB is never held in memory.
func (w *worker) submit(ctx context.Context, claim *ClaimResponse, outcome derive.Outcome, scratch string) error {
	encoded, err := json.Marshal(outcome)
	if err != nil {
		// Something in the result has no JSON form. Say so rather than drop the job.
		outcome = derive.Outcome{State: derive.StateFailed,
			Message: fmt.Sprintf("the worker could not encode the result: %v", err)}
		encoded, _ = json.Marshal(outcome)
	}

	var files []string
	if outcome.State == derive.StateOK && outcome.Result != nil {
		relDir, err := derive.Dir(claim.Task.SHA256)
		if err != nil {
			return err
		}
		for _, name := range outcome.Result.Files {
			files = append(files, filepath.Join(scratch, relDir, name))
		}
	}

	pr, pw := io.Pipe()
	form := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			if err := form.WriteField("session", w.session); err != nil {
				return err
			}
			if err := form.WriteField("outcome", string(encoded)); err != nil {
				return err
			}
			for _, path := range files {
				part, err := form.CreateFormFile("file", filepath.Base(path))
				if err != nil {
					return err
				}
				f, err := os.Open(path)
				if err != nil {
					return err
				}
				_, err = io.Copy(part, f)
				f.Close() //nolint:errcheck
				if err != nil {
					return err
				}
			}
			return form.Close()
		}()
		pw.CloseWithError(err) //nolint:errcheck
	}()

	resp, err := w.do(ctx, http.MethodPost, fmt.Sprintf(ResultPath, claim.JobID), form.FormDataContentType(), pr)
	if err != nil {
		pr.CloseWithError(err) //nolint:errcheck
		return err
	}
	defer resp.Body.Close() //nolint:errcheck

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrNoLease
	default:
		return fmt.Errorf("result: %s", errorBody(resp))
	}
}

// release hands every job still running back to the server.
func (w *worker) release() {
	w.mu.Lock()
	ids := make([]int64, 0, len(w.inFlight))
	for id := range w.inFlight {
		ids = append(ids, id)
	}
	w.mu.Unlock()
	if len(ids) == 0 {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(ReleaseRequest{Session: w.session, JobIDs: ids})
	resp, err := w.do(ctx, http.MethodPost, ReleasePath, "application/json", bytes.NewReader(body))
	if err != nil {
		w.log.Warn("could not hand jobs back; the server will take them back when their lease ends",
			"jobs", len(ids), "error", err)
		return
	}
	resp.Body.Close() //nolint:errcheck
	w.log.Info("handed unfinished jobs back to the server", "jobs", len(ids))
}

func (w *worker) do(ctx context.Context, method, path, contentType string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.opts.Server+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+w.opts.Token)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	return w.opts.Client.Do(req)
}

// setWaiting logs the moment the server stops or starts taking remote workers.
func (w *worker) setWaiting(waiting bool) {
	w.mu.Lock()
	changed := w.waiting != waiting
	w.waiting = waiting
	w.mu.Unlock()
	if !changed {
		return
	}
	if waiting {
		w.log.Info("the server runs background work itself right now; waiting until its jobs page hands work out again")
	} else {
		w.log.Info("the server is handing out work again")
	}
}

func (w *worker) track(id int64, running bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if running {
		w.inFlight[id] = true
	} else {
		delete(w.inFlight, id)
	}
}

func (w *worker) count(state string) {
	w.mu.Lock()
	w.counts[state]++
	w.mu.Unlock()
}

// report logs a line a minute while there is work, rather than one per job: a backlog is
// tens of thousands of jobs.
func (w *worker) report(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		w.mu.Lock()
		counts := w.counts
		w.counts = map[string]int{}
		running := len(w.inFlight)
		w.mu.Unlock()

		total := 0
		attrs := []any{"running", running}
		for state, n := range counts {
			total += n
			attrs = append(attrs, state, n)
		}
		if total > 0 {
			w.log.Info("derived in the last minute", append([]any{"jobs", total}, attrs...)...)
		}
	}
}

// errorBody reads the server's JSON error, or the status when there is none.
func errorBody(resp *http.Response) string {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(raw, &e) == nil && e.Error != "" {
		return e.Error + " (" + strconv.Itoa(resp.StatusCode) + ")"
	}
	if s := strings.TrimSpace(string(raw)); s != "" {
		return s + " (" + resp.Status + ")"
	}
	return resp.Status
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

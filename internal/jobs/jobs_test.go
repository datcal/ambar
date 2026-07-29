package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func newTestQueue(t *testing.T, workers int) (*Queue, *db.DB) {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return New(database, Options{Workers: workers}), database
}

// runUntil starts the queue and stops it once done fires or the deadline passes.
func runUntil(t *testing.T, q *Queue, done <-chan struct{}, timeout time.Duration) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		if err := q.Run(ctx); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	select {
	case <-done:
	case <-time.After(timeout):
		t.Error("timed out waiting for the queue to do its work")
	}
	cancel()

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Error("the queue did not shut down")
	}
}

func TestEnqueueAndRun(t *testing.T) {
	q, database := newTestQueue(t, 1)

	type payload struct{ Name string }
	got := make(chan string, 1)
	q.Register("greet", func(ctx context.Context, raw []byte) error {
		var p payload
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		got <- p.Name
		return nil
	})

	id, err := q.Enqueue(context.Background(), "greet", payload{Name: "ada"}, EnqueueOptions{})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if id == 0 {
		t.Error("no id returned")
	}

	// Waiting for the *row* rather than for the handler's signal. The handler sends
	// before it returns, so stopping the queue on that signal would race the
	// completion write — which is exactly the ordering bug the race detector found.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var state string
			if err := database.Reader.QueryRow(`SELECT state FROM jobs`).Scan(&state); err == nil &&
				state == StateDone {
				return
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	runUntil(t, q, done, 10*time.Second)

	select {
	case name := <-got:
		if name != "ada" {
			t.Errorf("handler received %q, want ada", name)
		}
	default:
		t.Error("the handler never ran")
	}

	stats, err := q.Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Done != 1 || stats.Queued != 0 || stats.Failed != 0 {
		t.Errorf("stats = %+v, want 1 done", stats)
	}
}

// TestClaimIsExclusive is the property the whole design rests on: the single writer
// connection (§4) means one UPDATE ... RETURNING is enough, with no SKIP LOCKED and no
// transaction to get wrong. If two workers can claim one row, every derive runs twice.
func TestClaimIsExclusive(t *testing.T) {
	q, _ := newTestQueue(t, 8)

	const total = 200
	var (
		mu      sync.Mutex
		seen    = map[int64]int{}
		counter atomic.Int64
	)

	q.Register("count", func(ctx context.Context, raw []byte) error {
		var p struct{ N int64 }
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		mu.Lock()
		seen[p.N]++
		mu.Unlock()
		counter.Add(1)
		return nil
	})

	for i := 0; i < total; i++ {
		if _, err := q.Enqueue(context.Background(), "count", map[string]int64{"N": int64(i)},
			EnqueueOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	go func() {
		for counter.Load() < total {
			time.Sleep(5 * time.Millisecond)
		}
		close(done)
	}()
	runUntil(t, q, done, 30*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != total {
		t.Errorf("%d distinct jobs ran, want %d", len(seen), total)
	}
	for n, times := range seen {
		if times != 1 {
			t.Errorf("job %d ran %d times, want exactly once", n, times)
		}
	}
}

// TestDedupeKey covers §6's "idempotent, keyed on sha256 + derive_version, so rescans
// do no work" — enforced by the partial unique index, not by caller discipline.
func TestDedupeKey(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	first, err := q.Enqueue(ctx, "derive", nil, EnqueueOptions{DedupeKey: "derive:abc:1"})
	if err != nil {
		t.Fatalf("first enqueue: %v", err)
	}

	_, err = q.Enqueue(ctx, "derive", nil, EnqueueOptions{DedupeKey: "derive:abc:1"})
	if !errors.Is(err, ErrDuplicate) {
		t.Errorf("second enqueue returned %v, want ErrDuplicate", err)
	}

	// A different key is unaffected.
	if _, err := q.Enqueue(ctx, "derive", nil, EnqueueOptions{DedupeKey: "derive:abc:2"}); err != nil {
		t.Errorf("a different dedupe key was rejected: %v", err)
	}
	// And no key at all never dedupes.
	for i := 0; i < 3; i++ {
		if _, err := q.Enqueue(ctx, "other", nil, EnqueueOptions{}); err != nil {
			t.Errorf("un-keyed enqueue %d failed: %v", i, err)
		}
	}

	// Once the job is done the key is free again, so a derive_version bump can
	// legitimately re-schedule the same asset.
	if err := q.finishDone(ctx, first); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, "derive", nil, EnqueueOptions{DedupeKey: "derive:abc:1"}); err != nil {
		t.Errorf("re-enqueue after completion failed: %v", err)
	}

	var n int
	if err := database.Reader.QueryRow(`SELECT count(*) FROM jobs`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 6 {
		t.Errorf("%d job rows, want 6", n)
	}
}

// TestRetryWithBackoff: a transient failure is retried, not parked.
func TestRetryWithBackoff(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	now := time.Now()
	q.WithClock(func() time.Time { return now })

	var attempts atomic.Int32
	q.Register("flaky", func(context.Context, []byte) error {
		attempts.Add(1)
		return errors.New("transient trouble")
	})

	id, err := q.Enqueue(ctx, "flaky", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Drive the loop by hand so the backoff is observable without sleeping.
	for i := 1; i <= MaxAttempts; i++ {
		ran, err := q.runOne(ctx, 0)
		if err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
		if !ran {
			t.Fatalf("attempt %d: no job was claimed (backoff not advanced?)", i)
		}

		var state string
		var runAfter int64
		if err := database.Reader.QueryRow(
			`SELECT state, run_after FROM jobs WHERE id = ?`, id).Scan(&state, &runAfter); err != nil {
			t.Fatal(err)
		}

		if i < MaxAttempts {
			if state != StateQueued {
				t.Errorf("after attempt %d state = %q, want queued", i, state)
			}
			// The retry must be in the future, or it would spin.
			if runAfter <= now.Unix() {
				t.Errorf("after attempt %d run_after is not in the future", i)
			}
			// Advance past the backoff for the next iteration.
			now = time.Unix(runAfter, 0)
		} else if state != StateFailed {
			t.Errorf("after the final attempt state = %q, want failed", state)
		}
	}

	if got := attempts.Load(); got != MaxAttempts {
		t.Errorf("handler ran %d times, want %d", got, MaxAttempts)
	}

	var lastError string
	if err := database.Reader.QueryRow(`SELECT last_error FROM jobs WHERE id = ?`, id).
		Scan(&lastError); err != nil {
		t.Fatal(err)
	}
	// §12: failures must be inspectable in the UI, so the message has to be stored.
	if lastError != "transient trouble" {
		t.Errorf("last_error = %q", lastError)
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	prev := time.Duration(0)
	for attempt := 1; attempt <= 12; attempt++ {
		d := backoff(attempt)
		if d <= 0 {
			t.Fatalf("backoff(%d) = %s, want positive", attempt, d)
		}
		if d > backoffMax {
			t.Errorf("backoff(%d) = %s, over the %s cap", attempt, d, backoffMax)
		}
		if attempt > 1 && d < prev {
			t.Errorf("backoff(%d) = %s, less than backoff(%d) = %s", attempt, d, attempt-1, prev)
		}
		prev = d
	}
	// Guard against a negative attempt count producing a zero delay and a spin.
	if backoff(0) <= 0 || backoff(-5) <= 0 {
		t.Error("backoff must be positive for a non-positive attempt count")
	}
}

// TestUnknownTypeFailsImmediately: an unregistered type is a programming error, so
// burning three attempts on it wastes time and muddies the log.
func TestUnknownTypeFailsImmediately(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, "nobody-handles-this", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if ran, err := q.runOne(ctx, 0); err != nil || !ran {
		t.Fatalf("runOne: ran=%v err=%v", ran, err)
	}

	var (
		state     string
		attempts  int
		lastError string
	)
	if err := database.Reader.QueryRow(
		`SELECT state, attempts, last_error FROM jobs WHERE id = ?`, id).
		Scan(&state, &attempts, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Errorf("state = %q, want failed", state)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1 — no point retrying a missing handler", attempts)
	}
	if lastError == "" {
		t.Error("no error recorded")
	}
}

// TestPanicInHandlerBecomesAFailure: §16 calls a recovered panic a bug to fix, but one
// malformed image must not take down the worker pool and stall every other job.
func TestPanicInHandlerBecomesAFailure(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	q.Register("boom", func(context.Context, []byte) error {
		panic("deliberate test panic")
	})

	id, err := q.Enqueue(ctx, "boom", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Must not propagate out of runOne.
	for i := 0; i < MaxAttempts; i++ {
		if _, err := q.runOne(ctx, 0); err != nil {
			t.Fatalf("runOne returned an error: %v", err)
		}
		// Clear the backoff so the next attempt is claimable.
		if _, err := database.Writer.Exec(`UPDATE jobs SET run_after = 0 WHERE id = ?`, id); err != nil {
			t.Fatal(err)
		}
	}

	var state, lastError string
	if err := database.Reader.QueryRow(
		`SELECT state, last_error FROM jobs WHERE id = ?`, id).Scan(&state, &lastError); err != nil {
		t.Fatal(err)
	}
	if state != StateFailed {
		t.Errorf("state = %q, want failed", state)
	}
	if lastError == "" || !strings.Contains(lastError, "panicked") {
		t.Errorf("last_error = %q, want it to mention the panic", lastError)
	}
}

// TestRequeueStale is §12's "finish or requeue in-flight jobs". A row in 'running' at
// startup can only mean the process died mid-job.
func TestRequeueStale(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	id, err := q.Enqueue(ctx, "work", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: claimed, never finished.
	if _, err := database.Writer.Exec(
		`UPDATE jobs SET state='running', started_at=1 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}

	n, err := q.RequeueStale(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("requeued %d jobs, want 1", n)
	}

	var state string
	var startedAt *int64
	if err := database.Reader.QueryRow(
		`SELECT state, started_at FROM jobs WHERE id = ?`, id).Scan(&state, &startedAt); err != nil {
		t.Fatal(err)
	}
	if state != StateQueued {
		t.Errorf("state = %q, want queued", state)
	}
	if startedAt != nil {
		t.Error("started_at was not cleared")
	}
}

// TestShutdownLeavesJobRequeueable: cancelling mid-job must not burn the attempt or
// mark it failed, because nothing went wrong.
func TestShutdownLeavesJobRequeueable(t *testing.T) {
	q, database := newTestQueue(t, 1)

	started := make(chan struct{})
	q.Register("slow", func(ctx context.Context, _ []byte) error {
		close(started)
		<-ctx.Done() // block until shutdown
		return ctx.Err()
	})

	if _, err := q.Enqueue(context.Background(), "slow", nil, EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		q.Run(ctx) //nolint:errcheck
	}()

	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never started")
	}
	cancel()

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("the queue did not shut down")
	}

	var state string
	if err := database.Reader.QueryRow(`SELECT state FROM jobs`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != StateRunning {
		t.Errorf("state = %q, want running so the next startup requeues it", state)
	}

	// And that is exactly what the next startup does.
	if n, err := q.RequeueStale(context.Background()); err != nil || n != 1 {
		t.Errorf("RequeueStale = %d, %v; want 1", n, err)
	}
}

func TestPriorityOrder(t *testing.T) {
	q, _ := newTestQueue(t, 1)
	ctx := context.Background()

	var order []string
	var mu sync.Mutex
	q.Register("ordered", func(_ context.Context, raw []byte) error {
		var p struct{ Name string }
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		mu.Lock()
		order = append(order, p.Name)
		mu.Unlock()
		return nil
	})

	// Enqueued low-priority first, so only priority can explain the order.
	for _, tc := range []struct {
		name     string
		priority int
	}{
		{"low", 0}, {"high", 100}, {"medium", 50},
	} {
		if _, err := q.Enqueue(ctx, "ordered", map[string]string{"Name": tc.name},
			EnqueueOptions{Priority: tc.priority}); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < 3; i++ {
		if ran, err := q.runOne(ctx, 0); err != nil || !ran {
			t.Fatalf("run %d: ran=%v err=%v", i, ran, err)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	// A user-triggered scan must outrank the derive jobs it will itself enqueue.
	if len(order) != 3 || order[0] != "high" || order[1] != "medium" || order[2] != "low" {
		t.Errorf("order = %v, want [high medium low]", order)
	}
}

func TestDelayedJobIsNotClaimedEarly(t *testing.T) {
	q, _ := newTestQueue(t, 1)
	ctx := context.Background()

	now := time.Now()
	q.WithClock(func() time.Time { return now })
	q.Register("later", func(context.Context, []byte) error { return nil })

	if _, err := q.Enqueue(ctx, "later", nil, EnqueueOptions{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}

	if ran, err := q.runOne(ctx, 0); err != nil {
		t.Fatal(err)
	} else if ran {
		t.Error("a delayed job was claimed before its time")
	}

	now = now.Add(2 * time.Hour)
	if ran, err := q.runOne(ctx, 0); err != nil {
		t.Fatal(err)
	} else if !ran {
		t.Error("the job was not claimed after its delay passed")
	}
}

// TestRetryFailed is §6's "retry failed derivatives" action.
func TestRetryFailed(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	q.Register("always-fails", func(context.Context, []byte) error {
		return errors.New("nope")
	})

	// Two failed jobs of one type, one of another.
	for i := 0; i < 2; i++ {
		id, err := q.Enqueue(ctx, "always-fails", map[string]int{"i": i}, EnqueueOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.finishFailed(ctx, id, "nope"); err != nil {
			t.Fatal(err)
		}
	}
	otherID, err := q.Enqueue(ctx, "other-type", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.finishFailed(ctx, otherID, "nope"); err != nil {
		t.Fatal(err)
	}

	// Filtered by type.
	n, err := q.RetryFailed(ctx, "always-fails")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("requeued %d, want 2", n)
	}

	var queued, failed int
	if err := database.Reader.QueryRow(
		`SELECT sum(state='queued'), sum(state='failed') FROM jobs`).Scan(&queued, &failed); err != nil {
		t.Fatal(err)
	}
	if queued != 2 || failed != 1 {
		t.Errorf("queued=%d failed=%d, want 2 and 1", queued, failed)
	}

	// Attempts are reset: a human asking for a retry is new information.
	var attempts int
	if err := database.Reader.QueryRow(
		`SELECT max(attempts) FROM jobs WHERE state='queued'`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if attempts != 0 {
		t.Errorf("attempts = %d after a retry request, want 0", attempts)
	}

	// And with no filter, everything failed is requeued.
	if n, err = q.RetryFailed(ctx, ""); err != nil || n != 1 {
		t.Errorf("RetryFailed(all) = %d, %v; want 1", n, err)
	}
}

func TestRecentOrdersUnfinishedFirst(t *testing.T) {
	q, _ := newTestQueue(t, 1)
	ctx := context.Background()

	doneID, err := q.Enqueue(ctx, "a", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.finishDone(ctx, doneID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.Enqueue(ctx, "b", nil, EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}

	jobs, err := q.Recent(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 {
		t.Fatalf("%d jobs, want 2", len(jobs))
	}
	// Someone opening /jobs wants to see what is still happening.
	if jobs[0].State != StateQueued {
		t.Errorf("first job state = %q, want queued first", jobs[0].State)
	}

	filtered, err := q.Recent(ctx, StateDone, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != doneID {
		t.Errorf("state filter returned %d jobs", len(filtered))
	}
}

func TestEnqueueRejectsEmptyType(t *testing.T) {
	q, _ := newTestQueue(t, 1)
	if _, err := q.Enqueue(context.Background(), "", nil, EnqueueOptions{}); err == nil {
		t.Error("an empty job type was accepted")
	}
}

func TestErrorMessageIsTruncated(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	huge := make([]byte, 10_000)
	for i := range huge {
		huge[i] = 'x'
	}
	q.Register("verbose", func(context.Context, []byte) error {
		return fmt.Errorf("%s", huge)
	})

	id, err := q.Enqueue(ctx, "verbose", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.runOne(ctx, 0); err != nil {
		t.Fatal(err)
	}

	var stored string
	if err := database.Reader.QueryRow(`SELECT last_error FROM jobs WHERE id = ?`, id).
		Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if len(stored) > maxErrorLen+32 {
		t.Errorf("stored error is %d bytes; it should be truncated", len(stored))
	}
}

func TestPrune(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx := context.Background()

	old, err := q.Enqueue(ctx, "old", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.finishDone(ctx, old); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Writer.Exec(
		`UPDATE jobs SET finished_at = ? WHERE id = ?`,
		time.Now().Add(-48*time.Hour).Unix(), old); err != nil {
		t.Fatal(err)
	}

	recent, err := q.Enqueue(ctx, "recent", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.finishDone(ctx, recent); err != nil {
		t.Fatal(err)
	}
	// And one still queued, which must never be pruned.
	if _, err := q.Enqueue(ctx, "pending", nil, EnqueueOptions{}); err != nil {
		t.Fatal(err)
	}

	n, err := q.Prune(ctx, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("pruned %d rows, want 1", n)
	}

	var remaining int
	if err := database.Reader.QueryRow(`SELECT count(*) FROM jobs`).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Errorf("%d rows remain, want 2", remaining)
	}
}

// TestCompletionSurvivesShutdown pins the fix for an ordering bug the race detector
// surfaced: a handler that succeeds just as shutdown begins must still have its
// completion recorded.
//
// Without it the row stays 'running', the next startup requeues it, and the job runs
// twice. Harmless for an idempotent derive; wrong in general.
func TestCompletionSurvivesShutdown(t *testing.T) {
	q, database := newTestQueue(t, 1)
	ctx, cancel := context.WithCancel(context.Background())

	q.Register("finishes-then-shutdown", func(context.Context, []byte) error {
		// Cancel *before* returning, so the completion write happens with an
		// already-cancelled context.
		cancel()
		return nil
	})

	id, err := q.Enqueue(context.Background(), "finishes-then-shutdown", nil, EnqueueOptions{})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := q.runOne(ctx, 0); err != nil {
		t.Fatalf("runOne: %v", err)
	}

	var state string
	if err := database.Reader.QueryRow(`SELECT state FROM jobs WHERE id = ?`, id).
		Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != StateDone {
		t.Errorf("state = %q, want done — the completion write must not be abandoned "+
			"because the context was cancelled", state)
	}
}

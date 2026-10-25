package remote

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
)

type fixture struct {
	db      *db.DB
	queue   *jobs.Queue
	deriver *derive.Deriver
	clock   time.Time
}

func (f *fixture) now() time.Time { return f.clock }

// newFixture indexes a library of n small PNGs and queues their derives.
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	base := t.TempDir()
	lib := filepath.Join(base, "library")
	for i := 0; i < n; i++ {
		img := image.NewRGBA(image.Rect(0, 0, 8, 8))
		img.Set(0, 0, color.RGBA{R: uint8(i), A: 0xff})
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(lib, "pack", "img"+string(rune('a'+i))+".png")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	database, err := db.Open(filepath.Join(base, "ambar.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	ctx := context.Background()
	if _, err := database.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.New(database, index.Options{Root: lib}).Scan(ctx, index.ScanOptions{}); err != nil {
		t.Fatal(err)
	}

	f := &fixture{db: database, clock: time.Unix(1_800_000_000, 0)}
	f.queue = jobs.New(database, jobs.Options{Workers: 1}).WithClock(f.now)
	f.deriver = derive.New(database, derive.Options{LibraryRoot: lib, DataRoot: filepath.Join(base, "data")})
	f.deriver.Register(f.queue)
	if _, err := derive.EnqueueStale(ctx, database, f.queue); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fixture) coordinator(mode string) *Coordinator {
	return NewCoordinator(f.db, f.queue, f.deriver, Options{Mode: mode}).WithClock(f.now)
}

func (f *fixture) jobState(t *testing.T, id int64) (state string, attempts int) {
	t.Helper()
	if err := f.db.Reader.QueryRow(`SELECT state, attempts FROM jobs WHERE id = ?`, id).
		Scan(&state, &attempts); err != nil {
		t.Fatal(err)
	}
	return state, attempts
}

func claimRequest(session string) ClaimRequest {
	return ClaimRequest{Worker: "pc", Session: session, DeriveVersion: derive.Version}
}

// TestExpiredLeaseIsTakenBack: a worker that vanishes mid-job — a laptop lid, a crash —
// must not strand the job in 'running' until the server restarts. The attempt stays
// spent, so a file that kills every worker that opens it still runs out of tries.
func TestExpiredLeaseIsTakenBack(t *testing.T) {
	f := newFixture(t, 1)
	c := f.coordinator(ModeOnly)
	ctx := context.Background()

	claim, err := c.Claim(ctx, claimRequest("s1"))
	if err != nil || claim == nil {
		t.Fatalf("claim = %v, %v", claim, err)
	}
	if state, _ := f.jobState(t, claim.JobID); state != "running" {
		t.Fatalf("a claimed job is %s, want running", state)
	}

	f.clock = f.clock.Add(LeaseDuration - time.Minute)
	c.expire(ctx)
	if state, _ := f.jobState(t, claim.JobID); state != "running" {
		t.Errorf("before the lease ends the job is %s, want running", state)
	}

	f.clock = f.clock.Add(2 * time.Minute)
	c.expire(ctx)
	state, attempts := f.jobState(t, claim.JobID)
	if state != "queued" || attempts != 1 {
		t.Errorf("after the lease ends: %s with %d attempts, want queued with 1", state, attempts)
	}
	// And the late result is refused rather than recorded twice.
	if err := c.Submit(ctx, claim.JobID, "s1", derive.Outcome{State: derive.StateUnsupported}, t.TempDir()); err != ErrNoLease {
		t.Errorf("a result after the lease ended = %v, want ErrNoLease", err)
	}
}

// TestReleaseRefundsTheAttempt: a worker that says it is stopping hands its jobs back
// whole.
func TestReleaseRefundsTheAttempt(t *testing.T) {
	f := newFixture(t, 1)
	c := f.coordinator(ModeOnly)
	ctx := context.Background()

	claim, _ := c.Claim(ctx, claimRequest("s1"))
	if claim == nil {
		t.Fatal("no claim")
	}
	c.Release(ctx, ReleaseRequest{Session: "someone-else", JobIDs: []int64{claim.JobID}})
	if state, _ := f.jobState(t, claim.JobID); state != "running" {
		t.Errorf("another session released the job: %s", state)
	}
	c.Release(ctx, ReleaseRequest{Session: "s1", JobIDs: []int64{claim.JobID}})
	if state, attempts := f.jobState(t, claim.JobID); state != "queued" || attempts != 0 {
		t.Errorf("after release: %s with %d attempts, want queued with 0", state, attempts)
	}
}

// TestAutoModeStepsAsideWhileAWorkerIsConnected: the server's own workers leave derive
// jobs alone exactly while a remote worker has asked for work recently, and take them
// back when it goes quiet. Off never steps aside; only always does.
func TestAutoModeStepsAsideWhileAWorkerIsConnected(t *testing.T) {
	f := newFixture(t, 1)
	ctx := context.Background()

	if skip := f.coordinator(ModeOff).SkipLocal(); len(skip) != 0 {
		t.Errorf("off skips %v", skip)
	}
	if skip := f.coordinator(ModeOnly).SkipLocal(); len(skip) != 1 || skip[0] != derive.JobType {
		t.Errorf("only skips %v, want [%s]", skip, derive.JobType)
	}

	c := f.coordinator(ModeAuto)
	if skip := c.SkipLocal(); len(skip) != 0 {
		t.Errorf("auto with no worker skips %v", skip)
	}
	if _, err := c.Claim(ctx, claimRequest("s1")); err != nil {
		t.Fatal(err)
	}
	if skip := c.SkipLocal(); len(skip) != 1 {
		t.Errorf("auto with a worker connected skips %v, want derive", skip)
	}
	f.clock = f.clock.Add(ActiveWindow + time.Second)
	if skip := c.SkipLocal(); len(skip) != 0 {
		t.Errorf("auto after the worker went quiet skips %v", skip)
	}
}

// TestABlenderWorkerIsOfferedWhatWaitsOnBlender: FBX files sit in needs_blender at the
// current derive version, so nothing would ever queue them again. A worker that has
// Blender is the reason to.
func TestABlenderWorkerIsOfferedWhatWaitsOnBlender(t *testing.T) {
	f := newFixture(t, 2)
	ctx := context.Background()

	// Pretend both were derived on a server without Blender.
	if _, err := f.db.Writer.Exec(`UPDATE assets SET derive_state = 'needs_blender', derive_version = ?`,
		derive.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`UPDATE jobs SET state = 'done', dedupe_key = NULL`); err != nil {
		t.Fatal(err)
	}

	c := f.coordinator(ModeOnly)
	if claim, err := c.Claim(ctx, claimRequest("s1")); err != nil || claim != nil {
		t.Fatalf("a worker without Blender was handed %v, %v", claim, err)
	}

	req := claimRequest("s2")
	req.Blender = true
	deadline := time.Now().Add(5 * time.Second)
	for {
		claim, err := c.Claim(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		if claim != nil {
			break // the backlog was queued in the background and is now being handed out
		}
		if time.Now().After(deadline) {
			t.Fatal("a Blender worker was never offered the needs_blender assets")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestClaimRefusesAMismatchedWorker(t *testing.T) {
	f := newFixture(t, 1)
	req := claimRequest("s1")
	req.DeriveVersion = derive.Version + 1
	if _, err := f.coordinator(ModeAuto).Claim(context.Background(), req); err == nil {
		t.Error("a worker built from different derive code was handed a job")
	}
	if _, err := f.coordinator(ModeOff).Claim(context.Background(), claimRequest("s1")); err != ErrDisabled {
		t.Errorf("off mode claim = %v, want ErrDisabled", err)
	}
}

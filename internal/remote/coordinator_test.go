package remote

import (
	"bytes"
	"context"
	"database/sql"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/HugoSmits86/nativewebp"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
)

type fixture struct {
	db       *db.DB
	queue    *jobs.Queue
	deriver  *derive.Deriver
	dataRoot string
	clock    time.Time
}

func (f *fixture) now() time.Time { return f.clock }

// newFixture indexes a library of n small PNGs and queues their derives.
func newFixture(t *testing.T, n int) *fixture {
	t.Helper()
	return newLibraryFixture(t, n, nil)
}

// newLibraryFixture is newFixture with these extra files in the library as well.
func newLibraryFixture(t *testing.T, n int, extra map[string]string) *fixture {
	t.Helper()
	base := t.TempDir()
	lib := filepath.Join(base, "library")
	for rel, content := range extra {
		path := filepath.Join(lib, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
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

	f := &fixture{db: database, dataRoot: filepath.Join(base, "data"), clock: time.Unix(1_800_000_000, 0)}
	f.queue = jobs.New(database, jobs.Options{Workers: 1}).WithClock(f.now)
	f.deriver = derive.New(database, derive.Options{LibraryRoot: lib, DataRoot: f.dataRoot})
	f.deriver.Register(f.queue)
	f.deriver.RegisterThumb(f.queue)
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

	// Without a Blender of its own the server never draws thumbnails, whatever the mode.
	thumbs := []string{derive.ThumbJobType}
	both := []string{derive.JobType, derive.ThumbJobType}
	if skip := f.coordinator(ModeOff).SkipLocal(); !slices.Equal(skip, thumbs) {
		t.Errorf("off skips %v, want %v", skip, thumbs)
	}
	local := NewCoordinator(f.db, f.queue, f.deriver, Options{Mode: ModeOff, LocalThumbs: true})
	if skip := local.SkipLocal(); len(skip) != 0 {
		t.Errorf("off with a Blender of its own skips %v", skip)
	}
	if skip := f.coordinator(ModeOnly).SkipLocal(); !slices.Equal(skip, both) {
		t.Errorf("only skips %v, want %v", skip, both)
	}

	c := f.coordinator(ModeAuto)
	if skip := c.SkipLocal(); !slices.Equal(skip, thumbs) {
		t.Errorf("auto with no worker skips %v, want %v", skip, thumbs)
	}
	if _, err := c.Claim(ctx, claimRequest("s1")); err != nil {
		t.Fatal(err)
	}
	if skip := c.SkipLocal(); !slices.Equal(skip, both) {
		t.Errorf("auto with a worker connected skips %v, want %v", skip, both)
	}
	f.clock = f.clock.Add(ActiveWindow + time.Second)
	if skip := c.SkipLocal(); !slices.Equal(skip, thumbs) {
		t.Errorf("auto after the worker went quiet skips %v, want %v", skip, thumbs)
	}
}

// TestABlenderWorkerDrawsTheModelThumbnails: a worker that can draw is offered every
// derived model without a Blender thumbnail, its picture is moved into place, and a model
// with nothing to draw is remembered so it is not offered again. A worker that cannot
// draw is never handed one.
func TestABlenderWorkerDrawsTheModelThumbnails(t *testing.T) {
	const tri = "v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"
	f := newLibraryFixture(t, 0, map[string]string{
		"pack/crate.obj": tri,
		"pack/rig.obj":   tri + "# a different file\n",
	})
	ctx := context.Background()
	// Both derived, and nothing else waiting.
	if _, err := f.db.Writer.Exec(`UPDATE assets SET derive_state = 'ok', derive_version = ?`, derive.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Writer.Exec(`UPDATE jobs SET state = 'done', dedupe_key = NULL`); err != nil {
		t.Fatal(err)
	}

	c := f.coordinator(ModeOnly)
	blenderOnly := claimRequest("old")
	blenderOnly.Blender = true
	if claim, err := c.Claim(ctx, blenderOnly); err != nil || claim != nil {
		t.Fatalf("a worker that cannot draw was handed %+v, %v", claim, err)
	}

	drawer := claimRequest("new")
	drawer.Blender, drawer.Thumbs = true, true
	got := map[string]*ClaimResponse{}
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < 2 {
		claim, err := c.Claim(ctx, drawer)
		if err != nil {
			t.Fatal(err)
		}
		if claim != nil {
			if claim.Task.Op != derive.OpThumb || claim.Task.PackPath != "pack" {
				t.Fatalf("handed %+v, want a thumbnail in pack", claim.Task)
			}
			got[claim.Task.Filename] = claim
			continue
		}
		if time.Now().After(deadline) {
			t.Fatalf("the models were never offered to draw; got %v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The crate is drawn.
	crate := got["crate.obj"]
	staging := t.TempDir()
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 16; y < 48; y++ {
		for x := 16; x < 48; x++ {
			img.Set(x, y, color.RGBA{R: 0xc0, G: 0x80, B: 0x30, A: 0xff})
		}
	}
	var webp bytes.Buffer
	if err := nativewebp.Encode(&webp, img, nil); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string][]byte{
		derive.FileThumb:     webp.Bytes(),
		derive.FilePreview:   webp.Bytes(),
		derive.FileThumbMeta: []byte(`{"renderer":3,"untextured":0,"drawn_at":1}`),
	} {
		if err := os.WriteFile(filepath.Join(staging, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ok := derive.Outcome{State: derive.StateOK, Result: &derive.Result{
		Files: []string{derive.FileThumb, derive.FilePreview, derive.FileThumbMeta}}}
	if err := c.Submit(ctx, crate.JobID, "new", ok, staging); err != nil {
		t.Fatal(err)
	}
	if state, _ := f.jobState(t, crate.JobID); state != "done" {
		t.Errorf("the drawn thumbnail's job is %s, want done", state)
	}
	dir := f.thumbDir(t, crate.Task.SHA256)
	if derive.ModelThumbStatus(dir, derive.RendererBlender) != derive.ThumbCurrent {
		t.Error("the crate's thumbnail is not current after it was drawn")
	}
	var palette sql.NullString
	if err := f.db.Reader.QueryRow(`SELECT palette_json FROM assets WHERE id = ?`, crate.Task.AssetID).Scan(&palette); err != nil || !palette.Valid {
		t.Errorf("no palette recorded from the drawn thumbnail: %v, %v", palette, err)
	}
	var state string
	if err := f.db.Reader.QueryRow(`SELECT derive_state FROM assets WHERE id = ?`, crate.Task.AssetID).Scan(&state); err != nil || state != "ok" {
		t.Errorf("derive_state after a thumbnail = %q, %v; want it untouched", state, err)
	}

	// The rig has nothing to draw.
	rig := got["rig.obj"]
	nothing := derive.Outcome{State: derive.StateUnsupported, Message: "nothing to draw: no geometry"}
	if err := c.Submit(ctx, rig.JobID, "new", nothing, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if derive.ModelThumbStatus(f.thumbDir(t, rig.Task.SHA256), derive.RendererBlender) != derive.ThumbCurrent {
		t.Error("a model with nothing to draw would be offered again")
	}

	// So the next worker to connect is offered nothing.
	if n, err := f.deriver.QueueThumbs(ctx); err != nil || n != 0 {
		t.Errorf("queued %d thumbnails after both were settled (%v), want none", n, err)
	}
}

func (f *fixture) thumbDir(t *testing.T, sha256hex string) string {
	t.Helper()
	rel, err := derive.Dir(sha256hex)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(f.dataRoot, rel)
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

// TestTheBlenderWorkerOutlivesARestart: the jobs page says which worker with Blender was
// here last and when, after the server restarted too — the night a worker stopped and the
// server was redeployed is when the page most needs to say how long it has been gone.
func TestTheBlenderWorkerOutlivesARestart(t *testing.T) {
	f := newFixture(t, 1)
	seen := filepath.Join(t.TempDir(), "blender-worker.json")
	c := NewCoordinator(f.db, f.queue, f.deriver, Options{Mode: ModeAuto, SeenFile: seen}).WithClock(f.now)
	ctx := context.Background()

	if w, ok := c.Blender(); ok {
		t.Fatalf("a worker with Blender before any connected: %+v", w)
	}
	if _, err := c.Claim(ctx, claimRequest("plain")); err != nil {
		t.Fatal(err)
	}
	if w, ok := c.Blender(); ok {
		t.Errorf("a worker without Blender counted as one with it: %+v", w)
	}

	req := claimRequest("blender")
	req.Worker, req.Blender = "linux1-podman", true
	if _, err := c.Claim(ctx, req); err != nil {
		t.Fatal(err)
	}
	if w, ok := c.Blender(); !ok || !w.Connected || w.Name != "linux1-podman" {
		t.Errorf("while it asks for work: %+v, %v", w, ok)
	}

	f.clock = f.clock.Add(3 * time.Hour)
	after := NewCoordinator(f.db, f.queue, f.deriver, Options{Mode: ModeAuto, SeenFile: seen}).WithClock(f.now)
	w, ok := after.Blender()
	if !ok || w.Connected || w.Name != "linux1-podman" || !w.LastSeen.Equal(time.Unix(1_800_000_000, 0)) {
		t.Errorf("after a restart: %+v, %v; want linux1-podman, not connected, last seen at its claim", w, ok)
	}
}

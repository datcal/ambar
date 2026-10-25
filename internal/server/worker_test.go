package server

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/remote"
)

// attachCoordinator turns the worker API on, as serve does with AMBAR_REMOTE_DERIVE.
func (ts *testServer) attachCoordinator(mode string) *remote.Coordinator {
	c := remote.NewCoordinator(ts.db, ts.queue, ts.deriver, remote.Options{Mode: mode})
	ts.srv.EnableRemoteWorkers(c)
	return c
}

// seedImages writes PNGs into the library, indexes them and queues their derives —
// everything up to the point a worker would take over.
func (ts *testServer) seedImages(t *testing.T, files map[string][]byte) {
	t.Helper()
	for rel, content := range files {
		abs := filepath.Join(ts.cfg.LibraryRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if _, err := index.New(ts.db, index.Options{Root: ts.cfg.LibraryRoot}).Scan(ctx, index.ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := derive.EnqueueStale(ctx, ts.db, ts.queue); err != nil {
		t.Fatal(err)
	}
}

func (ts *testServer) postWorker(t *testing.T, path, token, contentType string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, ts.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", contentType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// TestRemoteWorkerDerivesForTheServer is the whole round trip: the server queues work,
// a worker on "another machine" — here the same library directory, standing in for an
// SMB mount — claims it over HTTP, decodes locally, and uploads; the server's database
// and derivative directory end up exactly as a local derive would have left them.
func TestRemoteWorkerDerivesForTheServer(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)
	ts.attachCoordinator(remote.ModeOnly)
	ts.seedImages(t, map[string][]byte{
		"pack/hero.png": spritePNG(t, 32, 32),
		"pack/tile.png": spritePNG(t, 48, 48),
	})

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() {
		stopped <- remote.RunWorker(ctx, remote.WorkerOptions{
			Server: ts.URL, Token: token,
			LibraryRoot: ts.cfg.LibraryRoot, WorkDir: t.TempDir(),
			Concurrency: 2, IdleWait: 20 * time.Millisecond, Log: discardLogger(),
		})
	}()

	deadline := time.Now().Add(15 * time.Second)
	for {
		var ok int
		if err := ts.db.Reader.QueryRow(
			`SELECT count(*) FROM assets WHERE derive_state = 'ok'`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		if ok == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 2 assets derived before the deadline", ok)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if err := <-stopped; err != nil {
		t.Errorf("worker stopped with %v", err)
	}

	for _, rel := range []string{"pack/hero.png", "pack/tile.png"} {
		var row struct {
			sha   string
			width int
		}
		if err := ts.db.Reader.QueryRow(`
			SELECT a.sha256, a.width FROM assets a JOIN packs p ON p.id = a.pack_id
			WHERE p.library_rel_path || '/' || a.rel_path = ?`, rel).Scan(&row.sha, &row.width); err != nil {
			t.Fatal(err)
		}
		if row.width == 0 {
			t.Errorf("%s: no width recorded; the analysis did not come back", rel)
		}
		relDir, _ := derive.Dir(row.sha)
		if _, err := os.Stat(filepath.Join(ts.cfg.DataRoot, relDir, derive.FileThumb)); err != nil {
			t.Errorf("%s: the thumbnail is not on the server: %v", rel, err)
		}
	}

	var queued int
	if err := ts.db.Reader.QueryRow(
		`SELECT count(*) FROM jobs WHERE type = ? AND state != 'done'`, derive.JobType).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d derive jobs are not done", queued)
	}
	// Every upload's staging directory is gone.
	leftovers, _ := filepath.Glob(filepath.Join(ts.cfg.DataRoot, "derivatives", ".incoming", "upload-*"))
	if len(leftovers) != 0 {
		t.Errorf("staging directories left behind: %v", leftovers)
	}
}

// TestWorkerAPIRefusals: the three answers that stop a worker doing harm — the feature
// is off, the worker's derive code differs from the server's, or it submits a result
// for a job it does not hold.
func TestWorkerAPIRefusals(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)

	claim := func(version int) *http.Response {
		body, _ := json.Marshal(remote.ClaimRequest{Worker: "pc", Session: "s1", DeriveVersion: version})
		return ts.postWorker(t, remote.ClaimPath, token, "application/json", body)
	}

	// No coordinator attached: the deployment never turned workers on.
	if resp := claim(derive.Version); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("with workers off, claim = %d, want 503", resp.StatusCode)
	}

	ts.attachCoordinator(remote.ModeAuto)
	if resp := claim(derive.Version + 1); resp.StatusCode != http.StatusConflict {
		t.Errorf("with a different derive version, claim = %d, want 409", resp.StatusCode)
	}
	if resp := claim(derive.Version); resp.StatusCode != http.StatusNoContent {
		t.Errorf("with nothing queued, claim = %d, want 204", resp.StatusCode)
	}

	var buf bytes.Buffer
	form := multipart.NewWriter(&buf)
	_ = form.WriteField("session", "s1")
	_ = form.WriteField("outcome", `{"state":"unsupported","message":"x"}`)
	_ = form.Close()
	resp := ts.postWorker(t, "/api/v1/worker/jobs/12345/result", token, form.FormDataContentType(), buf.Bytes())
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("a result for a job never handed out = %d, want 409", resp.StatusCode)
	}

	// A read-only token cannot drive the queue.
	if resp := ts.postWorker(t, remote.ClaimPath, ts.readToken(t), "application/json", []byte(`{}`)); resp.StatusCode != http.StatusForbidden {
		t.Errorf("a read token's claim = %d, want 403", resp.StatusCode)
	}
}

// TestWorkerCannotUploadArbitraryFiles: the result endpoint writes under the data root,
// so it accepts only the names derive itself writes, and only content that is what the
// name says.
func TestWorkerCannotUploadArbitraryFiles(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	token := ts.apiToken(t)
	ts.attachCoordinator(remote.ModeOnly)
	ts.seedImages(t, map[string][]byte{"pack/hero.png": spritePNG(t, 32, 32)})

	body, _ := json.Marshal(remote.ClaimRequest{Worker: "pc", Session: "s1", DeriveVersion: derive.Version})
	resp := ts.postWorker(t, remote.ClaimPath, token, "application/json", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d", resp.StatusCode)
	}
	var claim remote.ClaimResponse
	if err := json.NewDecoder(resp.Body).Decode(&claim); err != nil {
		t.Fatal(err)
	}

	submit := func(name string, content []byte) int {
		var buf bytes.Buffer
		form := multipart.NewWriter(&buf)
		_ = form.WriteField("session", "s1")
		outcome, _ := json.Marshal(derive.Outcome{State: derive.StateOK,
			Result: &derive.Result{Files: []string{name}}})
		_ = form.WriteField("outcome", string(outcome))
		part, _ := form.CreateFormFile("file", name)
		_, _ = part.Write(content)
		_ = form.Close()
		r := ts.postWorker(t, "/api/v1/worker/jobs/"+itoa("%d", claim.JobID)+"/result", token,
			form.FormDataContentType(), buf.Bytes())
		return r.StatusCode
	}

	if code := submit("../../evil.sh", []byte("#!/bin/sh")); code != http.StatusBadRequest {
		t.Errorf("an unknown file name = %d, want 400", code)
	}
	if code := submit(derive.FileThumb, []byte("not an image")); code != http.StatusBadRequest {
		t.Errorf("a thumbnail that is not an image = %d, want 400", code)
	}
	matches, _ := filepath.Glob(filepath.Join(ts.cfg.DataRoot, "derivatives", "*", "*", "*"))
	for _, m := range matches {
		if filepath.Base(filepath.Dir(m)) != ".incoming" {
			t.Errorf("a refused upload left %s behind", m)
		}
	}
	if _, err := os.Stat(filepath.Join(ts.cfg.DataRoot, "evil.sh")); err == nil {
		t.Error("a traversing file name escaped the staging directory")
	}
}

// TestJobsPageShowsRemoteWorkers: the point of the card is "who is connected and what is it
// doing", answered on the page and in the poll that keeps it live.
func TestJobsPageShowsRemoteWorkers(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	token := ts.apiToken(t)
	ts.attachCoordinator(remote.ModeAuto)
	ts.seedImages(t, map[string][]byte{"pack/hero.png": spritePNG(t, 32, 32)})

	body, _ := json.Marshal(remote.ClaimRequest{Worker: "linux1", Session: "s1",
		DeriveVersion: derive.Version, Blender: true, Concurrency: 8})
	if resp := ts.postWorker(t, remote.ClaimPath, token, "application/json", body); resp.StatusCode != http.StatusOK {
		t.Fatalf("claim = %d", resp.StatusCode)
	}

	page := readBody(t, ts.get(t, "/jobs"))
	for _, want := range []string{"Remote workers", "linux1", "connected", "pack/hero.png", "on linux1"} {
		if !strings.Contains(page, want) {
			t.Errorf("the jobs page does not show %q", want)
		}
	}

	var status struct {
		RemoteMode string `json:"remote_mode"`
		Workers    []remote.WorkerStatus
		Active     []struct {
			Worker string `json:"worker"`
		} `json:"active"`
	}
	if err := json.Unmarshal([]byte(readBody(t, ts.get(t, "/api/v1/jobs/status"))), &status); err != nil {
		t.Fatal(err)
	}
	if status.RemoteMode != remote.ModeAuto || len(status.Workers) != 1 {
		t.Fatalf("status = %+v", status)
	}
	w := status.Workers[0]
	if w.Name != "linux1" || !w.Connected || !w.Blender || w.Concurrency != 8 ||
		len(w.Current) != 1 || w.Current[0].File != "pack/hero.png" {
		t.Errorf("worker = %+v", w)
	}
	if len(status.Active) != 1 || status.Active[0].Worker != "linux1" {
		t.Errorf("the running job does not say where it runs: %+v", status.Active)
	}
}

// TestWorkPlacementOnTheJobsPage: the choice of where background work runs is made on the
// page, takes effect at once on both sides, and survives a restart.
func TestWorkPlacementOnTheJobsPage(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	token := ts.apiToken(t)
	stateFile := filepath.Join(ts.cfg.DataRoot, "work-placement")
	ts.srv.EnableRemoteWorkers(remote.NewCoordinator(ts.db, ts.queue, ts.deriver,
		remote.Options{Mode: remote.ModeOff, StateFile: stateFile}))
	ts.seedImages(t, map[string][]byte{"pack/hero.png": spritePNG(t, 32, 32)})

	claim := func() int {
		body, _ := json.Marshal(remote.ClaimRequest{Worker: "pc", Session: "s1", DeriveVersion: derive.Version})
		return ts.postWorker(t, remote.ClaimPath, token, "application/json", body).StatusCode
	}

	if page := readBody(t, ts.get(t, "/jobs")); !strings.Contains(page, "Where background work runs") {
		t.Fatal("the jobs page has no placement choice")
	}
	if code := claim(); code != http.StatusServiceUnavailable {
		t.Errorf("with \"this server\" chosen, a claim = %d, want 503", code)
	}

	if code, body := ts.postForm(t, "/jobs/placement", url.Values{"mode": {"only"}}); code != http.StatusSeeOther {
		t.Fatalf("placement = %d\n%s", code, body)
	}
	if !ts.queue.Paused() {
		t.Error("\"remote workers only\" did not stop this server's workers")
	}
	if code := claim(); code != http.StatusOK {
		t.Errorf("after choosing remote workers, a claim = %d, want 200", code)
	}
	if raw, _ := os.ReadFile(stateFile); strings.TrimSpace(string(raw)) != remote.ModeOnly {
		t.Errorf("the choice was not remembered: %q", raw)
	}
	if status := readBody(t, ts.get(t, "/api/v1/jobs/status")); !strings.Contains(status, `"local_paused":true`) {
		t.Errorf("the poll does not say paused: %s", status)
	}

	// A restart reads the choice back, whatever the environment default says.
	again := remote.NewCoordinator(ts.db, ts.queue, ts.deriver, remote.Options{Mode: remote.ModeOff, StateFile: stateFile})
	if again.Mode() != remote.ModeOnly {
		t.Errorf("after a restart the mode is %s, want only", again.Mode())
	}

	ts.postForm(t, "/jobs/placement", url.Values{"mode": {"off"}})
	if ts.queue.Paused() {
		t.Error("choosing this server did not restart its workers")
	}
}

// TestClearQueueFromTheJobsPage: the button deletes what is waiting, and only that.
func TestClearQueueFromTheJobsPage(t *testing.T) {
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedImages(t, map[string][]byte{
		"pack/a.png": spritePNG(t, 16, 16),
		"pack/b.png": spritePNG(t, 24, 24),
	})
	if page := readBody(t, ts.get(t, "/jobs")); !strings.Contains(page, "Clear the queue (2)") {
		t.Fatal("the jobs page does not offer to clear two waiting jobs")
	}
	if code, body := ts.postForm(t, "/jobs/clear-queue", url.Values{}); code != http.StatusSeeOther {
		t.Fatalf("clear = %d\n%s", code, body)
	}
	if n := ts.queuedJobs(t, derive.JobType); n != 0 {
		t.Errorf("%d jobs still queued", n)
	}
}

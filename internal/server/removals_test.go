package server

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/dupes"
	"github.com/datcal/ambar/internal/removal"
)

func removalTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	return ts
}

// libraryFile reports whether a library-relative path still exists.
func (ts *testServer) libraryFile(t *testing.T, relPath string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(ts.cfg.LibraryRoot, filepath.FromSlash(relPath)))
	return err == nil
}

func (ts *testServer) queuedJobs(t *testing.T, jobType string) int {
	t.Helper()
	var n int
	if err := ts.db.Reader.QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE type = ? AND state = 'queued'`, jobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// seedDuplicates indexes two byte-identical files plus one unique file.
func (ts *testServer) seedDuplicates(t *testing.T) {
	t.Helper()
	ts.seedLibrary(t, map[string]string{
		"pack/a/hero.png": "identical-bytes",
		"pack/b/hero.png": "identical-bytes",
		"pack/unique.png": "one-of-a-kind",
		"pack/.DS_Store":  "finder junk",
	})
}

func TestDupesPageEmptyBeforeScan(t *testing.T) {
	ts := removalTestServer(t)

	resp := ts.get(t, "/dupes")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)
	if !strings.Contains(body, "No scan has run yet") {
		t.Error("a first visit should say no scan has run yet")
	}
	// §9.1: nothing is ever pre-selected, and no page acts without a preview.
	if strings.Contains(body, "checked") {
		t.Error("no control may be pre-selected")
	}
	if strings.Contains(body, "/removals/apply") {
		t.Error("the report page must not post straight to apply")
	}
}

func TestDupesScanEnqueuesJob(t *testing.T) {
	ts := removalTestServer(t)

	status, _ := ts.postForm(t, "/dupes/scan", url.Values{})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", status)
	}
	if n := ts.queuedJobs(t, dupes.JobType); n != 1 {
		t.Fatalf("queued dupes jobs = %d, want 1", n)
	}
	// A double click coalesces rather than stacking a second sweep.
	ts.postForm(t, "/dupes/scan", url.Values{})
	if n := ts.queuedJobs(t, dupes.JobType); n != 1 {
		t.Errorf("queued dupes jobs = %d after a second press, want 1", n)
	}
}

func TestRemovalPlanPreviewsWithoutMovingAnything(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	status, body := ts.postForm(t, "/removals/plan", url.Values{
		"root":    {"library"},
		"action":  {"trash"},
		"finding": {"dupes:exact:test"},
		"reason":  {"Exact duplicate"},
		"path":    {"pack/a/hero.png"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	// §9.1: the preview lists every affected path and the total bytes.
	if !strings.Contains(body, "pack/a/hero.png") {
		t.Error("the preview must name the path")
	}
	if !strings.Contains(body, "Move") {
		t.Errorf("the preview must offer the confirm step:\n%s", body)
	}
	// And nothing has happened yet.
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("planning must not move anything")
	}
	if n := ts.queuedJobs(t, removal.JobType); n != 0 {
		t.Errorf("planning must not enqueue work, got %d job(s)", n)
	}
}

func TestRemovalPlanShowsRefusalsWithReasons(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	status, body := ts.postForm(t, "/removals/plan", url.Values{
		"root":   {"library"},
		"action": {"trash"},
		// The only copy of this content, plus a traversal attempt.
		"path": {"pack/unique.png", "../../../etc/passwd"},
	})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	if !strings.Contains(body, "Refused") {
		t.Fatalf("refusals must be shown:\n%s", body)
	}
	if !strings.Contains(body, "last remaining copy") {
		t.Error("the reason for refusing the only copy must be shown")
	}
	if !strings.Contains(body, "Nothing in that selection can be acted on") {
		t.Error("a fully refused selection must say so plainly")
	}
}

func TestRemovalApplyEnqueuesAndRedirectsToTheTrash(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	form := url.Values{
		"root":   {"library"},
		"action": {"trash"},
		"path":   {"pack/a/hero.png"},
		"reason": {"Exact duplicate"},
	}
	form.Set("csrf_token", ts.csrfToken(t, "/"))
	resp, err := ts.client.PostForm(ts.URL+"/removals/apply", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", resp.StatusCode)
	}
	if location := resp.Header.Get("Location"); !strings.HasPrefix(location, "/trash") {
		t.Errorf("location = %q, want the trash page", location)
	}
	// Invariant 8: the work is queued, not done inside the request.
	if n := ts.queuedJobs(t, removal.JobType); n != 1 {
		t.Fatalf("queued removal jobs = %d, want 1", n)
	}
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("the handler itself must not move files")
	}
}

func TestRemovalApplyRefusesAnEmptySelection(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	status, body := ts.postForm(t, "/removals/apply", url.Values{
		"root": {"library"}, "action": {"trash"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(body, "Nothing") && !strings.Contains(body, "nothing") {
		t.Errorf("the page should explain that nothing was selected:\n%s", body)
	}
	if n := ts.queuedJobs(t, removal.JobType); n != 0 {
		t.Errorf("nothing may be enqueued, got %d", n)
	}
}

func TestRemovalApplyRefusesWhenTheLibraryIsReadOnly(t *testing.T) {
	ts := newTestServerWithConfig(t, func(cfg *config.Config) { cfg.LibraryReadonly = true })
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	ts.seedDuplicates(t)

	status, body := ts.postForm(t, "/removals/apply", url.Values{
		"root": {"library"}, "action": {"trash"}, "path": {"pack/a/hero.png"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if !strings.Contains(body, "read-only") {
		t.Errorf("the refusal must say why:\n%s", body)
	}
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("the file must be untouched")
	}
	if n := ts.queuedJobs(t, removal.JobType); n != 0 {
		t.Errorf("nothing may be enqueued, got %d", n)
	}
}

func TestRemovalApplyRefusesEveryTargetItCannotAct(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	// The only copy: refused by invariant 4, so there is nothing left to apply.
	status, _ := ts.postForm(t, "/removals/apply", url.Values{
		"root": {"library"}, "action": {"trash"}, "path": {"pack/unique.png"},
	})
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if n := ts.queuedJobs(t, removal.JobType); n != 0 {
		t.Errorf("nothing may be enqueued, got %d", n)
	}
	if !ts.libraryFile(t, "pack/unique.png") {
		t.Error("the file must be untouched")
	}
}

func TestRemovalScriptIsDownloadable(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	form := url.Values{
		"root": {"library"}, "action": {"trash"},
		"path": {"pack/a/hero.png", "pack/.DS_Store"},
	}
	form.Set("csrf_token", ts.csrfToken(t, "/"))
	resp, err := ts.client.PostForm(ts.URL+"/removals/script", form)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "ambar-removal.sh") {
		t.Errorf("content-disposition = %q", cd)
	}
	body := ts.body(t, resp)
	if !strings.HasPrefix(body, "#!/bin/sh") {
		t.Errorf("not a script:\n%s", body)
	}
	if !strings.Contains(body, "mv -n --") {
		t.Error("the script should move files into the trash")
	}
	// Exporting is read-only.
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("exporting a script must not move anything")
	}
	if n := ts.queuedJobs(t, removal.JobType); n != 0 {
		t.Errorf("exporting must not enqueue work, got %d", n)
	}
}

func TestRemovalRoutesRequireCSRF(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	for _, path := range []string{"/removals/plan", "/removals/apply", "/removals/script",
		"/dupes/scan", "/trash/purge"} {
		resp, err := ts.client.PostForm(ts.URL+path, url.Values{
			"root": {"library"}, "action": {"trash"}, "path": {"pack/a/hero.png"},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
			t.Errorf("%s accepted a request with no CSRF token (status %d)", path, resp.StatusCode)
		}
	}
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("nothing may move without a CSRF token")
	}
}

func TestTrashPageAndPurgeWithoutRetention(t *testing.T) {
	ts := removalTestServer(t)

	resp := ts.get(t, "/trash")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if body := ts.body(t, resp); !strings.Contains(body, "The trash is empty") {
		t.Error("an empty trash should say so")
	}

	// With no retention configured, purging is refused rather than defaulting to
	// "everything" (§9.1).
	status, _ := ts.postForm(t, "/trash/purge", url.Values{})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", status)
	}
	body := ts.body(t, ts.get(t, "/trash"))
	if !strings.Contains(body, "AMBAR_TRASH_RETENTION") {
		t.Errorf("the refusal must explain itself:\n%s", body)
	}
}

func TestTrashPageListsAnAppliedBatch(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	// Apply a batch directly, the way the worker would.
	planner := removal.NewPlanner(ts.db, ts.cfg.LibraryRoot, ts.cfg.DataRoot, ts.cfg.TrashDir)
	plan, err := planner.Plan(context.Background(), "Exact duplicate", []removal.Target{
		{Root: removal.RootLibrary, Path: "pack/a/hero.png", Action: removal.ActionTrash,
			Finding: "dupes:exact:test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := removal.NewExecutor(ts.db, ts.cfg.LibraryRoot, ts.cfg.DataRoot, ts.cfg.TrashDir,
		ts.cfg.DedupeLinkMode, nil, nil)
	if _, err := exec.Apply(context.Background(), plan, removal.Actor{Username: testUsername}); err != nil {
		t.Fatal(err)
	}

	body := ts.body(t, ts.get(t, "/trash"))
	if !strings.Contains(body, "pack/a/hero.png") {
		t.Errorf("the trash page must list the removed path:\n%s", body)
	}
	if !strings.Contains(body, "Restore the ticked items") {
		t.Error("a restorable batch must offer a restore control")
	}
	if !strings.Contains(body, "dupes:exact:test") {
		t.Error("the finding that motivated the removal should be visible")
	}
}

func TestTrashRestorePutsFilesBack(t *testing.T) {
	ts := removalTestServer(t)
	ts.seedDuplicates(t)

	planner := removal.NewPlanner(ts.db, ts.cfg.LibraryRoot, ts.cfg.DataRoot, ts.cfg.TrashDir)
	plan, err := planner.Plan(context.Background(), "Exact duplicate", []removal.Target{
		{Root: removal.RootLibrary, Path: "pack/a/hero.png", Action: removal.ActionTrash},
	})
	if err != nil {
		t.Fatal(err)
	}
	exec := removal.NewExecutor(ts.db, ts.cfg.LibraryRoot, ts.cfg.DataRoot, ts.cfg.TrashDir,
		ts.cfg.DedupeLinkMode, nil, nil)
	result, err := exec.Apply(context.Background(), plan, removal.Actor{Username: testUsername})
	if err != nil {
		t.Fatal(err)
	}
	if ts.libraryFile(t, "pack/a/hero.png") {
		t.Fatal("the file should have moved into the trash")
	}

	status, _ := ts.postForm(t, "/trash/"+result.BatchID+"/restore", url.Values{
		"path": {"pack/a/hero.png"},
	})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", status)
	}
	if !ts.libraryFile(t, "pack/a/hero.png") {
		t.Error("the file must be back where it came from")
	}
}

func TestTrashRestoreRejectsAnUnknownBatch(t *testing.T) {
	ts := removalTestServer(t)

	// An id that looks like a batch but is not: handled, refused, and reported. The
	// message travels in the redirect rather than a session, so follow it.
	form := url.Values{}
	form.Set("csrf_token", ts.csrfToken(t, "/"))
	resp, err := ts.client.PostForm(ts.URL+"/trash/nope/restore", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status = %d, want a 303 carrying the error", resp.StatusCode)
	}
	body := ts.body(t, ts.get(t, resp.Header.Get("Location")))
	if !strings.Contains(body, "Restore failed") {
		t.Errorf("the failure must be reported:\n%s", body)
	}

	// A traversal never reaches the handler at all: net/http cleans the path and
	// redirects, so the id can only ever be one path segment (invariant 9).
	for _, id := range []string{"..", "../..", "%2e%2e%2f%2e%2e"} {
		status, _ := ts.postForm(t, "/trash/"+id+"/restore", url.Values{})
		if status == http.StatusOK {
			t.Errorf("batch id %q was accepted", id)
		}
	}
}

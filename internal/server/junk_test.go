package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/datcal/ambar/internal/junk"
)

func junkTestServer(t *testing.T) *testServer {
	t.Helper()
	ts := newTestServer(t)
	ts.createUser(t, testUsername, testPassword)
	ts.login(t, testUsername, testPassword)
	return ts
}

func (ts *testServer) queuedJunkJobs(t *testing.T) int {
	t.Helper()
	var n int
	if err := ts.db.Reader.QueryRowContext(context.Background(),
		`SELECT count(*) FROM jobs WHERE type = ? AND state = 'queued'`, junk.JobType).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestJunkPageEmptyBeforeScan(t *testing.T) {
	ts := junkTestServer(t)

	resp := ts.get(t, "/junk")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := ts.body(t, resp)
	if !strings.Contains(body, "No scan has run yet") {
		t.Error("a first visit should say no scan has run yet")
	}
	// M13 added the human-selected removal flow, but nothing may be pre-selected and
	// nothing may act directly: every path goes through the /removals/plan preview
	// (§9.1, invariant 3).
	if strings.Contains(body, "checked") {
		t.Error("no checkbox on the junk page may be pre-selected")
	}
	if strings.Contains(body, "/removals/apply") {
		t.Error("the junk page must not post straight to apply; the preview is not skippable")
	}
}

func TestJunkScanEnqueuesJob(t *testing.T) {
	ts := junkTestServer(t)

	status, _ := ts.postForm(t, "/junk/scan", url.Values{})
	if status != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", status)
	}
	if n := ts.queuedJunkJobs(t); n != 1 {
		t.Fatalf("queued junk jobs = %d, want 1", n)
	}

	// With the sweep queued (no worker runs in the test), the page reports it. M16 dropped the
	// "then reload" half of that sentence: the line is live now, fed by /api/v1/jobs/status.
	body := ts.body(t, ts.get(t, "/junk"))
	if !strings.Contains(body, "A sweep is running") {
		t.Error("the page should say a sweep is running while one is queued")
	}
	if strings.Contains(body, "then reload") {
		t.Error("the page still tells the user to reload")
	}
}

func TestJunkScanDeduplicates(t *testing.T) {
	ts := junkTestServer(t)
	ts.postForm(t, "/junk/scan", url.Values{})
	ts.postForm(t, "/junk/scan", url.Values{}) // second press coalesces

	if n := ts.queuedJunkJobs(t); n != 1 {
		t.Errorf("queued junk jobs = %d, want 1 (deduped)", n)
	}
}

func TestJunkReportRendered(t *testing.T) {
	ts := junkTestServer(t)

	report := &junk.Report{
		LibraryScanned: true,
		Findings: []junk.Finding{
			{
				Kind:       junk.KindMacOSX,
				TotalBytes: 4096,
				Items:      []junk.Item{{Path: "pack/__MACOSX", Bytes: 4096, Detail: "3 file(s)"}},
			},
			{
				Kind:       junk.KindOSJunk,
				TotalBytes: 12,
				Items:      []junk.Item{{Path: "pack/.DS_Store", Bytes: 12}},
			},
		},
	}
	if err := junk.WriteReport(ts.cfg.DataRoot, report, 1_700_000_000); err != nil {
		t.Fatal(err)
	}

	body := ts.body(t, ts.get(t, "/junk"))
	for _, want := range []string{
		"__MACOSX shadow trees",
		"pack/__MACOSX",
		"OS metadata files",
		"pack/.DS_Store",
		"2 candidate(s)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("junk page missing %q", want)
		}
	}
}

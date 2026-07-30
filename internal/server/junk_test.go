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
	// Reporting-only: there must be no removal action on the page in M12.
	if strings.Contains(body, "/junk/remove") || strings.Contains(strings.ToLower(body), ">delete<") {
		t.Error("the junk page must not offer a removal action in M12")
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

	// With the sweep queued (no worker runs in the test), the page reports it.
	if body := ts.body(t, ts.get(t, "/junk")); !strings.Contains(body, "A scan is running") {
		t.Error("the page should say a scan is running while one is queued")
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

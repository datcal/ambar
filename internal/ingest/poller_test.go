package ingest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
)

func newPollerFixture(t *testing.T) (*Poller, *db.DB, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "library")
	if err := os.MkdirAll(filepath.Join(root, InboxDir), 0o755); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(filepath.Join(base, "ambar.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := database.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	queue := jobs.New(database, jobs.Options{Workers: 1})
	return NewPoller(root, queue, time.Second, nil), database, root
}

func ingestJobs(t *testing.T, database *db.DB) []string {
	t.Helper()
	rows, err := database.Reader.Query(
		`SELECT payload_json FROM jobs WHERE type = ? ORDER BY id`, JobType)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func TestPollerEnqueuesOnlyWhenStable(t *testing.T) {
	p, database, root := newPollerFixture(t)
	ctx := context.Background()
	inbox := filepath.Join(root, InboxDir)
	writeZip(t, filepath.Join(inbox, "pack.zip"), map[string]string{"a.png": "a"})

	// First sighting: recorded, not yet enqueued.
	if n, err := p.pollOnce(ctx); err != nil || n != 0 {
		t.Fatalf("first poll = %d, %v; want 0", n, err)
	}
	if len(ingestJobs(t, database)) != 0 {
		t.Fatal("enqueued on first sighting")
	}

	// Second sighting unchanged: stable, enqueued once.
	if n, err := p.pollOnce(ctx); err != nil || n != 1 {
		t.Fatalf("second poll = %d, %v; want 1", n, err)
	}
	if len(ingestJobs(t, database)) != 1 {
		t.Fatalf("want one ingest job, got %d", len(ingestJobs(t, database)))
	}

	// Third poll: still present, but dedup keeps it at one queued job.
	if _, err := p.pollOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if len(ingestJobs(t, database)) != 1 {
		t.Errorf("dedup failed: %d jobs", len(ingestJobs(t, database)))
	}
}

func TestPollerWaitsForFileToSettle(t *testing.T) {
	p, database, root := newPollerFixture(t)
	ctx := context.Background()
	inbox := filepath.Join(root, InboxDir)
	path := filepath.Join(inbox, "growing.zip")
	writeZip(t, path, map[string]string{"a.png": "a"})

	p.pollOnce(ctx) // first sighting

	// The file changes (still copying): a new mtime/size means "not settled".
	if err := os.WriteFile(path, []byte("still arriving, not a valid zip yet, but larger"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, _ := p.pollOnce(ctx); n != 0 {
		t.Errorf("enqueued a file that changed between polls")
	}
	// Now it settles.
	if n, _ := p.pollOnce(ctx); n != 1 {
		t.Errorf("did not enqueue after the file settled")
	}
	_ = database
}

func TestPollerIgnoresNonArchives(t *testing.T) {
	p, database, root := newPollerFixture(t)
	ctx := context.Background()
	inbox := filepath.Join(root, InboxDir)
	os.WriteFile(filepath.Join(inbox, "notes.txt"), []byte("hi"), 0o644)
	os.WriteFile(filepath.Join(inbox, "pack.zip.url"), []byte("URL=https://x"), 0o644)

	p.pollOnce(ctx)
	p.pollOnce(ctx)
	if len(ingestJobs(t, database)) != 0 {
		t.Errorf("enqueued a non-archive")
	}
}

func TestPollerSniffsSourceURL(t *testing.T) {
	p, database, root := newPollerFixture(t)
	ctx := context.Background()
	inbox := filepath.Join(root, InboxDir)
	writeZip(t, filepath.Join(inbox, "pack.zip"), map[string]string{"a.png": "a"})
	os.WriteFile(filepath.Join(inbox, "pack.zip.url"),
		[]byte("[InternetShortcut]\nURL=https://kenney.itch.io/pack\n"), 0o644)

	p.pollOnce(ctx)
	p.pollOnce(ctx)
	jobsList := ingestJobs(t, database)
	if len(jobsList) != 1 {
		t.Fatalf("want 1 job, got %d", len(jobsList))
	}
	if !strings.Contains(jobsList[0], "kenney.itch.io/pack") {
		t.Errorf("source URL not sniffed into payload: %s", jobsList[0])
	}
}

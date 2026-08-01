package ingest

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/datcal/ambar/internal/db"
)

func newIngester(t *testing.T, keepArchives, readonly bool) (*Ingester, *db.DB, string) {
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
	ig := New(database, Options{
		LibraryRoot: root, KeepArchives: keepArchives, Readonly: readonly,
	}).WithClock(func() time.Time { return time.Unix(1_700_000_000, 0) })
	return ig, database, root
}

func writeZip(t *testing.T, path string, names map[string]string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	for name, data := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(data))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestIngestExtractsAndRecordsProvenance(t *testing.T) {
	ig, database, root := newIngester(t, true, false)
	ctx := context.Background()
	writeZip(t, filepath.Join(root, InboxDir, "foo.zip"), map[string]string{
		"pack/a.png":     "a",
		"pack/sub/b.png": "b",
	})

	res, err := ig.Ingest(ctx, InboxDir+"/foo.zip", "https://kenney.itch.io/foo", "")
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Quarantined {
		t.Fatalf("unexpected quarantine: %s", res.QuarantineReason)
	}
	if res.PackRelPath != "foo" {
		t.Errorf("pack rel path = %q, want foo", res.PackRelPath)
	}
	if res.Flattened != "pack" || res.FilesWritten != 2 {
		t.Errorf("result = %+v", res)
	}

	// Files extracted, wrapper folder flattened.
	for _, rel := range []string{"foo/a.png", "foo/sub/b.png"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Errorf("missing %s: %v", rel, err)
		}
	}

	// Pack row carries provenance and starts needing it.
	var (
		srcURL, archiveName, state, kind string
		size                             int64
	)
	err = database.Reader.QueryRow(`
		SELECT source_url, original_archive_name, original_archive_size, provenance_state, kind
		FROM packs WHERE library_rel_path = 'foo'`).
		Scan(&srcURL, &archiveName, &size, &state, &kind)
	if err != nil {
		t.Fatalf("read pack: %v", err)
	}
	if srcURL != "https://kenney.itch.io/foo" || archiveName != "foo.zip" {
		t.Errorf("provenance not recorded: url=%q archive=%q", srcURL, archiveName)
	}
	if state != "needs_provenance" || kind != "archive" {
		t.Errorf("state=%q kind=%q", state, kind)
	}
	if size == 0 {
		t.Errorf("archive size not recorded")
	}

	// Original archive retained in _archives, gone from _inbox.
	if _, err := os.Stat(filepath.Join(root, InboxDir, "foo.zip")); !os.IsNotExist(err) {
		t.Errorf("archive still in _inbox")
	}
	if _, err := os.Stat(filepath.Join(root, ArchivesDir, "foo.zip")); err != nil {
		t.Errorf("archive not retained in _archives: %v", err)
	}
}

func TestIngestQuarantinesHostileArchive(t *testing.T) {
	ig, database, root := newIngester(t, true, false)
	ctx := context.Background()
	writeZip(t, filepath.Join(root, InboxDir, "bad.zip"), map[string]string{
		"../evil.txt": "pwned",
	})

	res, err := ig.Ingest(ctx, InboxDir+"/bad.zip", "", "")
	if err != nil {
		t.Fatalf("ingest returned error instead of quarantining: %v", err)
	}
	if !res.Quarantined {
		t.Fatal("hostile archive was not quarantined")
	}

	// Moved to _quarantine with an error log; no pack created; nothing escaped.
	if _, err := os.Stat(filepath.Join(root, QuarantineDir, "bad.zip")); err != nil {
		t.Errorf("archive not in _quarantine: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, QuarantineDir, "bad.zip.error.txt")); err != nil {
		t.Errorf("no quarantine error log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root), "evil.txt")); !os.IsNotExist(err) {
		t.Errorf("traversal escaped during ingest")
	}
	var n int
	database.Reader.QueryRow(`SELECT count(*) FROM packs`).Scan(&n)
	if n != 0 {
		t.Errorf("a pack was created for a quarantined archive")
	}
}

func TestIngestNotKeepingArchivesRemovesInput(t *testing.T) {
	ig, _, root := newIngester(t, false, false)
	ctx := context.Background()
	writeZip(t, filepath.Join(root, InboxDir, "foo.zip"), map[string]string{"x/y.png": "y"})

	if _, err := ig.Ingest(ctx, InboxDir+"/foo.zip", "", ""); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, InboxDir, "foo.zip")); !os.IsNotExist(err) {
		t.Errorf("archive should have been removed when not keeping archives")
	}
	if _, err := os.Stat(filepath.Join(root, ArchivesDir)); err == nil {
		t.Errorf("_archives should not have been created")
	}
}

func TestIngestRefusedWhenReadonly(t *testing.T) {
	ig, _, root := newIngester(t, true, true)
	writeZip(t, filepath.Join(root, InboxDir, "foo.zip"), map[string]string{"a.png": "a"})
	_, err := ig.Ingest(context.Background(), InboxDir+"/foo.zip", "", "")
	if !errors.Is(err, ErrReadonly) {
		t.Errorf("err = %v, want ErrReadonly", err)
	}
}

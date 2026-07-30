package removal

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// trashed is the absolute path a library file ends up at inside a batch.
func (f *fixture) trashed(batchID, root, relPath string) string {
	return filepath.Join(f.trashDir, batchID, root, filepath.FromSlash(relPath))
}

func (f *fixture) apply(p *Plan) *Result {
	f.t.Helper()
	res, err := f.exec.Apply(context.Background(), p, Actor{Username: "burak", IP: "10.0.0.1"})
	if err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return res
}

func TestApplyMovesToTrashPreservingRelativePath(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("sprites/hero.png", "same-bytes")
	keptID := f.writeAsset("sprites/hero-copy.png", "same-bytes")

	res := f.apply(f.plan(trashTarget(packPrefix + "/sprites/hero.png")))

	if res.Applied != 1 || res.Failed != 0 {
		t.Fatalf("applied=%d failed=%d, want 1/0", res.Applied, res.Failed)
	}
	// Gone from the library...
	if f.exists(packPrefix + "/sprites/hero.png") {
		t.Error("the original must no longer be in the library")
	}
	// ...and sitting in the trash under its original relative path, so a restore is
	// unambiguous (§9.1).
	dest := f.trashed(res.BatchID, "library", packPrefix+"/sprites/hero.png")
	content, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("expected the file in the trash at %s: %v", dest, err)
	}
	if string(content) != "same-bytes" {
		t.Errorf("trashed content = %q", content)
	}
	// The other copy is untouched — invariant 1 for everything not selected.
	if !f.exists(packPrefix + "/sprites/hero-copy.png") {
		t.Error("an unselected file must never be touched")
	}
	if f.missingSince(keptID) != nil {
		t.Error("the kept copy must not be marked missing")
	}
	if at := f.missingSince(id); at == nil || *at != f.now.Unix() {
		t.Errorf("the trashed asset must be marked missing, got %v", at)
	}
}

func TestApplyWritesAReadableManifest(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix + "/a.png"))
	plan.Reason = "duplicates: identical content"
	res := f.apply(plan)

	batch, err := f.exec.Batch(res.BatchID)
	if err != nil {
		t.Fatalf("read batch: %v", err)
	}
	if batch.State != BatchApplied {
		t.Errorf("state = %q, want %q", batch.State, BatchApplied)
	}
	if batch.Reason != "duplicates: identical content" || batch.Actor != "burak" {
		t.Errorf("the record must say why and who: %+v", batch)
	}
	if len(batch.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(batch.Entries))
	}
	entry := batch.Entries[0]
	if entry.Path != packPrefix+"/a.png" || entry.Root != RootLibrary {
		t.Errorf("entry must record where it came from: %+v", entry)
	}
	if entry.Finding != "test" {
		t.Errorf("entry must record the finding that motivated it, got %q", entry.Finding)
	}
	if entry.TrashPath != "library/"+packPrefix+"/a.png" {
		t.Errorf("trash path = %q", entry.TrashPath)
	}
	if batch.Bytes() != int64(len("same-bytes")) {
		t.Errorf("batch bytes = %d", batch.Bytes())
	}

	// And it is listed.
	batches, err := f.exec.ListBatches()
	if err != nil || len(batches) != 1 || batches[0].ID != res.BatchID {
		t.Errorf("ListBatches = %+v, %v", batches, err)
	}
}

func TestApplyContinuesAfterOneFailedEntry(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.writeAsset("c.png", "same-bytes")

	plan := f.plan(trashTarget(packPrefix+"/a.png"), trashTarget(packPrefix+"/b.png"))
	// The file disappears between planning and applying — a rescan, another process,
	// or a NAS share that blinked.
	if err := os.Remove(filepath.Join(f.libraryRoot, packPrefix, "a.png")); err != nil {
		t.Fatal(err)
	}

	res := f.apply(plan)
	if res.Applied != 1 || res.Failed != 1 {
		t.Fatalf("applied=%d failed=%d, want 1/1", res.Applied, res.Failed)
	}
	if f.exists(packPrefix + "/b.png") {
		t.Error("the second entry should still have been applied")
	}

	batch, err := f.exec.Batch(res.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	var failed int
	for _, e := range batch.Entries {
		if !e.Done() {
			failed++
			if e.Error == "" {
				t.Error("a failed entry must record why")
			}
		}
	}
	if failed != 1 || batch.Failed() != 1 {
		t.Errorf("the manifest must show the failure: %+v", batch.Entries)
	}
}

func TestApplyRefusesAnEmptyPlan(t *testing.T) {
	f := newFixture(t)
	if _, err := f.exec.Apply(context.Background(), &Plan{}, Actor{}); err == nil {
		t.Error("an empty plan must be an error, not a no-op batch")
	}
	if _, err := f.exec.Apply(context.Background(), nil, Actor{}); err == nil {
		t.Error("a nil plan must be an error")
	}
}

func TestApplyMovesDataRootOrphansUnderTheirOwnRoot(t *testing.T) {
	f := newFixture(t)
	sha := strings.Repeat("a", 64)
	orphan := filepath.Join(f.dataRoot, "derivatives", "ab", sha)
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "thumb.webp"), []byte("thumb"), 0o644); err != nil {
		t.Fatal(err)
	}

	res := f.apply(f.plan(Target{Root: RootData, Path: "derivatives/ab/" + sha, Action: ActionTrash}))

	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Error("the orphan directory should have moved")
	}
	moved := f.trashed(res.BatchID, "data", "derivatives/ab/"+sha+"/thumb.webp")
	if _, err := os.Stat(moved); err != nil {
		t.Errorf("expected the tree in the trash at %s: %v", moved, err)
	}
}

// --- restore ----------------------------------------------------------------

func TestRestorePutsFilesBack(t *testing.T) {
	f := newFixture(t)
	id := f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")

	res := f.apply(f.plan(trashTarget(packPrefix + "/hero.png")))

	restored, failures, err := f.exec.Restore(context.Background(), res.BatchID, nil, Actor{Username: "burak"})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 1 || len(failures) != 0 {
		t.Fatalf("restored=%d failures=%v", restored, failures)
	}
	if !f.exists(packPrefix + "/hero.png") {
		t.Error("the file must be back where it came from")
	}
	if at := f.missingSince(id); at != nil {
		t.Errorf("missing_since must be cleared on restore, got %v", *at)
	}

	// The manifest records it, so a second restore does nothing rather than failing.
	batch, err := f.exec.Batch(res.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Entries[0].Restored() || batch.Restorable() != 0 {
		t.Errorf("manifest should mark the entry restored: %+v", batch.Entries[0])
	}
	again, _, err := f.exec.Restore(context.Background(), res.BatchID, nil, Actor{})
	if err != nil || again != 0 {
		t.Errorf("second restore = %d, %v; want 0, nil", again, err)
	}
}

func TestRestoreSelectsIndividualPaths(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.writeAsset("c.png", "same-bytes")

	res := f.apply(f.plan(trashTarget(packPrefix+"/a.png"), trashTarget(packPrefix+"/b.png")))

	restored, _, err := f.exec.Restore(context.Background(), res.BatchID, []string{packPrefix + "/a.png"}, Actor{})
	if err != nil {
		t.Fatal(err)
	}
	if restored != 1 {
		t.Fatalf("restored = %d, want 1", restored)
	}
	if !f.exists(packPrefix+"/a.png") || f.exists(packPrefix+"/b.png") {
		t.Error("only the selected path should have come back")
	}
}

func TestRestoreNeverOverwrites(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")
	res := f.apply(f.plan(trashTarget(packPrefix + "/hero.png")))

	// Something new occupies the original path.
	f.writeFile(packPrefix+"/hero.png", "a different file entirely")

	restored, failures, err := f.exec.Restore(context.Background(), res.BatchID, nil, Actor{})
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if restored != 0 || len(failures) != 1 {
		t.Fatalf("restored=%d failures=%v; want 0 and one refusal", restored, failures)
	}
	content, err := os.ReadFile(filepath.Join(f.libraryRoot, packPrefix, "hero.png"))
	if err != nil || string(content) != "a different file entirely" {
		t.Errorf("the occupying file must survive untouched, got %q / %v", content, err)
	}
	// And the trashed copy is still recoverable.
	if _, err := os.Stat(f.trashed(res.BatchID, "library", packPrefix+"/hero.png")); err != nil {
		t.Errorf("the trashed copy must still be there: %v", err)
	}
}

func TestRestoreRejectsATamperedManifest(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("hero.png", "same-bytes")
	f.writeAsset("hero-copy.png", "same-bytes")
	res := f.apply(f.plan(trashTarget(packPrefix + "/hero.png")))

	manifestPath := filepath.Join(f.trashDir, res.BatchID, ManifestName)
	// The manifest is a file on disk. Someone editing it must not be able to write
	// outside the library (invariant 9) — the trash record is input, not authority.
	for _, tamper := range []struct {
		name  string
		apply func(*Batch)
	}{
		{"escaping original path", func(b *Batch) { b.Entries[0].Path = "../../../etc/ambar-owned.txt" }},
		{"escaping trash path", func(b *Batch) { b.Entries[0].TrashPath = "../../../../etc/passwd" }},
		{"absolute original path", func(b *Batch) { b.Entries[0].Path = "/etc/ambar-owned.txt" }},
	} {
		t.Run(tamper.name, func(t *testing.T) {
			batch, err := f.exec.Batch(res.BatchID)
			if err != nil {
				t.Fatal(err)
			}
			batch.Entries[0].RestoredAt = 0
			tamper.apply(batch)
			data, err := json.Marshal(batch)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
				t.Fatal(err)
			}

			restored, failures, err := f.exec.Restore(context.Background(), res.BatchID, nil, Actor{})
			if err == nil && restored == 0 && len(failures) == 0 {
				t.Fatal("a tampered entry must be refused or reported, not silently skipped")
			}
			if restored != 0 {
				t.Fatalf("nothing may be restored from a tampered manifest, got %d", restored)
			}
		})
	}
}

func TestBatchIDsFromAURLAreValidated(t *testing.T) {
	f := newFixture(t)
	for _, id := range []string{"", ".", "..", "../..", "sub/dir", `back\slash`, "nope"} {
		if _, err := f.exec.Batch(id); err == nil {
			t.Errorf("batch id %q must be rejected", id)
		}
	}
}

// --- purge ------------------------------------------------------------------

func TestPurgeOnlyTakesBatchesOlderThanTheCutoff(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("old.png", "same-bytes")
	f.writeAsset("new.png", "same-bytes")
	f.writeAsset("keep.png", "same-bytes")

	oldBatch := f.apply(f.plan(trashTarget(packPrefix + "/old.png")))
	f.now = f.now.Add(48 * time.Hour)
	newBatch := f.apply(f.plan(trashTarget(packPrefix + "/new.png")))

	cutoff := f.now.Add(-24 * time.Hour)
	report, err := f.exec.Purge(context.Background(), cutoff, Actor{})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if len(report.Batches) != 1 || report.Batches[0] != oldBatch.BatchID {
		t.Fatalf("purged %v, want just %s", report.Batches, oldBatch.BatchID)
	}
	if report.Kept != 1 {
		t.Errorf("kept = %d, want 1", report.Kept)
	}
	if report.Bytes != int64(len("same-bytes")) {
		t.Errorf("bytes = %d", report.Bytes)
	}
	if _, err := os.Stat(filepath.Join(f.trashDir, oldBatch.BatchID)); !os.IsNotExist(err) {
		t.Error("the old batch should be gone")
	}
	// Nothing younger than the retention window, whatever the pressure (§9.1).
	if _, err := os.Stat(f.trashed(newBatch.BatchID, "library", packPrefix+"/new.png")); err != nil {
		t.Errorf("the young batch must survive: %v", err)
	}
}

func TestPurgeRefusesWithoutACutoff(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	res := f.apply(f.plan(trashTarget(packPrefix + "/a.png")))

	if _, err := f.exec.Purge(context.Background(), time.Time{}, Actor{}); err == nil {
		t.Fatal(`"no retention configured" must never mean "purge everything"`)
	}
	if _, err := os.Stat(filepath.Join(f.trashDir, res.BatchID)); err != nil {
		t.Errorf("the batch must still be there: %v", err)
	}
}

// --- linking ----------------------------------------------------------------

func TestLinkSharesContentAndKeepsBothPaths(t *testing.T) {
	f := newFixture(t)
	redundantID := f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")

	plan := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png",
		Action: ActionLink, KeepPath: packPrefix + "/b.png", Finding: "dupes:exact"})
	res := f.apply(plan)
	if res.Applied != 1 {
		batch, _ := f.exec.Batch(res.BatchID)
		t.Fatalf("applied=%d failed=%d: %+v", res.Applied, res.Failed, batch.Entries)
	}

	// Both paths still work, with the same content — §9.1's whole point.
	for _, p := range []string{packPrefix + "/a.png", packPrefix + "/b.png"} {
		content, err := os.ReadFile(filepath.Join(f.libraryRoot, filepath.FromSlash(p)))
		if err != nil || string(content) != "same-bytes" {
			t.Fatalf("%s: %q / %v", p, content, err)
		}
	}
	// hardlink mode: genuinely one inode.
	aInfo, err := os.Stat(filepath.Join(f.libraryRoot, packPrefix, "a.png"))
	if err != nil {
		t.Fatal(err)
	}
	bInfo, err := os.Stat(filepath.Join(f.libraryRoot, packPrefix, "b.png"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(aInfo, bInfo) {
		t.Error("hardlink mode should leave the two paths sharing one inode")
	}
	// Nothing was removed, so nothing is missing.
	if at := f.missingSince(redundantID); at != nil {
		t.Errorf("a linked asset is still present; missing_since = %v", *at)
	}
}

func TestLinkRefusesWhenTheBytesDivergedSinceTheScan(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")

	plan := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png",
		Action: ActionLink, KeepPath: packPrefix + "/b.png"})

	// The index still says the two are identical, but the file changed underneath —
	// linking now would replace one file's content with another's.
	f.writeFile(packPrefix+"/a.png", "edited since the scan")

	res := f.apply(plan)
	if res.Applied != 0 || res.Failed != 1 {
		t.Fatalf("applied=%d failed=%d, want 0/1", res.Applied, res.Failed)
	}
	batch, err := f.exec.Batch(res.BatchID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(batch.Entries[0].Error, "differ") {
		t.Errorf("error should explain the divergence, got %q", batch.Entries[0].Error)
	}
	content, err := os.ReadFile(filepath.Join(f.libraryRoot, packPrefix, "a.png"))
	if err != nil || string(content) != "edited since the scan" {
		t.Errorf("the edited file must be untouched, got %q / %v", content, err)
	}
}

func TestLinkModeOffRefusesToLink(t *testing.T) {
	f := newFixture(t)
	f.writeAsset("a.png", "same-bytes")
	f.writeAsset("b.png", "same-bytes")
	f.exec = NewExecutor(f.db, f.libraryRoot, f.dataRoot, f.trashDir, "off", nil, nil).
		WithClock(func() time.Time { return f.now })

	plan := f.plan(Target{Root: RootLibrary, Path: packPrefix + "/a.png",
		Action: ActionLink, KeepPath: packPrefix + "/b.png"})
	res := f.apply(plan)

	if res.Failed != 1 {
		t.Fatalf("linking with mode=off must fail, got %+v", res)
	}
	if !f.exists(packPrefix + "/a.png") {
		t.Error("the file must be untouched")
	}
}

func TestProbeLinkSupport(t *testing.T) {
	dir := t.TempDir()

	if got := ProbeLinkSupport("hardlink", dir); !got.OK {
		t.Errorf("hardlinks should work in a temp dir: %+v", got)
	}
	if got := ProbeLinkSupport("off", dir); got.OK || got.Detail == "" {
		t.Errorf("off must report not-ok with an explanation: %+v", got)
	}
	if got := ProbeLinkSupport("nonsense", dir); got.OK || !strings.Contains(got.Detail, "nonsense") {
		t.Errorf("an unknown mode must be named: %+v", got)
	}
	// reflink depends on the filesystem, so only the shape of the answer is
	// asserted: either it works, or it says why not.
	if got := ProbeLinkSupport("reflink", dir); !got.OK && got.Detail == "" {
		t.Error("a failed reflink probe must explain itself")
	}
	// The probe leaves nothing behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("probe left files behind: %v", entries)
	}
}

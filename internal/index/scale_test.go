package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// §16: "The UI must stay usable at 20,000 assets. Generate a library that size and
// test against it rather than discovering the problem in production."
const scaleAssets = 20_000

// queryBudget is generous on purpose. The point is not to benchmark the machine
// but to catch an algorithmic regression — an OFFSET pagination, a missing index,
// a per-row query — which shows up as hundreds of milliseconds or seconds, not as
// a few extra microseconds.
const queryBudget = 500 * time.Millisecond

// buildScaleLibrary writes a synthetic library shaped like the real one: packs under
// buckets, format-variant subfolders, and a mix of kinds.
//
// Three quarters of the files are variant triples — the same base name in PNG/, PSD/
// and ASEPRITE/, exactly the craftpix shape §5.1 describes — and the rest stand alone.
// That matters: an earlier version gave every file a unique name, so nothing collapsed
// and the group queries were measured against a workload that cannot occur.
func buildScaleLibrary(t *testing.T, root string, count int) {
	t.Helper()

	const packs = 40
	bucketNames := []string{"2d", "3d", "mix", "audio"}

	// The three format folders of one artwork, and the extension each holds.
	variants := []struct{ folder, ext string }{
		{"PNG", "png"},
		{"PSD", "psd"},
		{"ASEPRITE", "aseprite"},
	}
	soloExts := []string{"glb", "wav", "tmx", "ogg"}

	// One MkdirAll per directory rather than per file.
	made := map[string]bool{}
	write := func(dir, name, content string) {
		t.Helper()
		if !made[dir] {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			made[dir] = true
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < count; i++ {
		// Bucket and pack derive from the ARTWORK index, not the file index, so all
		// variants of one artwork land in the same pack. Group keys are per-pack, so
		// deriving them from i scattered every triple across three packs and nothing
		// grouped — a fixture bug that made the group queries look untested.
		artwork := i / 4
		bucket := bucketNames[artwork%len(bucketNames)]
		pack := fmt.Sprintf("vendor-pack-%03d", artwork%packs)
		packDir := filepath.Join(root, bucket, pack)

		// Content varies per file so every hash differs, which is the realistic case
		// for move detection and for M13's duplicate finder.
		content := fmt.Sprintf("asset number %d padding padding padding", i)

		if i%4 == 3 {
			write(packDir, fmt.Sprintf("solo_%06d.%s", i, soloExts[i%len(soloExts)]), content)
			continue
		}
		v := variants[i%4]
		write(filepath.Join(packDir, v.folder), fmt.Sprintf("art_%06d.%s", i/4, v.ext), content)
	}
}

// TestScaleTwentyThousandAssets is the §16 requirement. It is the test that would
// catch an OFFSET-based pagination or a missing index before the NAS does.
func TestScaleTwentyThousandAssets(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a 20,000-file library; skipped under -short")
	}
	// The race detector multiplies runtime by roughly 30x — the initial scan goes
	// from under 2 seconds to nearly a minute — so every absolute budget below
	// would fail for reasons that have nothing to do with the code. This is a
	// performance test; `make test` runs it uninstrumented, which is where §16's
	// requirement is actually enforced.
	if raceEnabled {
		t.Skip("performance budgets are not meaningful under -race; run via `make test`")
	}

	f := newFixture(t)
	buildScaleLibrary(t, f.root, scaleAssets)

	// --- the scan itself ---

	start := time.Now()
	report, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	scanDuration := time.Since(start)

	if report.Added != scaleAssets {
		t.Fatalf("Added = %d, want %d", report.Added, scaleAssets)
	}
	if len(report.Errors) != 0 {
		t.Errorf("scan errors: %v", report.Errors[:min(3, len(report.Errors))])
	}
	t.Logf("first scan: %d assets in %s (%d packs)", report.Added, scanDuration.Round(time.Millisecond),
		report.PacksFound)

	// --- the rescan, which must not re-read the library ---

	start = time.Now()
	rescan, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("rescan: %v", err)
	}
	rescanDuration := time.Since(start)

	if rescan.Unchanged != scaleAssets {
		t.Errorf("Unchanged = %d, want %d", rescan.Unchanged, scaleAssets)
	}
	if rescan.Hashed != 0 {
		t.Errorf("the rescan hashed %d files; the (size, mtime) fast path is broken", rescan.Hashed)
	}
	t.Logf("rescan: %s (nothing re-hashed)", rescanDuration.Round(time.Millisecond))

	// A rescan reads no file contents, so it must be much cheaper than the first
	// scan. Comparing against the first scan rather than a fixed number keeps this
	// meaningful on a slow disk as well as a fast one.
	if rescanDuration > scanDuration {
		t.Errorf("the rescan (%s) was slower than the initial scan (%s), so the fast path is not working",
			rescanDuration.Round(time.Millisecond), scanDuration.Round(time.Millisecond))
	}

	// --- the queries the grid makes ---

	ctx := context.Background()

	timed := func(name string, fn func() error) time.Duration {
		t.Helper()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d := time.Since(start)
		t.Logf("%s: %s", name, d.Round(time.Microsecond))
		if d > queryBudget {
			t.Errorf("%s took %s, over the %s budget", name, d.Round(time.Millisecond), queryBudget)
		}
		return d
	}

	var firstPage *Page
	firstDuration := timed("first page", func() error {
		var err error
		firstPage, err = f.ix.List(ctx, ListOptions{})
		return err
	})
	if firstPage.Total != scaleAssets {
		t.Errorf("Total = %d, want %d", firstPage.Total, scaleAssets)
	}

	timed("search", func() error {
		page, err := f.ix.List(ctx, ListOptions{Query: "art_000042"})
		if err != nil {
			return err
		}
		if page.Total == 0 {
			return fmt.Errorf("the search found nothing")
		}
		return nil
	})

	timed("prefix search matching many rows", func() error {
		_, err := f.ix.List(ctx, ListOptions{Query: "art"})
		return err
	})

	timed("kind filter", func() error {
		_, err := f.ix.List(ctx, ListOptions{Kind: "model"})
		return err
	})

	timed("stats", func() error {
		_, err := f.ix.Stats(ctx)
		return err
	})

	// --- the OFFSET regression check ---
	//
	// Walk deep into the result set and time a page there. With keyset pagination
	// a deep page costs the same as the first; with OFFSET it degrades linearly,
	// which is exactly the failure §8 warns about at 20k rows.
	cursor := firstPage.NextCursor
	const deepPages = 100
	for i := 0; i < deepPages && cursor != ""; i++ {
		page, err := f.ix.List(ctx, ListOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("paging to depth %d: %v", i, err)
		}
		cursor = page.NextCursor
	}
	if cursor == "" {
		t.Fatalf("ran out of pages before depth %d", deepPages)
	}

	deepDuration := timed(fmt.Sprintf("page at depth %d", deepPages), func() error {
		_, err := f.ix.List(ctx, ListOptions{Cursor: cursor})
		return err
	})

	// Allow generous slack for scheduling noise on a small absolute duration, but
	// an order of magnitude means the pagination is walking rows it should skip.
	if firstDuration > 0 && deepDuration > 10*firstDuration && deepDuration > 20*time.Millisecond {
		t.Errorf("a page at depth %d took %s versus %s for the first page; "+
			"pagination appears to be OFFSET-based rather than keyset",
			deepPages, deepDuration.Round(time.Millisecond), firstDuration.Round(time.Millisecond))
	}
}

// TestScaleMoveDetectionDoesNotDegrade checks the phase-3 hash matching at size:
// it must be a map lookup, not a scan per absent row.
func TestScaleMoveDetectionDoesNotDegrade(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large library; skipped under -short")
	}
	if raceEnabled {
		t.Skip("hashing 5,000 files under -race takes minutes; run via `make test`")
	}

	const count = 5_000

	f := newFixture(t)
	buildScaleLibrary(t, f.root, count)
	f.scan()

	// Move a whole bucket, which relocates a quarter of the library at once — what
	// reorganising over SMB actually looks like.
	if err := os.Rename(filepath.Join(f.root, "2d"), filepath.Join(f.root, "two-dee")); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	report, err := f.ix.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatalf("scan after a bulk move: %v", err)
	}
	t.Logf("bulk move of %d assets reconciled in %s", report.Moved, time.Since(start).Round(time.Millisecond))

	if report.Moved == 0 {
		t.Fatal("a bulk directory rename produced no moves")
	}
	if report.Added != 0 {
		t.Errorf("Added = %d after a rename; those should all be moves (§9.1 rule 2)", report.Added)
	}
	if report.MarkedMissing != 0 {
		t.Errorf("MarkedMissing = %d after a rename, want 0", report.MarkedMissing)
	}

	// The row count is unchanged: no duplicates were created.
	if got := f.assetCount(); got != count {
		t.Errorf("%d rows after the move, want %d", got, count)
	}
}

// TestScaleGroupQueriesStayFlat extends the §16 requirement to the group-based grid
// that M2 introduced.
//
// The group query joins three tables and, when filtered, runs an EXISTS subquery per
// row — so it deserves its own check rather than inheriting confidence from the asset
// query.
func TestScaleGroupQueriesStayFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a large library; skipped under -short")
	}
	if raceEnabled {
		t.Skip("performance budgets are not meaningful under -race; run via `make test`")
	}

	const count = 12_000

	f := newFixture(t)
	buildScaleLibrary(t, f.root, count)
	f.scan()

	ctx := context.Background()

	timed := func(name string, fn func() error) time.Duration {
		t.Helper()
		start := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		d := time.Since(start)
		t.Logf("%-34s %s", name, d.Round(time.Microsecond))
		if d > queryBudget {
			t.Errorf("%s took %s, over the %s budget", name, d.Round(time.Millisecond), queryBudget)
		}
		return d
	}

	var first *GroupPage
	firstDuration := timed("group grid, first page", func() error {
		var err error
		first, err = f.ix.ListGroups(ctx, ListOptions{})
		return err
	})
	if first.Total == 0 {
		t.Fatal("no groups")
	}
	// The fixture writes PNG/PSD/ASEPRITE variants of shared names, so grouping should
	// have collapsed a meaningful fraction.
	t.Logf("%d files collapsed into %d groups", count, first.Total)
	// Three of every four files are variant triples, so the collapse should be close
	// to 2:1. Asserting a ratio rather than an exact number keeps this robust to
	// fixture tweaks while still catching grouping being switched off.
	if first.Total >= count*3/4 {
		t.Errorf("%d groups from %d files — far too little was collapsed", first.Total, count)
	}

	timed("group grid, search", func() error {
		_, err := f.ix.ListGroups(ctx, ListOptions{Query: "art"})
		return err
	})
	timed("group grid, kind filter (EXISTS)", func() error {
		_, err := f.ix.ListGroups(ctx, ListOptions{Kind: "model"})
		return err
	})

	// Page deep and confirm it costs the same as the first page.
	cursor := first.NextCursor
	const deepPages = 50
	for i := 0; i < deepPages && cursor != ""; i++ {
		page, err := f.ix.ListGroups(ctx, ListOptions{Cursor: cursor})
		if err != nil {
			t.Fatalf("paging to depth %d: %v", i, err)
		}
		cursor = page.NextCursor
	}
	if cursor == "" {
		t.Skip("not enough groups to page that deep")
	}

	deepDuration := timed(fmt.Sprintf("group grid, page at depth %d", deepPages), func() error {
		_, err := f.ix.ListGroups(ctx, ListOptions{Cursor: cursor})
		return err
	})

	if firstDuration > 0 && deepDuration > 10*firstDuration && deepDuration > 20*time.Millisecond {
		t.Errorf("a group page at depth %d took %s versus %s for the first; "+
			"pagination is not keyset", deepPages, deepDuration.Round(time.Millisecond),
			firstDuration.Round(time.Millisecond))
	}

	// Regrouping the whole library must also stay cheap, since it runs on every scan.
	timed("regroup the whole library", func() error {
		_, err := f.ix.Regroup(ctx)
		return err
	})
}

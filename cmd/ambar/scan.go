package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/sidecar"
)

const scanUsage = `Usage:
  ambar scan [--dry-run] [--no-dimensions] [--verbose]

Walks AMBAR_LIBRARY_ROOT, detects packs, and reconciles the index.

Safe to re-run: unchanged files are skipped without being re-read, a file that has
moved is recognised as a move rather than a new asset, and a file that has gone
missing is marked, never deleted.
`

func runScan(args []string) error {
	fs := flag.NewFlagSet("ambar scan", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false,
		"report what would change without writing anything")
	noDimensions := fs.Bool("no-dimensions", false,
		"skip reading image headers for width and height")
	verbose := fs.Bool("verbose", false,
		"list detected packs and every per-file error")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, scanUsage)
		return fmt.Errorf("`ambar scan` takes no positional arguments, got %q", fs.Arg(0))
	}

	log := newLogger()

	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck // nothing useful to do on close failure

	ignore, err := library.NewMatcher(cfg.IgnoreGlobs)
	if err != nil {
		return err
	}

	// Ctrl-C stops the scan. Because every write happens in one transaction at
	// the end, an interrupted scan leaves the index exactly as it was rather than
	// half-updated.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	indexer := index.New(database, index.Options{
		Root:    cfg.LibraryRoot,
		Ignore:  ignore,
		Buckets: cfg.LibraryBuckets,
		Log:     log,
	})

	if *dryRun {
		fmt.Println("dry run: nothing will be written")
	}
	fmt.Printf("scanning %s\n", cfg.LibraryRoot)

	report, err := indexer.Scan(ctx, index.ScanOptions{
		DryRun:         *dryRun,
		ReadDimensions: !*noDimensions,
	})
	if err != nil {
		return err
	}

	printScanReport(report, *verbose)

	// Queue the derivative work this scan uncovered. The jobs are picked up by
	// `ambar serve`, or immediately by `ambar derive`.
	if !*dryRun {
		queue := jobs.New(database, jobs.Options{Workers: cfg.Workers, Log: log})
		queued, err := derive.EnqueueStale(ctx, database, queue)
		if err != nil {
			return err
		}
		if queued > 0 {
			fmt.Printf("\nqueued %d derivative job(s). Run `ambar derive` to generate them now,\n"+
				"or leave them for `ambar serve` to pick up.\n", queued)
		}

		// §3: import metadata from any sidecars whose packs the index does not
		// already carry — this is what recovers a rebuilt or copied library.
		mgr := sidecar.New(database, sidecar.Options{
			LibraryRoot: cfg.LibraryRoot, DataRoot: cfg.DataRoot,
			Readonly: cfg.LibraryReadonly, Log: log,
		})
		if n, err := mgr.ImportAll(ctx); err != nil {
			log.Warn("sidecar import failed", "error", err)
		} else if n > 0 {
			fmt.Printf("imported metadata for %d pack(s) from sidecars\n", n)
		}
	}

	// A scan that hit per-file errors has still done useful work, so this is not
	// a failure — but the exit code should let a cron job notice.
	if len(report.Errors) > 0 {
		return fmt.Errorf("%d file(s) could not be indexed; see the errors above", len(report.Errors))
	}
	return nil
}

func printScanReport(r *index.ScanReport, verbose bool) {
	fmt.Println()

	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	line := func(label string, value any) {
		fmt.Fprintf(w, "  %s\t%v\n", label, value)
	}

	fmt.Println("packs")
	line("found", r.PacksFound)
	if !r.DryRun {
		line("new", r.PacksNew)
	}
	// §5.1: without collapsing, each multi-variant group would be several grid rows
	// for the same artwork.
	if r.Groups > 0 {
		line("asset groups", r.Groups)
		line("  multi-format", r.MultiVariantGroups)
	}

	fmt.Fprintln(w)
	w.Flush()
	fmt.Println("files")
	line("seen", r.FilesSeen)
	line("added", r.Added)
	line("unchanged", r.Unchanged)
	if r.MetadataOnly > 0 {
		line("touched (same content)", r.MetadataOnly)
	}
	// The three that deserve attention, so they are always shown even at zero.
	line("moved", r.Moved)
	line("content changed", r.ContentChanged)
	line("marked missing", r.MarkedMissing)
	if r.Reappeared > 0 {
		line("reappeared", r.Reappeared)
	}
	line("hashed", r.Hashed)
	if r.IgnoredJunk > 0 {
		line("ignored as junk", r.IgnoredJunk)
	}
	w.Flush()

	if len(r.Buckets) > 0 {
		fmt.Printf("\nbuckets (recursed into, not treated as packs): %s\n",
			strings.Join(r.Buckets, ", "))
	}
	if len(r.ReservedSkipped) > 0 {
		fmt.Printf("reserved directories skipped: %s\n", strings.Join(r.ReservedSkipped, ", "))
	}

	// §12 wants changed hashes flagged for review, and a missing file is the case
	// that could mean an unmounted share. Neither is buried in the numbers.
	if r.ContentChanged > 0 {
		fmt.Printf("\n%d file(s) changed content at an unchanged path. Nothing was lost — "+
			"the index now records the new bytes.\n", r.ContentChanged)
	}
	if r.MarkedMissing > 0 {
		fmt.Printf("\n%d file(s) are no longer present. Their index rows were MARKED, not deleted, "+
			"so restoring the files restores everything.\n", r.MarkedMissing)
	}

	if len(r.Errors) > 0 {
		fmt.Printf("\n%d error(s):\n", len(r.Errors))
		shown := r.Errors
		if !verbose && len(shown) > 10 {
			shown = shown[:10]
		}
		for _, e := range shown {
			fmt.Printf("  %v\n", e)
		}
		if len(shown) < len(r.Errors) {
			fmt.Printf("  ... and %d more (use --verbose to see all)\n", len(r.Errors)-len(shown))
		}
	}

	fmt.Printf("\ndone in %s\n", r.Duration.Round(time.Millisecond))
	if r.DryRun {
		fmt.Println("dry run: the index was not modified")
	}
}

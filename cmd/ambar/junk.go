package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/junk"
)

// runJunk reports removable clutter (§9.1, M12). It is reporting-only: it finds,
// measures, and prints, and never removes anything. The removal path arrives with
// M13, where the trash-staging and safety invariants ship alongside it.
func runJunk(args []string) error {
	fs := flag.NewFlagSet("ambar junk", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, database, err := openDatabase(ctx)
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	indexer := index.New(database, index.Options{Root: cfg.LibraryRoot})
	known, err := indexer.ContentHashes(ctx)
	if err != nil {
		return err
	}

	report, err := junk.Scan(junk.Options{
		LibraryRoot: cfg.LibraryRoot,
		DataRoot:    cfg.DataRoot,
		KnownHashes: known,
	})
	if err != nil {
		return err
	}

	if report.Empty() {
		fmt.Println("no junk found: the library and derivative cache are clean")
		return nil
	}

	fmt.Printf("found %d candidate(s) across %d finding(s), %s reclaimable:\n\n",
		report.TotalItems(), len(report.Findings), formatBytes(report.TotalBytes()))

	for _, f := range report.Findings {
		fmt.Printf("  %-22s %4d item(s)  %10s  — %s\n",
			f.Kind.Title(), f.Count(), formatBytes(f.TotalBytes), f.Kind.Explanation())
	}

	fmt.Println("\nThis is a report only — Ambar never removes anything on its own (invariant 3).")
	fmt.Println("A deliberate, human-selected removal workflow with trash staging arrives in M13.")
	return nil
}

// formatBytes renders a size the way a human reads it, matching the web UI's
// FormatBytes so the CLI and the page agree.
func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	value := float64(b)
	units := []string{"KB", "MB", "GB", "TB"}
	var suffix string
	for _, u := range units {
		value /= unit
		suffix = u
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}

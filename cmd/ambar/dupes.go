package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/dupes"
)

// runDupes reports duplicate content (§9.1, M13). Reporting only, like `ambar
// junk`: it compares, explains and prints. Nothing here can remove a file — the
// removal path needs a selection and a confirmed preview, which is the web flow, or
// the shell script it exports.
func runDupes(args []string) error {
	fs := flag.NewFlagSet("ambar dupes", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "list every copy in every finding, not just the summary")
	nearDistance := fs.Int("near-distance", 0,
		"maximum perceptual-hash distance for near-duplicates (default 5)")
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

	opts := dupes.DefaultOptions()
	if *nearDistance > 0 {
		opts.NearDistance = *nearDistance
	}

	report, err := dupes.NewDetector(database, opts).Scan(ctx)
	if err != nil {
		return err
	}

	// The CLI caches the same report the web page reads, so a command-line check
	// leaves the UI up to date instead of contradicting it.
	if err := dupes.WriteReport(cfg.DataRoot, report, time.Now().Unix()); err != nil {
		return err
	}

	if report.Empty() {
		fmt.Printf("no duplicates found across %d asset(s) in %d pack(s)\n",
			report.Stats.Assets, report.Stats.Packs)
		return nil
	}

	fmt.Printf("scanned %d asset(s) in %d pack(s); up to %s reclaimable\n",
		report.Stats.Assets, report.Stats.Packs, formatBytes(report.Stats.ReclaimableBytes()))
	fmt.Printf("  %s by acting on whole packs, %s file by file — the same bytes counted two ways\n\n",
		formatBytes(report.Stats.PackReclaimableBytes), formatBytes(report.Stats.ExactReclaimableBytes))

	for _, note := range report.Notes {
		fmt.Printf("  note: %s\n", note)
	}
	if len(report.Notes) > 0 {
		fmt.Println()
	}

	if len(report.Packs) > 0 {
		fmt.Printf("packs (%d):\n", len(report.Packs))
		for _, f := range report.Packs {
			fmt.Printf("  %-32s %3d%% similar  %10s\n    candidate: %s\n    kept:      %s\n",
				f.Kind.Title(), f.SimilarityPercent(), formatBytes(f.Bytes),
				f.Candidate.Path, f.Container.Path)
			if f.Candidate.Blocked() {
				fmt.Printf("    blocked:   in use by %v — never a removal candidate\n", f.Candidate.ProjectUses)
			}
			if len(f.Transfers) > 0 {
				fmt.Printf("    transfer:  %v would move onto the kept pack first\n", f.Transfers)
			}
		}
		fmt.Println()
	}

	if len(report.Exact) > 0 {
		fmt.Printf("exact duplicates (%d finding(s)):\n", len(report.Exact))
		for _, f := range report.Exact {
			fmt.Printf("  %s  %d copies  %s reclaimable\n", shortHash(f.Sha), f.Count(), formatBytes(f.Bytes))
			if !*verbose {
				continue
			}
			for _, c := range f.Copies {
				marker := " "
				switch {
				case c.Blocked():
					marker = "!"
				case c.Favoured:
					marker = "*"
				}
				fmt.Printf("    %s %s\n", marker, c.Path)
				if c.Blocked() {
					fmt.Printf("        in use by %s\n", c.BlockedBy())
				} else if c.Favoured {
					fmt.Printf("        keep hint: %s\n", c.FavouredWhy)
				}
			}
		}
		fmt.Println()
	}

	if len(report.Near) > 0 {
		fmt.Printf("near-duplicate clusters (%d) — review only, never proposed for removal:\n", len(report.Near))
		for _, f := range report.Near {
			fmt.Printf("  %d image(s), distance up to %d\n", f.Count(), f.MaxDistance)
			if !*verbose {
				continue
			}
			for _, c := range f.Copies {
				fmt.Printf("      %s\n", c.Path)
			}
		}
		fmt.Println()
	}

	fmt.Println("* = the copy the keep heuristics favour (a hint; it selects nothing)")
	fmt.Println("! = refused: an asset a Godot project uses is never a removal candidate")
	fmt.Println("\nNothing was changed. Select copies in the web UI at /dupes, confirm the preview,")
	fmt.Println("or export the selection as a shell script and run it yourself.")
	return nil
}

// shortHash is the first 12 hex characters, enough to identify a hash in a list.
func shortHash(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

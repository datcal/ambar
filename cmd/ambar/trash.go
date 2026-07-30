package main

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/removal"
)

// runTrash inspects and manages the trash (§9.1).
//
//	ambar trash list                      what is in there and how to get it back
//	ambar trash restore <batch> [path...] put files back where they came from
//	ambar trash purge [--older-than 30d]  delete old batches, permanently
//
// Purging is the only irreversible operation in Ambar, so it is a command a person
// types and never something that happens on its own (§9.1 rules out "any automatic
// purging of trash, including under low-disk conditions").
func runTrash(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: ambar trash list|restore|purge")
	}
	switch args[0] {
	case "list":
		return runTrashList(args[1:])
	case "restore":
		return runTrashRestore(args[1:])
	case "purge":
		return runTrashPurge(args[1:])
	default:
		return fmt.Errorf("unknown trash command %q; use list, restore or purge", args[0])
	}
}

// openTrash wires the executor the CLI needs.
func openTrash(ctx context.Context) (*removal.Executor, func() error, error) {
	cfg, database, err := openDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}
	log := newLogger()
	exec := removal.NewExecutor(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir,
		cfg.DedupeLinkMode, audit.New(database, log), log)
	return exec, database.Close, nil
}

func runTrashList(args []string) error {
	fs := flag.NewFlagSet("ambar trash list", flag.ContinueOnError)
	verbose := fs.Bool("v", false, "list every entry, not just the batch summaries")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	exec, closeDB, err := openTrash(ctx)
	if err != nil {
		return err
	}
	defer closeDB() //nolint:errcheck

	batches, err := exec.ListBatches()
	if err != nil {
		return err
	}
	if len(batches) == 0 {
		fmt.Println("the trash is empty")
		return nil
	}

	var total int64
	for _, b := range batches {
		total += b.Bytes()
	}
	fmt.Printf("%d batch(es), %s held:\n\n", len(batches), formatBytes(total))

	for _, b := range batches {
		fmt.Printf("  %s  %s  %d entr%s  %s\n", b.ID, b.CreatedTime().Format("2006-01-02 15:04"),
			len(b.Entries), plural(len(b.Entries)), formatBytes(b.Bytes()))
		if b.Reason != "" {
			fmt.Printf("      reason: %s\n", b.Reason)
		}
		if b.State != removal.BatchApplied {
			fmt.Printf("      state:  %s — interrupted; the entries below are what it intended to move\n", b.State)
		}
		if b.Failed() > 0 {
			fmt.Printf("      failed: %d entr%s did not happen\n", b.Failed(), plural(b.Failed()))
		}
		if !*verbose {
			continue
		}
		for _, e := range b.Entries {
			state := "in the trash"
			switch {
			case !e.Done():
				state = "failed: " + e.Error
			case e.Restored():
				state = "restored"
			case e.Action == removal.ActionLink:
				state = "linked to " + e.KeepPath
			}
			fmt.Printf("        %-12s %s (%s)\n", e.Root, e.Path, state)
		}
	}
	fmt.Println("\nrestore with: ambar trash restore <batch-id> [path ...]")
	return nil
}

func runTrashRestore(args []string) error {
	fs := flag.NewFlagSet("ambar trash restore", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: ambar trash restore <batch-id> [path ...]")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	exec, closeDB, err := openTrash(ctx)
	if err != nil {
		return err
	}
	defer closeDB() //nolint:errcheck

	batchID := fs.Arg(0)
	paths := fs.Args()[1:]

	restored, failures, err := exec.Restore(ctx, batchID, paths, removal.Actor{Username: "cli"})
	if err != nil {
		return err
	}
	fmt.Printf("restored %d item(s) from %s\n", restored, batchID)
	for path, reason := range failures {
		// Never an overwrite: a path that has since been re-created is reported and
		// left in the trash for the human to sort out.
		fmt.Printf("  not restored: %s\n      %s\n", path, reason)
	}
	if restored > 0 {
		fmt.Println("run `ambar scan` so the index picks the files up again")
	}
	return nil
}

func runTrashPurge(args []string) error {
	fs := flag.NewFlagSet("ambar trash purge", flag.ContinueOnError)
	olderThan := fs.Duration("older-than", 0,
		"purge batches finished longer ago than this (default: AMBAR_TRASH_RETENTION)")
	yes := fs.Bool("yes", false, "actually purge; without this the command only reports what it would do")
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

	log := newLogger()
	exec := removal.NewExecutor(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir,
		cfg.DedupeLinkMode, audit.New(database, log), log)

	window := *olderThan
	if window <= 0 {
		window = cfg.TrashRetention
	}
	if window <= 0 {
		// "No retention configured" must never be read as "delete everything".
		return fmt.Errorf("no retention window: set AMBAR_TRASH_RETENTION or pass --older-than " +
			"(e.g. --older-than 720h)")
	}
	cutoff := time.Now().Add(-window)

	batches, err := exec.ListBatches()
	if err != nil {
		return err
	}
	var candidates []*removal.Batch
	var bytes int64
	for _, b := range batches {
		at := b.AppliedAt
		if at == 0 {
			at = b.CreatedAt
		}
		if at < cutoff.Unix() {
			candidates = append(candidates, b)
			bytes += b.Bytes()
		}
	}

	if len(candidates) == 0 {
		fmt.Printf("nothing older than %s; %d batch(es) kept\n", window, len(batches))
		return nil
	}

	fmt.Printf("%d batch(es) finished before %s, holding %s:\n",
		len(candidates), cutoff.Format("2006-01-02 15:04"), formatBytes(bytes))
	for _, b := range candidates {
		fmt.Printf("  %s  %d entr%s  %s\n", b.ID, len(b.Entries), plural(len(b.Entries)), formatBytes(b.Bytes()))
	}

	// A dry run by default. This is the one place in Ambar where data is destroyed,
	// and typing the command should not be enough on its own.
	if !*yes {
		fmt.Println("\nthis was a dry run — nothing was deleted. Re-run with --yes to purge.")
		return nil
	}

	report, err := exec.Purge(ctx, cutoff, removal.Actor{Username: "cli"})
	if err != nil {
		return err
	}
	fmt.Printf("\npurged %d batch(es), %s freed; %d kept as younger than %s\n",
		len(report.Batches), formatBytes(report.Bytes), report.Kept, window)
	return nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

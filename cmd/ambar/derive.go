package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/library"
)

const deriveUsage = `Usage:
  ambar derive [--workers N] [--retry-failed]

Generates thumbnails and previews for anything that needs them, then exits.

The same work happens automatically while ` + "`ambar serve`" + ` is running; this command is
for a one-shot pass — after a CLI scan, or from a scheduled task.
`

func runDerive(args []string) error {
	fs := flag.NewFlagSet("ambar derive", flag.ContinueOnError)
	workers := fs.Int("workers", 0,
		"how many derivatives to generate at once (default: AMBAR_WORKERS)")
	retryFailed := fs.Bool("retry-failed", false,
		"reset previously failed derivatives and try them again")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, deriveUsage)
		return fmt.Errorf("`ambar derive` takes no positional arguments, got %q", fs.Arg(0))
	}

	log := newLogger()

	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "derivatives"), 0o755); err != nil {
		return fmt.Errorf("create the derivatives directory: %w", err)
	}

	concurrency := *workers
	if concurrency <= 0 {
		concurrency = cfg.Workers
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	queue := jobs.New(database, jobs.Options{Workers: concurrency, Log: log})
	derive.New(database, derive.Options{
		LibraryRoot: cfg.LibraryRoot,
		DataRoot:    cfg.DataRoot,
		MaxPixels:   cfg.MaxImagePixels,
		Log:         log,
		BlenderBin:  cfg.BlenderBin,
	}).Register(queue)

	// A scan job could be sitting in the queue from the UI. Registering the handler
	// means this command drains it rather than leaving it claimed and abandoned.
	ignore, err := library.NewMatcher(cfg.IgnoreGlobs)
	if err != nil {
		return err
	}
	indexer := index.New(database, index.Options{
		Root:    cfg.LibraryRoot,
		Ignore:  ignore,
		Buckets: cfg.LibraryBuckets,
		Log:     log,
	})
	indexer.RegisterScanJob(queue, func(ctx context.Context) error {
		_, err := derive.EnqueueStale(ctx, database, queue)
		return err
	})

	if *retryFailed {
		n, err := derive.ResetFailed(ctx, database)
		if err != nil {
			return err
		}
		if _, err := queue.RetryFailed(ctx, ""); err != nil {
			return err
		}
		fmt.Printf("reset %d previously failed derivative(s)\n", n)
	}

	queued, err := derive.EnqueueStale(ctx, database, queue)
	if err != nil {
		return err
	}

	before, err := derive.LoadStats(ctx, database)
	if err != nil {
		return err
	}

	runnable, err := queue.Runnable(ctx)
	if err != nil {
		return err
	}
	if runnable == 0 {
		if backlogged, err := queue.Backlogged(ctx); err == nil && backlogged > 0 {
			fmt.Printf("nothing runnable yet: %d job(s) are serving out a retry backoff.\n"+
				"Run again shortly, or use --retry-failed to reset them now.\n", backlogged)
		} else {
			fmt.Println("nothing to do: every asset already has current derivatives")
		}
		printDeriveStats(before)
		return nil
	}

	fmt.Printf("generating derivatives for %d asset(s) with %d worker(s)\n", runnable, concurrency)
	if queued != runnable {
		fmt.Printf("  (%d were already queued from an earlier run)\n", runnable-queued)
	}

	start := time.Now()

	// Run the workers, and stop once there is nothing left that could start now.
	//
	// "Runnable now" rather than "queue empty" on purpose: a job that just failed is
	// queued again behind a retry backoff, and waiting ten minutes for a file that will
	// never decode is not useful work for a one-shot command. Those are left for the
	// next run and reported at the end.
	drained := make(chan struct{})
	workerCtx, stopWorkers := context.WithCancel(ctx)
	defer stopWorkers()

	go func() {
		defer close(drained)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-workerCtx.Done():
				return
			case <-ticker.C:
				runnable, err := queue.Runnable(workerCtx)
				if err != nil {
					return
				}
				s, err := queue.Stats(workerCtx)
				if err != nil {
					return
				}
				if runnable == 0 && s.Running == 0 {
					return
				}
			}
		}
	}()

	workersStopped := make(chan struct{})
	go func() {
		defer close(workersStopped)
		if err := queue.Run(workerCtx); err != nil {
			log.Error("workers stopped with an error", "error", err)
		}
	}()

	select {
	case <-drained:
	case <-ctx.Done():
		fmt.Println("\ninterrupted; partial progress is kept and the rest stays queued")
	}
	stopWorkers()
	<-workersStopped

	after, err := derive.LoadStats(context.Background(), database)
	if err != nil {
		return err
	}
	final, err := queue.Stats(context.Background())
	if err != nil {
		return err
	}

	fmt.Printf("\ndone in %s\n", time.Since(start).Round(time.Millisecond))
	printDeriveStats(after)

	// A job that failed once is queued again behind a backoff rather than abandoned,
	// so say so instead of leaving the counts looking inconsistent.
	if backlogged, err := queue.Backlogged(context.Background()); err == nil && backlogged > 0 {
		fmt.Printf("\n%d job(s) are waiting on a retry backoff. Run `ambar derive` again to\n"+
			"pick them up, or `--retry-failed` to reset them immediately.\n", backlogged)
	}

	if final.Failed > 0 {
		// Not a hard error: the successful derivatives are still useful. But the exit
		// code lets a scheduled task notice, and §12 wants failures visible.
		return fmt.Errorf("%d job(s) failed; see /jobs in the UI or `ambar derive --retry-failed`",
			final.Failed)
	}
	return nil
}

func printDeriveStats(s derive.Stats) {
	fmt.Println()
	fmt.Printf("  ok           %d\n", s.OK)
	if s.Pending > 0 {
		fmt.Printf("  pending      %d\n", s.Pending)
	}
	if s.Unsupported > 0 {
		fmt.Printf("  unsupported  %d  (no decoder for the format; not an error)\n", s.Unsupported)
	}
	if s.Failed > 0 {
		fmt.Printf("  failed       %d\n", s.Failed)
	}
}

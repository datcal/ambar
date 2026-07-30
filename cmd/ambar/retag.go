package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/autotag"
	"github.com/datcal/ambar/internal/tags"
)

const retagUsage = `Usage:
  ambar retag

Applies the automatic tags of §7 to every indexed asset: a folder: tag per
meaningful path segment, and type:/style:/has: tags from the classified kind and
image analysis.

Safe to re-run: it only adds tags, never removes them, and a manual tag is never
demoted by an automatic one. Run it after a scan and derive pass.
`

func runRetag(args []string) error {
	fs := flag.NewFlagSet("ambar retag", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		fmt.Fprint(os.Stderr, retagUsage)
		return fmt.Errorf("`ambar retag` takes no positional arguments, got %q", fs.Arg(0))
	}

	log := newLogger()

	_, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	tagger := autotag.New(database, tags.NewStore(database), log)

	fmt.Println("applying automatic tags")
	start := time.Now()
	rep, err := tagger.Retag(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("\ndone in %s\n", time.Since(start).Round(time.Millisecond))
	fmt.Printf("  assets examined     %d\n", rep.Assets)
	fmt.Printf("  assets tagged       %d\n", rep.AssetsTagged)
	fmt.Printf("  auto_path applied   %d\n", rep.PathTags)
	fmt.Printf("  auto_type applied   %d\n", rep.TypeTags)
	fmt.Printf("  distinct tags       %d\n", rep.DistinctTags)
	return nil
}

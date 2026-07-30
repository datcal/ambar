package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/datcal/ambar/internal/autotag"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/ingest"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/safepath"
	"github.com/datcal/ambar/internal/tags"
)

const ingestUsage = `Usage:
  ambar ingest <archive> [--source URL]

Extracts an archive into the library, records what provenance it can, then
indexes and auto-tags the new pack. The archive may already sit in the library's
_inbox/, or be any path elsewhere, in which case it is copied into _inbox first.

Unpacking is safe: path traversal, absolute paths and symlinks are refused, and a
bad archive is moved to _quarantine/ with an error log rather than half-applied.
`

func runIngest(args []string) error {
	fs := flag.NewFlagSet("ambar ingest", flag.ContinueOnError)
	source := fs.String("source", "", "source URL to record as provenance (§9)")
	// parseFlags handles a --source that follows the positional, which is the
	// natural way to type `ambar ingest foo.zip --source ...`.
	positional, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		fmt.Fprint(os.Stderr, ingestUsage)
		return fmt.Errorf("`ambar ingest` takes exactly one archive path")
	}
	archiveArg := positional[0]

	log := newLogger()
	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	if cfg.LibraryReadonly {
		return fmt.Errorf("ingest is disabled: AMBAR_LIBRARY_READONLY is set")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Get the archive to a library-relative path, copying it into _inbox if it is
	// not already under the library root.
	relPath, err := stageArchive(cfg.LibraryRoot, archiveArg)
	if err != nil {
		return err
	}

	ingester := ingest.New(database, ingest.Options{
		LibraryRoot:  cfg.LibraryRoot,
		KeepArchives: cfg.KeepArchives,
		Readonly:     cfg.LibraryReadonly,
		MaxBytes:     cfg.MaxArchiveUncompressed,
		MaxEntries:   cfg.MaxArchiveEntries,
		Log:          log,
	})

	fmt.Printf("ingesting %s\n", relPath)
	res, err := ingester.Ingest(ctx, relPath, *source)
	if err != nil {
		return err
	}
	if res.Quarantined {
		return fmt.Errorf("archive quarantined: %s\n  see %s/", res.QuarantineReason, ingest.QuarantineDir)
	}
	fmt.Printf("  extracted %d file(s) into %s\n", res.FilesWritten, res.PackRelPath)
	if res.Flattened != "" {
		fmt.Printf("  flattened redundant top folder %q\n", res.Flattened)
	}
	for _, s := range res.Skipped {
		fmt.Printf("  skipped non-regular entry %q\n", s)
	}

	// Index the new pack, then auto-tag it (§5 pipeline steps asset.index and
	// pack.autotag). A full scan is the simplest correct index and is idempotent.
	ignore, err := library.NewMatcher(cfg.IgnoreGlobs)
	if err != nil {
		return err
	}
	indexer := index.New(database, index.Options{
		Root: cfg.LibraryRoot, Ignore: ignore, Buckets: cfg.LibraryBuckets, Log: log,
	})
	if _, err := indexer.Scan(ctx, index.ScanOptions{ReadDimensions: true}); err != nil {
		return err
	}
	if rep, err := autotag.New(database, tags.NewStore(database), log).Retag(ctx); err != nil {
		return err
	} else {
		fmt.Printf("  auto-tagged: %d path + %d type tag application(s)\n", rep.PathTags, rep.TypeTags)
	}

	// Queue derivatives for the newly indexed files; leave generation to
	// `ambar derive` or `ambar serve`, exactly as `ambar scan` does.
	queue := jobs.New(database, jobs.Options{Workers: cfg.Workers, Log: log})
	if n, err := derive.EnqueueStale(ctx, database, queue); err != nil {
		return err
	} else if n > 0 {
		fmt.Printf("  queued %d derivative job(s); run `ambar derive` to generate them\n", n)
	}

	fmt.Printf("\ndone. The pack needs provenance — set its licence in the UI (§9).\n")
	return nil
}

// stageArchive returns a library-relative path to the archive, copying it into
// _inbox when the given path is outside the library root.
func stageArchive(libraryRoot, arg string) (string, error) {
	abs, err := filepath.Abs(arg)
	if err != nil {
		return "", fmt.Errorf("resolve %q: %w", arg, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return "", fmt.Errorf("archive %q: %w", arg, err)
	}

	if safepath.IsWithin(libraryRoot, abs) {
		rel, err := safepath.RelUnder(libraryRoot, abs)
		if err != nil {
			return "", err
		}
		return rel, nil
	}

	// Outside the library: copy into _inbox so ingest works only through paths it
	// has validated under the root.
	inboxAbs, err := safepath.Resolve(libraryRoot, ingest.InboxDir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(inboxAbs, 0o755); err != nil {
		return "", fmt.Errorf("create %s: %w", ingest.InboxDir, err)
	}
	dest := filepath.Join(inboxAbs, filepath.Base(abs))
	if err := copyFile(abs, dest); err != nil {
		return "", err
	}
	fmt.Printf("copied into %s/\n", ingest.InboxDir)
	return ingest.InboxDir + "/" + filepath.Base(abs), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

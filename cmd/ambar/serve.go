package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/autotag"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/dupes"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/ingest"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/junk"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/removal"
	"github.com/datcal/ambar/internal/server"
	"github.com/datcal/ambar/internal/sidecar"
	"github.com/datcal/ambar/internal/tags"
)

// shutdownGrace is how long in-flight requests get to finish (§12: graceful
// shutdown, close the database cleanly).
const shutdownGrace = 15 * time.Second

func runServe(args []string) error {
	fs := flag.NewFlagSet("ambar serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	log := newLogger()

	cfg, err := config.Load()
	if err != nil {
		// §17: fail loudly rather than limping along. The most common cause is
		// a uid/gid mismatch on the bind-mounted volumes.
		return fmt.Errorf("configuration is not usable:\n%w", err)
	}

	log.Info("starting ambar",
		"version", version,
		"commit", commit,
		"bind", cfg.Bind,
		"library_root", cfg.LibraryRoot,
		"data_root", cfg.DataRoot,
		"library_readonly", cfg.LibraryReadonly,
		"workers", cfg.Workers,
	)
	log.Info("session secret resolved", "source", cfg.SessionSecretSource)

	if len(cfg.TrustedProxies) == 0 {
		log.Info("AMBAR_TRUSTED_PROXIES is empty: forwarded-IP headers are ignored, " +
			"using the socket peer address")
	} else {
		log.Info("trusting forwarded-IP headers from configured proxies",
			"cidrs", fmt.Sprint(cfg.TrustedProxies), "header", cfg.RealIPHeader)
	}

	if !cfg.CookieSecure {
		// Worth one clear line in the log: this is the documented deviation from
		// §11, and whoever reads the log should know which mode they are in.
		log.Warn("session cookie will not carry the Secure attribute, so it is sent over plain HTTP; "+
			"this follows AMBAR_BASE_URL. Set AMBAR_COOKIE_SECURE=true once access is HTTPS-only",
			"base_url", cfg.BaseURL.String())
	}

	database, err := db.Open(cfg.DatabasePath())
	if err != nil {
		return err
	}
	defer func() {
		if err := database.Close(); err != nil {
			log.Error("could not close database cleanly", "error", err)
		}
	}()

	// The signal context is established before migrations so a Ctrl-C during a
	// long migration is not ignored.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	applied, err := database.Migrate(ctx)
	if err != nil {
		return err
	}
	if len(applied) > 0 {
		log.Info("applied database migrations", "versions", applied)
	} else {
		log.Debug("database schema already up to date")
	}

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

	// §12 wants derivatives on a real volume; the health check probes this directory,
	// so it has to exist before the first request.
	if err := os.MkdirAll(filepath.Join(cfg.DataRoot, "derivatives"), 0o755); err != nil {
		return fmt.Errorf("create the derivatives directory: %w", err)
	}

	queue := jobs.New(database, jobs.Options{Workers: cfg.Workers, Log: log})

	// §7 auto-tagging. M3 shipped it as a manual `ambar retag` because the signals it
	// reads (style:pixel-art, has:alpha) only exist after derive. Both halves are wired
	// up here: derive retags the content it just analysed, and a finished scan retags
	// the whole index for the path-derived tags. Both reconcile rather than append, so
	// a reclassified file loses the tag it no longer earns.
	tagger := autotag.New(database, tags.NewStore(database), log)

	deriver := derive.New(database, derive.Options{
		LibraryRoot: cfg.LibraryRoot,
		DataRoot:    cfg.DataRoot,
		MaxPixels:   cfg.MaxImagePixels,
		Log:         log,
		BlenderBin:  cfg.BlenderBin,
		AfterAnalyse: func(ctx context.Context, sha256hex string) error {
			_, err := tagger.RetagContent(ctx, sha256hex)
			return err
		},
	})
	deriver.Register(queue)
	deriver.RegisterPalette(queue)

	// A scan enqueues the derivative work it discovered. Passing this in as a
	// callback keeps internal/index unaware of internal/derive.
	sidecars := sidecar.New(database, sidecar.Options{
		LibraryRoot: cfg.LibraryRoot, DataRoot: cfg.DataRoot,
		Readonly: cfg.LibraryReadonly, Log: log,
	})
	indexer.RegisterScanJob(queue, func(ctx context.Context) error {
		// §3: recover metadata from sidecars for any pack the index does not carry.
		if n, err := sidecars.ImportAll(ctx); err != nil {
			log.WarnContext(ctx, "sidecar import failed", "error", err)
		} else if n > 0 {
			log.InfoContext(ctx, "imported sidecar metadata", "packs", n)
		}
		// §7 auto tags from the paths this scan just indexed. Cheap next to the scan
		// itself, and it prunes the tags of anything that moved or was reclassified.
		// A failure here must not fail the scan: the tags are recomputable, the index
		// is the valuable part.
		if rep, err := tagger.Retag(ctx); err != nil {
			log.WarnContext(ctx, "auto-tagging after scan failed", "error", err)
		} else if rep.AssetsTagged > 0 || rep.Pruned > 0 {
			log.InfoContext(ctx, "applied automatic tags",
				"assets", rep.AssetsTagged, "path_tags", rep.PathTags,
				"type_tags", rep.TypeTags, "pruned", rep.Pruned)
		}
		n, err := derive.EnqueueStale(ctx, database, queue)
		if err != nil {
			return err
		}
		if n > 0 {
			log.InfoContext(ctx, "enqueued derivative jobs", "count", n)
		}
		return nil
	})

	// Ingest (§5). A finished extraction enqueues a scan, which indexes the new
	// pack and (via the callback above) its derivatives. The _inbox poller (M4)
	// enqueues these jobs.
	ingest.New(database, ingest.Options{
		LibraryRoot:  cfg.LibraryRoot,
		KeepArchives: cfg.KeepArchives,
		Readonly:     cfg.LibraryReadonly,
		MaxBytes:     cfg.MaxArchiveUncompressed,
		MaxEntries:   cfg.MaxArchiveEntries,
		Log:          log,
	}).Register(queue, func(ctx context.Context) error {
		_, err := index.EnqueueScan(ctx, queue, index.ScanJobPayload{ReadDimensions: true})
		return err
	})

	// Junk sweep (§9.1, M12): reporting only. The walk is real work, so it runs on
	// the queue with pollable status (invariant 8), caching a report the /junk page
	// reads. The hash provider keeps internal/junk free of SQL.
	junk.NewRunner(cfg.LibraryRoot, cfg.DataRoot, indexer.ContentHashes, log).Register(queue)

	// Duplicate sweep (§9.1, M13): also detection only, and also on the queue —
	// comparing every pack's hash set and every image's perceptual hash is real work.
	dupes.NewRunner(database, cfg.DataRoot, dupes.DefaultOptions(), log).Register(queue)

	// The removal path (§9.1, M13). This is the one place in Ambar that touches user
	// data destructively, so the wiring is explicit about who does what: the planner
	// refuses, the executor moves to the trash, and the transfer callback carries
	// tags and provenance onto the superset before a subset pack is removed. The
	// callback is what keeps internal/removal unaware of tags and duplicates.
	removalPlanner := removal.NewPlanner(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir)
	removalExec := removal.NewExecutor(database, cfg.LibraryRoot, cfg.DataRoot, cfg.TrashDir,
		cfg.DedupeLinkMode, audit.New(database, log), log)
	removal.NewRunner(removalPlanner, removalExec,
		func(ctx context.Context, fromPackID, toPackID int64) (string, error) {
			summary, err := dupes.TransferCuration(ctx, database, fromPackID, toPackID)
			if err != nil {
				return "", err
			}
			return summary.Describe(), nil
		}, log).Register(queue)

	// §9.1: "Probe support at startup and report it in the health endpoint." Probing
	// writes a temporary file, so it is skipped on a read-only library — where nothing
	// can be linked or removed anyway.
	if !cfg.LibraryReadonly {
		support := removal.ProbeLinkSupport(cfg.DedupeLinkMode, cfg.LibraryRoot)
		if support.OK {
			log.Info("dedupe linking available", "mode", support.Mode, "detail", support.Detail)
		} else {
			log.Warn("dedupe linking unavailable; duplicates can only be moved to the trash",
				"mode", support.Mode, "detail", support.Detail)
		}
	}

	srv, err := server.New(cfg, database, indexer, queue, log,
		server.BuildInfo{Version: version, Commit: commit})
	if err != nil {
		return err
	}

	// One sweep at startup, not a background ticker. Nothing in this application
	// deletes on a schedule (ARCHITECTURE.md rule 3), and Lookup rejects expired
	// rows anyway, so this is only housekeeping.
	if n, err := srv.Sessions().DeleteExpired(ctx); err != nil {
		log.Warn("could not clear expired sessions", "error", err)
	} else if n > 0 {
		log.Info("cleared expired sessions", "count", n)
	}

	httpServer := &http.Server{
		Addr:    cfg.Bind,
		Handler: srv.Handler(),
		// A slow-loris client must not hold a connection open indefinitely.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// ReadTimeout and WriteTimeout are deliberately unset. Both would need
		// raising for the large archive uploads in M4 and the 200 MB ranged
		// downloads in M8; a whole-request deadline is the wrong tool for those,
		// and ReadHeaderTimeout already covers the slow-loris case. Revisit with
		// per-route deadlines when those milestones land.
		ErrorLog: nil,
	}

	// Workers run in-process: §0 is explicit that there is "no agent/server split, no
	// separate worker container".
	workersDone := make(chan struct{})
	go func() {
		defer close(workersDone)
		if err := queue.Run(ctx); err != nil {
			log.Error("job workers stopped with an error", "error", err)
		}
	}()

	// Anything left pending from a previous run, picked up without waiting for a scan.
	if n, err := derive.EnqueueStale(ctx, database, queue); err != nil {
		log.Warn("could not enqueue outstanding derivatives", "error", err)
	} else if n > 0 {
		log.Info("enqueued outstanding derivative jobs", "count", n)
	}

	// Models whose thumbnail exists but whose palette does not (M17). Converges rather
	// than repeating: once a model has swatches the query stops selecting it.
	if n, err := derive.EnqueueModelPalettes(ctx, database, queue); err != nil {
		log.Warn("could not enqueue model palettes", "error", err)
	} else if n > 0 {
		log.Info("enqueued model palette jobs", "count", n)
	}

	// Poll _inbox for dropped archives (§5, the primary ingest path). Skipped on a
	// read-only library, where ingest is disabled.
	if !cfg.LibraryReadonly {
		go ingest.NewPoller(cfg.LibraryRoot, queue, cfg.InboxPollInterval, log).Run(ctx)
		log.Info("watching _inbox for archives", "interval", cfg.InboxPollInterval)
	}

	// One scan a night, in the small hours (M16). Nothing else is scheduled: invariant 3 says
	// the application never removes anything on its own, and this is the only kind of
	// background work that cannot break it.
	if cfg.NightlyScanAt >= 0 {
		go runNightlyScan(ctx, queue, cfg.NightlyScanAt, log)
	} else {
		log.Info("nightly scan disabled")
	}

	// Serve in the background so the main goroutine can wait on the signal.
	serveErr := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Bind, "base_url", cfg.BaseURL.String())
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Info("shutdown signal received, draining", "grace", shutdownGrace)
	}

	// A fresh context: ctx is already cancelled by the signal.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		// Requests still running when the grace period ended. The deferred
		// database Close still runs.
		return fmt.Errorf("graceful shutdown did not finish within %s: %w", shutdownGrace, err)
	}
	// Workers get the same grace period. Anything still running is left in the
	// 'running' state and requeued at next startup (§12).
	select {
	case <-workersDone:
	case <-shutdownCtx.Done():
		log.Warn("job workers did not finish draining; in-flight jobs will be requeued on restart")
	}

	log.Info("stopped cleanly")
	return nil
}

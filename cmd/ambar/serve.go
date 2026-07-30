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

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/ingest"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/junk"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/server"
	"github.com/datcal/ambar/internal/sidecar"
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
	deriver := derive.New(database, derive.Options{
		LibraryRoot: cfg.LibraryRoot,
		DataRoot:    cfg.DataRoot,
		MaxPixels:   cfg.MaxImagePixels,
		Log:         log,
		BlenderBin:  cfg.BlenderBin,
	})
	deriver.Register(queue)

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

	srv, err := server.New(cfg, database, indexer, queue, log,
		server.BuildInfo{Version: version, Commit: commit})
	if err != nil {
		return err
	}

	// One sweep at startup, not a background ticker. Nothing in this application
	// deletes on a schedule (CLAUDE.md invariant 3), and Lookup rejects expired
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

	// Poll _inbox for dropped archives (§5, the primary ingest path). Skipped on a
	// read-only library, where ingest is disabled.
	if !cfg.LibraryReadonly {
		go ingest.NewPoller(cfg.LibraryRoot, queue, cfg.InboxPollInterval, log).Run(ctx)
		log.Info("watching _inbox for archives", "interval", cfg.InboxPollInterval)
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

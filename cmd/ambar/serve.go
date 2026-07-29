package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os/signal"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/server"
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

	srv, err := server.New(cfg, database, log, server.BuildInfo{Version: version, Commit: commit})
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
	log.Info("stopped cleanly")
	return nil
}

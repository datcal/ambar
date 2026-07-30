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

	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/ops"
)

func runRebuildIndex(args []string) error {
	fs := flag.NewFlagSet("ambar rebuild-index", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger()
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("configuration is not usable:\n%w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Println("rebuilding the index from the filesystem and sidecars")
	fmt.Println("(the library is never modified; only the database is replaced)")
	rep, err := ops.RebuildIndex(ctx, cfg, log)
	if err != nil {
		return err
	}
	fmt.Printf("\ndone: %d pack(s), %d asset(s), %d recovered from sidecars, %d auto-tag(s)\n",
		rep.Packs, rep.Assets, rep.SidecarPacks, rep.AutoTags)
	fmt.Println("run `ambar derive` to regenerate any missing derivatives")
	return nil
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("ambar verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	log := newLogger()
	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Println("re-hashing indexed files to detect bit rot")
	rep, err := ops.Verify(ctx, database, cfg.LibraryRoot, log)
	if err != nil {
		return err
	}
	fmt.Printf("\nchecked %d, unreadable %d, changed %d\n", rep.Checked, rep.Missing, rep.Mismatched)
	for _, p := range rep.Changed {
		fmt.Printf("  changed: %s\n", p)
	}
	if rep.Mismatched > 0 {
		// Non-zero exit so a scheduled verify can alert. Nothing was lost — the
		// rows are flagged, not deleted.
		return fmt.Errorf("%d file(s) changed content since indexing; flagged for review", rep.Mismatched)
	}
	return nil
}

func runBackup(args []string) error {
	fs := flag.NewFlagSet("ambar backup", flag.ContinueOnError)
	dir := fs.String("dir", "", "directory for the backup (default: AMBAR_BACKUP_DIR or <data>/backups)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, database, err := openDatabase(context.Background())
	if err != nil {
		return err
	}
	defer database.Close() //nolint:errcheck

	destDir := *dir
	if destDir == "" {
		destDir = cfg.BackupDir
	}
	if destDir == "" {
		destDir = filepath.Join(cfg.DataRoot, "backups")
	}
	dest := ops.BackupPath(destDir, time.Now())

	if err := ops.Backup(context.Background(), database, dest); err != nil {
		return err
	}
	if info, err := os.Stat(dest); err == nil {
		fmt.Printf("wrote %s (%.1f MB)\n", dest, float64(info.Size())/(1024*1024))
	} else {
		fmt.Printf("wrote %s\n", dest)
	}
	return nil
}

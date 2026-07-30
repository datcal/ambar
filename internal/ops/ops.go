// Package ops is the §12 maintenance operations: rebuild-index, verify and
// backup. They live together, and behind a testable API rather than only in the
// CLI, because rebuild-index fidelity is a §16 quality-bar test — invariant 2
// ("SQLite is a rebuildable index") is only true if this actually works.
package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/datcal/ambar/internal/autotag"
	"github.com/datcal/ambar/internal/config"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/safepath"
	"github.com/datcal/ambar/internal/sidecar"
	"github.com/datcal/ambar/internal/tags"
)

// RebuildReport summarises a rebuild-index run.
type RebuildReport struct {
	Packs        int
	Assets       int
	SidecarPacks int
	AutoTags     int
}

// RebuildIndex drops the database and reconstructs it from the filesystem and
// sidecars (§12, invariant 2): fresh schema, a full scan for identity, sidecar
// import for provenance and manual tags, then auto-tagging. Derivatives on disk
// are keyed by content hash and survive; they are left pending for `ambar derive`
// to regenerate. The library is never touched — only the database file is.
func RebuildIndex(ctx context.Context, cfg *config.Config, log *slog.Logger) (RebuildReport, error) {
	var rep RebuildReport

	dbPath := cfg.DatabasePath()
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(dbPath + suffix); err != nil && !os.IsNotExist(err) {
			return rep, fmt.Errorf("remove old database %s: %w", dbPath+suffix, err)
		}
	}

	database, err := db.Open(dbPath)
	if err != nil {
		return rep, err
	}
	defer database.Close() //nolint:errcheck
	if _, err := database.Migrate(ctx); err != nil {
		return rep, err
	}

	ignore, err := library.NewMatcher(cfg.IgnoreGlobs)
	if err != nil {
		return rep, err
	}
	indexer := index.New(database, index.Options{
		Root: cfg.LibraryRoot, Ignore: ignore, Buckets: cfg.LibraryBuckets, Log: log,
	})
	scan, err := indexer.Scan(ctx, index.ScanOptions{ReadDimensions: true})
	if err != nil {
		return rep, err
	}
	rep.Packs, rep.Assets = scan.PacksFound, scan.FilesSeen

	sc := sidecar.New(database, sidecar.Options{
		LibraryRoot: cfg.LibraryRoot, DataRoot: cfg.DataRoot, Readonly: cfg.LibraryReadonly, Log: log,
	})
	if n, err := sc.ImportAll(ctx); err != nil {
		log.WarnContext(ctx, "sidecar import during rebuild failed", "error", err)
	} else {
		rep.SidecarPacks = n
	}

	tagRep, err := autotag.New(database, tags.NewStore(database), log).Retag(ctx)
	if err != nil {
		return rep, err
	}
	rep.AutoTags = tagRep.PathTags + tagRep.TypeTags
	return rep, nil
}

// VerifyReport summarises a verify run.
type VerifyReport struct {
	Checked    int
	Mismatched int      // bytes at a stable path differ from the stored hash
	Missing    int      // the file could not be read
	Changed    []string // library-relative paths whose content changed
}

// Verify re-hashes indexed files and flags those whose bytes no longer match the
// stored hash (§12: "detect bit rot and truncated files"). Unlike scan, it hashes
// even when size and mtime are unchanged — that is the whole point. A mismatch
// sets content_changed_at, the same signal scan uses, so the review UI surfaces
// it. It never deletes and never rewrites the stored hash.
func Verify(ctx context.Context, database *db.DB, libraryRoot string, log *slog.Logger) (VerifyReport, error) {
	var rep VerifyReport

	rows, err := database.Reader.QueryContext(ctx, `
		SELECT a.id, p.library_rel_path, a.rel_path, a.sha256
		FROM assets a JOIN packs p ON p.id = a.pack_id
		WHERE a.missing_since IS NULL
		ORDER BY a.id`)
	if err != nil {
		return rep, fmt.Errorf("verify: load assets: %w", err)
	}
	type row struct {
		id       int64
		libPath  string
		wantHash string
	}
	var todo []row
	for rows.Next() {
		var (
			id           int64
			packRel, rel string
			hash         string
		)
		if err := rows.Scan(&id, &packRel, &rel, &hash); err != nil {
			rows.Close()
			return rep, err
		}
		lib := rel
		if packRel != "." && packRel != "" {
			lib = packRel + "/" + rel
		}
		todo = append(todo, row{id: id, libPath: lib, wantHash: hash})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return rep, err
	}

	now := time.Now().Unix()
	for _, r := range todo {
		abs, err := safepath.ResolveExisting(libraryRoot, r.libPath)
		if err != nil {
			rep.Missing++
			continue
		}
		got, err := hashFile(abs)
		if err != nil {
			rep.Missing++
			continue
		}
		rep.Checked++
		if got != r.wantHash {
			rep.Mismatched++
			rep.Changed = append(rep.Changed, r.libPath)
			if _, err := database.Writer.ExecContext(ctx,
				`UPDATE assets SET content_changed_at = ?, updated_at = ? WHERE id = ?`,
				now, now, r.id); err != nil {
				log.WarnContext(ctx, "verify: could not flag changed content", "asset_id", r.id, "error", err)
			}
		}
	}
	return rep, nil
}

// Backup writes a consistent copy of the database with SQLite's VACUUM INTO
// (§12: "at the SQLite level, not via btrfs/ZFS snapshots" — a filesystem
// snapshot of a live WAL database can be inconsistent). destPath must not exist.
func Backup(ctx context.Context, database *db.DB, destPath string) error {
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("backup: create directory: %w", err)
	}
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("backup: %s already exists", destPath)
	}
	// VACUUM INTO takes a consistent snapshot even while the database is in use.
	if _, err := database.Writer.ExecContext(ctx, `VACUUM INTO ?`, destPath); err != nil {
		return fmt.Errorf("backup: VACUUM INTO %s: %w", destPath, err)
	}
	return nil
}

// BackupPath builds a timestamped backup filename under dir.
func BackupPath(dir string, at time.Time) string {
	return filepath.Join(dir, "ambar-"+at.UTC().Format("20060102-150405")+".db")
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

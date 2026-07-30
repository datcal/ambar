// Package ingest is the §5 pipeline that turns a dropped archive into an indexed
// pack. It extracts the archive safely (internal/archive), records what it can of
// provenance, and moves the original aside — but it does not index the files
// itself: indexing is the scanner's job, and ingest enqueues a scan once the
// bytes are on disk. A failed extraction is quarantined, never left half-applied
// in the library.
package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/archive"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/library"
	"github.com/datcal/ambar/internal/safepath"
)

// Reserved directories under the library root (§3, §17).
const (
	InboxDir      = "_inbox"
	ArchivesDir   = "_archives"
	QuarantineDir = "_quarantine"
)

// JobType is the queue type for one archive ingest.
const JobType = "ingest.archive"

// ErrReadonly means ingest was attempted while AMBAR_LIBRARY_READONLY is set.
var ErrReadonly = errors.New("ingest is disabled: the library is read-only")

// Options configures an Ingester.
type Options struct {
	LibraryRoot  string
	KeepArchives bool
	Readonly     bool
	MaxBytes     int64
	MaxEntries   int
	Log          *slog.Logger
}

// Ingester runs the ingest pipeline.
type Ingester struct {
	db   *db.DB
	root string
	opts Options
	log  *slog.Logger
	now  func() time.Time
}

// New builds an Ingester.
func New(database *db.DB, opts Options) *Ingester {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Ingester{db: database, root: opts.LibraryRoot, opts: opts, log: log, now: time.Now}
}

// WithClock replaces the clock, for tests.
func (ig *Ingester) WithClock(now func() time.Time) *Ingester {
	ig.now = now
	return ig
}

// Result reports what one ingest did.
type Result struct {
	PackID       int64
	PackRelPath  string
	FilesWritten int
	Flattened    string
	Skipped      []string
	// Quarantined is set when the archive could not be extracted safely; the
	// reason is human-readable and the archive has been moved to _quarantine.
	Quarantined      bool
	QuarantineReason string
}

// Ingest processes one archive already present under the library root (typically
// _inbox/foo.zip). It never indexes — the caller enqueues a scan afterwards — so
// its unit of work is purely "get the bytes safely onto disk and record the
// pack". A safety or format failure is quarantined and returned as a non-error
// Result, because a bad drop is an expected event, not a pipeline fault.
func (ig *Ingester) Ingest(ctx context.Context, archiveRelPath, sourceURL string) (Result, error) {
	if ig.opts.Readonly {
		return Result{}, ErrReadonly
	}

	absArchive, err := safepath.ResolveExisting(ig.root, archiveRelPath)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: locate archive: %w", err)
	}

	info, err := archive.Inspect(absArchive)
	if err != nil {
		reason := fmt.Sprintf("inspect failed: %v", err)
		if qerr := ig.quarantine(absArchive, reason); qerr != nil {
			return Result{}, qerr
		}
		return Result{Quarantined: true, QuarantineReason: reason}, nil
	}

	packRel, absTarget, err := ig.uniquePackDir(absArchive)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(absTarget, 0o755); err != nil {
		return Result{}, fmt.Errorf("ingest: create target %q: %w", packRel, err)
	}

	extracted, err := archive.Extract(absArchive, absTarget, archive.Options{
		MaxTotalBytes: ig.opts.MaxBytes,
		MaxEntries:    ig.opts.MaxEntries,
	})
	if err != nil {
		reason := fmt.Sprintf("extract failed (%s): %v", info.Kind, err)
		// Remove the partial extraction, then quarantine the archive.
		_ = os.RemoveAll(absTarget)
		if qerr := ig.quarantine(absArchive, reason); qerr != nil {
			return Result{}, qerr
		}
		return Result{Quarantined: true, QuarantineReason: reason}, nil
	}

	sha, size, err := hashFile(absArchive)
	if err != nil {
		return Result{}, fmt.Errorf("ingest: hash archive: %w", err)
	}

	archiveName := filepath.Base(absArchive)
	if err := ig.retireArchive(absArchive); err != nil {
		return Result{}, err
	}

	packID, err := ig.createPack(ctx, packRel, archiveName, sha, size, sourceURL)
	if err != nil {
		return Result{}, err
	}

	return Result{
		PackID:       packID,
		PackRelPath:  packRel,
		FilesWritten: extracted.FilesWritten,
		Flattened:    extracted.Flattened,
		Skipped:      extracted.Skipped,
	}, nil
}

// uniquePackDir picks a library-relative directory for the extracted pack,
// derived from the archive name and suffixed until it does not collide.
func (ig *Ingester) uniquePackDir(absArchive string) (relPath, absPath string, err error) {
	base := filepath.Base(absArchive)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	slug := library.Slugify(base)
	if slug == "" {
		slug = "pack"
	}
	for i := 0; i < 1000; i++ {
		candidate := slug
		if i > 0 {
			candidate = fmt.Sprintf("%s-%d", slug, i+1)
		}
		abs, err := safepath.Resolve(ig.root, candidate)
		if err != nil {
			return "", "", fmt.Errorf("ingest: resolve target %q: %w", candidate, err)
		}
		if _, statErr := os.Lstat(abs); os.IsNotExist(statErr) {
			return candidate, abs, nil
		}
	}
	return "", "", fmt.Errorf("ingest: could not find a free directory for %q", slug)
}

// retireArchive moves the original into _archives (keeping it, §9) or removes it
// from _inbox. Removing an ingest input the human dropped is not the same as the
// application deleting library content (invariant 3): the archive is transient
// pipeline input, not an indexed original.
func (ig *Ingester) retireArchive(absArchive string) error {
	if !ig.opts.KeepArchives {
		if err := os.Remove(absArchive); err != nil {
			return fmt.Errorf("ingest: remove consumed archive: %w", err)
		}
		return nil
	}
	destDir, err := safepath.Resolve(ig.root, ArchivesDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("ingest: create %s: %w", ArchivesDir, err)
	}
	dest := uniquePath(filepath.Join(destDir, filepath.Base(absArchive)))
	if err := os.Rename(absArchive, dest); err != nil {
		return fmt.Errorf("ingest: retain archive: %w", err)
	}
	return nil
}

// quarantine moves a failed archive into _quarantine with an error log beside it,
// so a bad drop is inspectable rather than silently retried (§3, §12).
func (ig *Ingester) quarantine(absArchive, reason string) error {
	qDir, err := safepath.Resolve(ig.root, QuarantineDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		return fmt.Errorf("ingest: create %s: %w", QuarantineDir, err)
	}
	dest := uniquePath(filepath.Join(qDir, filepath.Base(absArchive)))
	if err := os.Rename(absArchive, dest); err != nil {
		return fmt.Errorf("ingest: quarantine archive: %w", err)
	}
	logPath := dest + ".error.txt"
	msg := fmt.Sprintf("%s\ningested at %s\n%s\n", filepath.Base(absArchive),
		ig.now().UTC().Format(time.RFC3339), reason)
	if err := os.WriteFile(logPath, []byte(msg), 0o644); err != nil {
		ig.log.Warn("could not write quarantine log", "path", logPath, "error", err)
	}
	ig.log.Warn("archive quarantined", "archive", filepath.Base(absArchive), "reason", reason)
	return nil
}

// createPack inserts the pack row with what provenance ingest knows. It starts in
// needs_provenance (§9); the capture form fills in the licence later. A later
// scan reconciles the assets and updates the pack's identity, but the ON CONFLICT
// in the scanner leaves these provenance columns untouched.
func (ig *Ingester) createPack(ctx context.Context, packRel, archiveName, sha string, size int64, sourceURL string) (int64, error) {
	name := filepath.Base(packRel)
	now := ig.now().Unix()
	res, err := ig.db.Writer.ExecContext(ctx, `
		INSERT INTO packs (name, slug, kind, library_rel_path,
		                   first_seen_at, last_seen_at, created_at, updated_at,
		                   source_url, original_archive_name, original_archive_sha256,
		                   original_archive_size, provenance_state)
		VALUES (?, ?, 'archive', ?, ?, ?, ?, ?, ?, ?, ?, ?, 'needs_provenance')
		ON CONFLICT(library_rel_path) DO UPDATE SET
		    source_url               = excluded.source_url,
		    original_archive_name    = excluded.original_archive_name,
		    original_archive_sha256  = excluded.original_archive_sha256,
		    original_archive_size    = excluded.original_archive_size,
		    updated_at               = excluded.updated_at`,
		name, library.Slugify(name), packRel, now, now, now, now,
		sourceURL, archiveName, sha, size)
	if err != nil {
		return 0, fmt.Errorf("ingest: create pack %q: %w", packRel, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, nil
}

// Register wires ingest into the queue. afterIngest runs after a successful,
// non-quarantined ingest; in practice it enqueues a scan so the new pack gets
// indexed — passed in rather than importing the index package here, the same
// pattern RegisterScanJob uses.
func (ig *Ingester) Register(q *jobs.Queue, afterIngest func(ctx context.Context) error) {
	q.Register(JobType, func(ctx context.Context, raw []byte) error {
		var p Payload
		if err := json.Unmarshal(raw, &p); err != nil {
			return fmt.Errorf("unmarshal ingest payload: %w", err)
		}
		res, err := ig.Ingest(ctx, p.ArchiveRelPath, p.SourceURL)
		if err != nil {
			return err
		}
		if res.Quarantined {
			return nil // handled: the archive is in _quarantine with a log
		}
		ig.log.InfoContext(ctx, "ingested archive",
			"pack", res.PackRelPath, "files", res.FilesWritten, "flattened", res.Flattened)
		if afterIngest != nil {
			return afterIngest(ctx)
		}
		return nil
	})
}

// Payload is the ingest job's arguments.
type Payload struct {
	ArchiveRelPath string `json:"archive_rel_path"`
	SourceURL      string `json:"source_url,omitempty"`
}

// Enqueue queues one archive for ingest, deduplicated on its path so a poller
// that sees the same file twice does not ingest it twice.
func Enqueue(ctx context.Context, q *jobs.Queue, p Payload) (int64, error) {
	return q.Enqueue(ctx, JobType, p, jobs.EnqueueOptions{
		Priority:  50,
		DedupeKey: JobType + ":" + p.ArchiveRelPath,
	})
}

// uniquePath appends -1, -2, ... before the extension until the path is free, so
// two archives of the same name do not clobber each other in _archives.
func uniquePath(path string) string {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	stem := strings.TrimSuffix(path, ext)
	for i := 1; i < 10000; i++ {
		candidate := fmt.Sprintf("%s-%d%s", stem, i, ext)
		if _, err := os.Lstat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
	return path
}

// hashFile returns the sha256 (hex) and byte size of a file.
func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

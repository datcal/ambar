// Package index owns the database side of the library: reconciling a filesystem
// walk against the stored index, and the read queries the UI needs.
//
// The reconciliation in this file is the highest-risk code in M1. Two rules from
// the spec are absolute:
//
//   - §12: a file that has gone missing is MARKED, never deleted, "because a NAS
//     share can be temporarily unmounted and destroying the index over that would
//     be catastrophic". There is no DELETE on assets anywhere in this package.
//   - §9.1 rule 2: a file with the same content at a new path is a MOVE, not a
//     duplicate. "Never report it as one; update the row."
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/library"
)

// MassMissingWarnRatio is the fraction of the index that going missing in one
// scan triggers a prominent warning. Not a refusal — a genuine bulk deletion over
// SMB is legitimate and the human is entitled to do it — but it is worth saying
// loudly, because the innocent explanation is rarer than the alarming one.
const MassMissingWarnRatio = 0.20

// Indexer reconciles the filesystem into the database.
type Indexer struct {
	db      *db.DB
	root    string
	ignore  *library.Matcher
	buckets []string
	log     *slog.Logger
	now     func() time.Time

	// Pack rows for the scan in progress, filled by applyPacks before any asset
	// write so inserts need no per-row lookup. Not safe for concurrent scans,
	// which is fine: writes go through the single writer connection (§4) and the
	// CLI runs one scan at a time.
	packIDs   map[string]int64
	packNames map[string]string
}

// Options configures an Indexer.
type Options struct {
	// Root is the absolute library root, already validated by config.
	Root string
	// Ignore supplies the §5.1 junk rules. Defaults to library.MustMatcher().
	Ignore *library.Matcher
	// Buckets overrides library.DefaultBucketNames.
	Buckets []string
	Log     *slog.Logger
}

func New(database *db.DB, opts Options) *Indexer {
	ignore := opts.Ignore
	if ignore == nil {
		ignore = library.MustMatcher()
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Indexer{
		db:      database,
		root:    opts.Root,
		ignore:  ignore,
		buckets: opts.Buckets,
		log:     log,
		now:     time.Now,
	}
}

// WithClock replaces the clock, for tests.
func (ix *Indexer) WithClock(now func() time.Time) *Indexer {
	ix.now = now
	return ix
}

// ScanOptions configures one scan.
type ScanOptions struct {
	// DryRun reports what would change without writing anything.
	DryRun bool
	// ReadDimensions reads image headers for width and height (§5 step 4).
	ReadDimensions bool

	// Progress is called as the scan works through the library, so the UI can say
	// "2431 / 20000 files" instead of "running" for four minutes (M16). Nil is fine and is
	// what the CLI passes; the queue's jobs.Reporter throttles the writes.
	Progress func(done, total int64, note string)
}

// report calls the progress hook if there is one.
func (o ScanOptions) report(done, total int64, note string) {
	if o.Progress != nil {
		o.Progress(done, total, note)
	}
}

// ScanReport is the outcome of one scan, and what the CLI prints.
type ScanReport struct {
	DryRun bool

	PacksFound int
	PacksNew   int

	FilesSeen      int
	Added          int
	Unchanged      int
	MetadataOnly   int // (size, mtime) moved but the bytes are identical
	ContentChanged int // same path, different bytes — §12 wants these flagged
	Moved          int // §9.1 rule 2: same bytes, new path
	MarkedMissing  int
	Reappeared     int

	// Asset groups (§5.1). MultiVariantGroups is the interesting number: each one
	// would otherwise be several grid rows for the same artwork.
	Groups             int
	MultiVariantGroups int

	IgnoredJunk int
	// SkippedNonAssets counts files that exist but are not artwork — readmes,
	// licences, archives, dotfiles. Reported so "why is my file not in the grid" has
	// an answer in the scan summary rather than in the source.
	SkippedNonAssets int
	Buckets          []string
	ReservedSkipped  []string

	Hashed   int
	Errors   []error
	Duration time.Duration
}

// existingAsset is the stored state of one row, as loaded for reconciliation.
type existingAsset struct {
	id           int64
	packRelPath  string
	relPath      string
	size         int64
	mtime        int64
	sha256       string
	missingSince sql.NullInt64
}

// Scan walks the library and reconciles it into the index.
//
// Three phases, because move detection is impossible in one: a row's path being
// absent cannot be established until the whole walk has finished.
func (ix *Indexer) Scan(ctx context.Context, opts ScanOptions) (*ScanReport, error) {
	start := ix.now()
	report := &ScanReport{DryRun: opts.DryRun}

	// --- phase 1: walk ---

	walked, err := library.Walk(library.WalkOptions{
		Root:           ix.root,
		Ignore:         ix.ignore,
		Buckets:        ix.buckets,
		ReadDimensions: opts.ReadDimensions,
	})
	if err != nil {
		return nil, err
	}
	report.PacksFound = len(walked.Packs)
	report.FilesSeen = len(walked.Files)
	report.IgnoredJunk = walked.IgnoredCount
	report.SkippedNonAssets = walked.SkippedNonAssets
	report.Buckets = walked.Buckets
	report.ReservedSkipped = walked.ReservedSkipped
	report.Errors = append(report.Errors, walked.Errors...)

	existing, err := ix.loadExisting(ctx)
	if err != nil {
		return nil, err
	}

	// The unmounted-share guard. §12 calls destroying the index over a
	// temporarily-absent share catastrophic; marking every asset missing is not
	// destruction, but it is alarming, wrecks the grid, and is never what the
	// operator wanted. Refusing costs nothing, because a genuinely emptied library
	// can be re-scanned after the operator confirms it.
	if len(walked.Files) == 0 && len(existing) > 0 {
		return nil, fmt.Errorf(
			"refusing to scan: the library at %s contains no indexable files, but the index holds %d assets. "+
				"This is what an unmounted or wrong AMBAR_LIBRARY_ROOT looks like. Check the mount; "+
				"if the library really is empty, the index is left untouched and nothing is lost",
			ix.root, len(existing))
	}

	// --- phase 2: reconcile what was seen against what is stored ---

	type newFile struct {
		file   library.File
		sha256 string
	}
	var (
		fresh   []newFile             // seen but not in the index
		updates []func(*sql.Tx) error // deferred writes, applied in one transaction
		seen    = make(map[string]bool, len(walked.Files))
	)

	now := ix.now().Unix()

	total := int64(len(walked.Files))
	for i, f := range walked.Files {
		// Hashing is the slow part, and it is in this loop, so this is where the number the
		// operator is waiting on lives.
		opts.report(int64(i), total, "checking files")

		key := libraryPath(f.PackRelPath, f.RelPath)
		seen[key] = true

		prior, known := existing[key]
		if !known {
			sum, err := hashFile(f.AbsPath)
			if err != nil {
				report.Errors = append(report.Errors, err)
				continue
			}
			report.Hashed++
			fresh = append(fresh, newFile{file: f, sha256: sum})
			continue
		}

		// The fast path: size and mtime unchanged means the bytes are unchanged.
		// §12 assigns actual re-hashing to `ambar verify`, precisely so a routine
		// scan does not read tens of GB.
		if prior.size == f.Size && prior.mtime == f.ModTime {
			report.Unchanged++
			if prior.missingSince.Valid {
				report.Reappeared++
				id := prior.id
				updates = append(updates, func(tx *sql.Tx) error {
					return clearMissing(ctx, tx, id, now)
				})
			}
			continue
		}

		// Something changed; now it is worth hashing.
		sum, err := hashFile(f.AbsPath)
		if err != nil {
			report.Errors = append(report.Errors, err)
			continue
		}
		report.Hashed++

		file, id, contentChanged := f, prior.id, sum != prior.sha256
		if contentChanged {
			report.ContentChanged++
		} else {
			report.MetadataOnly++
		}
		if prior.missingSince.Valid {
			report.Reappeared++
		}
		updates = append(updates, func(tx *sql.Tx) error {
			return updateAssetInPlace(ctx, tx, id, file, sum, contentChanged, now)
		})
	}

	// --- phase 3: absent rows, and the moves hiding among them ---

	// Each phase says what it is doing, because the file count only describes phase 2. On a
	// library where nothing changed, phase 2 is over in a moment and the remaining seconds are
	// spent here and in regrouping — which used to be reported as "running" and nothing else.
	// Zero total, not total/total: this phase has no count of its own, and reporting 100% while
	// it is still working — move detection hashes files, so it is where the seconds go on a
	// library where nothing changed — would be a bar that lies.
	opts.report(0, 0, "matching moved files")

	var absent []existingAsset
	for key, prior := range existing {
		if seen[key] {
			continue
		}
		if prior.missingSince.Valid {
			// Already known to be missing; nothing to report or change.
			continue
		}
		absent = append(absent, prior)
	}

	// §9.1 rule 2: an absent row whose content reappeared elsewhere is a move.
	// Matching by hash, so this is exact rather than heuristic.
	freshByHash := make(map[string][]int, len(fresh))
	for i, nf := range fresh {
		freshByHash[nf.sha256] = append(freshByHash[nf.sha256], i)
	}
	claimed := make([]bool, len(fresh))

	var stillMissing []existingAsset
	for _, prior := range absent {
		candidates := freshByHash[prior.sha256]
		matched := -1
		for _, i := range candidates {
			if !claimed[i] {
				matched = i
				break
			}
		}
		if matched < 0 {
			stillMissing = append(stillMissing, prior)
			continue
		}
		claimed[matched] = true
		report.Moved++

		file, id := fresh[matched].file, prior.id
		sum := fresh[matched].sha256
		updates = append(updates, func(tx *sql.Tx) error {
			return moveAsset(ctx, tx, id, file, sum, now)
		})
	}

	report.MarkedMissing = len(stillMissing)
	for _, prior := range stillMissing {
		id := prior.id
		updates = append(updates, func(tx *sql.Tx) error {
			return markMissing(ctx, tx, id, now)
		})
	}

	if report.MarkedMissing > 0 && len(existing) > 0 {
		if ratio := float64(report.MarkedMissing) / float64(len(existing)); ratio >= MassMissingWarnRatio {
			ix.log.WarnContext(ctx,
				"a large fraction of the index went missing in one scan; nothing was deleted, "+
					"but check that the library mount is complete",
				"marked_missing", report.MarkedMissing,
				"indexed_total", len(existing),
				"ratio", fmt.Sprintf("%.0f%%", ratio*100),
			)
		}
	}

	// Genuinely new files: everything not claimed as a move.
	for i, nf := range fresh {
		if claimed[i] {
			continue
		}
		report.Added++
		file, sum := nf.file, nf.sha256
		updates = append(updates, func(tx *sql.Tx) error {
			return ix.insertAsset(ctx, tx, file, sum, now)
		})
	}

	if opts.DryRun {
		report.Duration = ix.now().Sub(start)
		return report, nil
	}

	// --- apply ---

	// Packs first, so assets have something to reference. Counting new packs
	// needs the pre-existing set.
	priorPacks, _, err := ix.loadPacks(ctx)
	if err != nil {
		return nil, err
	}
	if err := ix.applyPacks(ctx, walked.Packs, now); err != nil {
		return nil, err
	}
	for _, p := range walked.Packs {
		if _, existed := priorPacks[p.RelPath]; !existed {
			report.PacksNew++
		}
	}

	// One transaction for all asset writes. §4 routes every write through the
	// single writer connection, so there is no concurrency to lose here, and a
	// 20k scan must not become 20k transactions.
	opts.report(0, 0, "writing the index")
	if err := ix.applyUpdates(ctx, updates); err != nil {
		return nil, err
	}

	// Grouping is derived from rel_path alone, so it costs no file reads and must run
	// after reconciliation has settled every path (§5.1).
	opts.report(0, 0, "grouping format variants")
	groupStats, err := ix.Regroup(ctx)
	if err != nil {
		return nil, err
	}
	report.Groups = groupStats.Groups
	report.MultiVariantGroups = groupStats.MultiVariant

	report.Duration = ix.now().Sub(start)
	return report, nil
}

// loadExisting reads the whole index, keyed by library-relative path.
//
// At 20k assets this is a few MB, which is the right trade: reconciliation needs
// random access by path, and a query per walked file would be far slower.
func (ix *Indexer) loadExisting(ctx context.Context) (map[string]existingAsset, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT a.id, p.library_rel_path, a.rel_path, a.size, a.mtime, a.sha256, a.missing_since
		FROM assets a
		JOIN packs p ON p.id = a.pack_id`)
	if err != nil {
		return nil, fmt.Errorf("load existing index: %w", err)
	}
	defer rows.Close()

	out := map[string]existingAsset{}
	for rows.Next() {
		var a existingAsset
		if err := rows.Scan(&a.id, &a.packRelPath, &a.relPath, &a.size, &a.mtime,
			&a.sha256, &a.missingSince); err != nil {
			return nil, fmt.Errorf("scan existing asset: %w", err)
		}
		out[libraryPath(a.packRelPath, a.relPath)] = a
	}
	return out, rows.Err()
}

// loadPacks returns the stored packs keyed by library-relative path.
func (ix *Indexer) loadPacks(ctx context.Context) (ids map[string]int64, names map[string]string, err error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `SELECT library_rel_path, id, name FROM packs`)
	if err != nil {
		return nil, nil, fmt.Errorf("load packs: %w", err)
	}
	defer rows.Close()

	ids, names = map[string]int64{}, map[string]string{}
	for rows.Next() {
		var (
			path, name string
			id         int64
		)
		if err := rows.Scan(&path, &id, &name); err != nil {
			return nil, nil, err
		}
		ids[path], names[path] = id, name
	}
	return ids, names, rows.Err()
}

// applyPacks upserts every detected pack and caches the resulting ids.
func (ix *Indexer) applyPacks(ctx context.Context, packs []library.Pack, now int64) error {
	tx, err := ix.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pack upsert: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	for _, p := range packs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO packs (name, slug, kind, library_rel_path,
			                   first_seen_at, last_seen_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(library_rel_path) DO UPDATE SET
			    name         = excluded.name,
			    slug         = excluded.slug,
			    kind         = excluded.kind,
			    last_seen_at = excluded.last_seen_at,
			    updated_at   = excluded.updated_at`,
			p.Name, p.Slug, p.Kind, p.RelPath, now, now, now, now); err != nil {
			return fmt.Errorf("upsert pack %s: %w", p.RelPath, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pack upsert: %w", err)
	}

	// Cache ids and names for insertAsset and its FTS row.
	ids, names, err := ix.loadPacks(ctx)
	if err != nil {
		return err
	}
	ix.packIDs, ix.packNames = ids, names
	return nil
}

// applyUpdates runs every deferred write in one transaction.
func (ix *Indexer) applyUpdates(ctx context.Context, updates []func(*sql.Tx) error) error {
	if len(updates) == 0 {
		return nil
	}
	tx, err := ix.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin index update: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op once committed

	for _, apply := range updates {
		if err := apply(tx); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit index update: %w", err)
	}
	return nil
}

// insertAsset adds a new row and its FTS entry.
func (ix *Indexer) insertAsset(ctx context.Context, tx *sql.Tx, f library.File, sum string, now int64) error {
	packID, ok := ix.packIDs[f.PackRelPath]
	if !ok {
		return fmt.Errorf("insert asset %s: pack %q has no id", f.RelPath, f.PackRelPath)
	}

	var id int64
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO assets (pack_id, rel_path, filename, ext, kind, size, mtime, sha256,
		                    width, height, first_seen_at, last_verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		RETURNING id`,
		packID, f.RelPath, f.Filename, f.Info.Ext, string(f.Info.Kind),
		f.Size, f.ModTime, sum,
		nullableInt(f.Info.Width), nullableInt(f.Info.Height),
		now, now, now, now,
	).Scan(&id); err != nil {
		return fmt.Errorf("insert asset %s: %w", f.RelPath, err)
	}

	return insertFTS(ctx, tx, id, f.Filename, ix.packNames[f.PackRelPath])
}

// updateAssetInPlace records a file whose path is unchanged but whose size, mtime
// or content is not.
func updateAssetInPlace(ctx context.Context, tx *sql.Tx, id int64, f library.File,
	sum string, contentChanged bool, now int64) error {

	// content_changed_at is only set when the bytes actually differ. §12 wants
	// those "flagged for review"; a touched mtime with identical content is not
	// interesting and must not raise a flag.
	var contentChangedAt any
	if contentChanged {
		contentChangedAt = now
	}

	_, err := tx.ExecContext(ctx, `
		UPDATE assets SET
		    size               = ?,
		    mtime              = ?,
		    sha256             = ?,
		    width              = ?,
		    height             = ?,
		    last_verified_at   = ?,
		    missing_since      = NULL,
		    content_changed_at = COALESCE(?, content_changed_at),
		    updated_at         = ?
		WHERE id = ?`,
		f.Size, f.ModTime, sum,
		nullableInt(f.Info.Width), nullableInt(f.Info.Height),
		now, contentChangedAt, now, id)
	if err != nil {
		return fmt.Errorf("update asset %d: %w", id, err)
	}
	return nil
}

// moveAsset re-points an existing row at its new location (§9.1 rule 2).
//
// The row keeps its id, so anything referring to this asset — and from M9, the
// project_uses rows that make it undeletable — survives the move intact. That is
// the whole reason a move must not be modelled as a delete plus an insert.
func moveAsset(ctx context.Context, tx *sql.Tx, id int64, f library.File, sum string, now int64) error {
	var packID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT id FROM packs WHERE library_rel_path = ?`, f.PackRelPath).Scan(&packID); err != nil {
		return fmt.Errorf("move asset %d: find pack %q: %w", id, f.PackRelPath, err)
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE assets SET
		    pack_id          = ?,
		    rel_path         = ?,
		    filename         = ?,
		    ext              = ?,
		    kind             = ?,
		    size             = ?,
		    mtime            = ?,
		    width            = ?,
		    height           = ?,
		    last_verified_at = ?,
		    missing_since    = NULL,
		    updated_at       = ?
		WHERE id = ?`,
		packID, f.RelPath, f.Filename, f.Info.Ext, string(f.Info.Kind),
		f.Size, f.ModTime,
		nullableInt(f.Info.Width), nullableInt(f.Info.Height),
		now, now, id); err != nil {
		return fmt.Errorf("move asset %d: %w", id, err)
	}

	// The filename and pack name may both have changed, so the FTS row is
	// rewritten rather than patched.
	var packName string
	if err := tx.QueryRowContext(ctx,
		`SELECT name FROM packs WHERE id = ?`, packID).Scan(&packName); err != nil {
		return fmt.Errorf("move asset %d: read pack name: %w", id, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM assets_fts WHERE rowid = ?`, id); err != nil {
		return fmt.Errorf("move asset %d: clear fts: %w", id, err)
	}
	return insertFTS(ctx, tx, id, f.Filename, packName)
}

// markMissing records that a file is gone. It never deletes (§12).
func markMissing(ctx context.Context, tx *sql.Tx, id int64, now int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE assets SET missing_since = ?, updated_at = ?
		WHERE id = ? AND missing_since IS NULL`, now, now, id); err != nil {
		return fmt.Errorf("mark asset %d missing: %w", id, err)
	}
	return nil
}

// clearMissing records that a file is back.
func clearMissing(ctx context.Context, tx *sql.Tx, id int64, now int64) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE assets SET missing_since = NULL, last_verified_at = ?, updated_at = ?
		WHERE id = ?`, now, now, id); err != nil {
		return fmt.Errorf("clear missing on asset %d: %w", id, err)
	}
	return nil
}

// insertFTS writes the search row. tag_text and notes stay empty until M3 and M4.
func insertFTS(ctx context.Context, tx *sql.Tx, id int64, filename, packName string) error {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO assets_fts (rowid, filename, pack_name, tag_text, notes)
		VALUES (?, ?, ?, '', '')`, id, filename, packName); err != nil {
		return fmt.Errorf("index asset %d for search: %w", id, err)
	}
	return nil
}

// hashFile computes the sha256 of a file's contents.
func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", fmt.Errorf("hash %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// libraryPath joins a pack's path with a pack-relative path. It is the identity of
// a file within the library, and the key reconciliation matches on.
func libraryPath(packRelPath, relPath string) string {
	if packRelPath == "." || packRelPath == "" {
		return relPath
	}
	return packRelPath + "/" + relPath
}

// nullableInt maps 0 to SQL NULL, because an unknown dimension is not a zero one.
func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

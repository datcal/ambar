package removal

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/datcal/ambar/internal/audit"
	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/safepath"
)

// ManifestName is the JSON record written into every trash batch: where each file
// came from, why it went, and who asked (§9.1 "with a JSON record of where it came
// from and why"). It is the source of truth for a restore — the database is only
// an index, and a restore has to work even after `rebuild-index`.
const ManifestName = "ambar-trash.json"

// Batch states. A batch is written before anything moves, so a crash mid-batch
// leaves a readable record of intent rather than orphaned files.
const (
	// BatchPlanned means the manifest was written but the moves had not finished.
	BatchPlanned = "planned"
	// BatchApplied means every entry reached its outcome, success or failure.
	BatchApplied = "applied"
)

// Entry is one moved path inside a batch.
type Entry struct {
	Root Root   `json:"root"`
	Path string `json:"path"` // original path, relative to Root
	// TrashPath is where it now lives, relative to the batch directory. Empty when
	// the move failed, or for a link op, which moves nothing.
	TrashPath string  `json:"trash_path,omitempty"`
	Action    Action  `json:"action"`
	KeepPath  string  `json:"keep_path,omitempty"`
	Finding   string  `json:"finding,omitempty"`
	Bytes     int64   `json:"bytes"`
	Files     int     `json:"files"`
	IsDir     bool    `json:"is_dir,omitempty"`
	AssetIDs  []int64 `json:"asset_ids,omitempty"`
	// Error is why this entry did not happen. A batch with some failed entries is
	// normal — one unreadable file must not abandon the rest — and saying so per
	// entry is how the user finds out.
	Error string `json:"error,omitempty"`
	// RestoredAt is set when the entry has been put back.
	RestoredAt int64 `json:"restored_at,omitempty"`
}

// Done reports whether the entry actually happened.
func (e Entry) Done() bool { return e.Error == "" }

// Restored reports whether the entry has been put back where it came from.
func (e Entry) Restored() bool { return e.RestoredAt > 0 }

// Batch is one applied Plan, recorded on disk beside the files it moved.
type Batch struct {
	ID        string  `json:"id"`
	State     string  `json:"state"`
	Reason    string  `json:"reason,omitempty"`
	Actor     string  `json:"actor,omitempty"`
	CreatedAt int64   `json:"created_at"`
	AppliedAt int64   `json:"applied_at,omitempty"`
	Entries   []Entry `json:"entries"`
}

// CreatedTime and AppliedTime are the timestamps as times, for templates.
func (b Batch) CreatedTime() time.Time { return time.Unix(b.CreatedAt, 0) }

// AppliedTime is CreatedTime for the moment the batch finished.
func (b Batch) AppliedTime() time.Time { return time.Unix(b.AppliedAt, 0) }

// Bytes is how much this batch is holding in the trash, excluding entries that
// failed or have been restored.
func (b Batch) Bytes() int64 {
	var total int64
	for _, e := range b.Entries {
		if e.Done() && !e.Restored() && e.Action == ActionTrash {
			total += e.Bytes
		}
	}
	return total
}

// Restorable is how many entries are still sitting in the trash.
func (b Batch) Restorable() int {
	n := 0
	for _, e := range b.Entries {
		if e.Done() && !e.Restored() && e.Action == ActionTrash {
			n++
		}
	}
	return n
}

// Failed is how many entries did not happen.
func (b Batch) Failed() int {
	n := 0
	for _, e := range b.Entries {
		if !e.Done() {
			n++
		}
	}
	return n
}

// Actor is who asked, for the audit log and the trash record.
type Actor struct {
	UserID   *int64
	Username string
	IP       string
}

// Result is what an Apply did.
type Result struct {
	BatchID string
	// Applied and Failed count entries, not paths in the plan: a directory entry is
	// one entry however many files it contained.
	Applied int
	Failed  int
	Bytes   int64
	Batch   *Batch
}

// Executor carries out a Plan. It performs no policy: everything it acts on was
// already accepted by the Planner, and it re-checks paths only to defend against
// a plan that travelled through a job payload (invariant 9).
type Executor struct {
	db          *db.DB
	libraryRoot string
	dataRoot    string
	trashDir    string
	linkMode    string
	audit       *audit.Logger
	log         *slog.Logger
	now         func() time.Time
}

// NewExecutor builds an Executor. audit may be nil in tests; linkMode is
// AMBAR_DEDUPE_LINK_MODE (reflink | hardlink | off).
func NewExecutor(database *db.DB, libraryRoot, dataRoot, trashDir, linkMode string, auditLog *audit.Logger, log *slog.Logger) *Executor {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if linkMode == "" {
		linkMode = "off"
	}
	return &Executor{
		db:          database,
		libraryRoot: libraryRoot,
		dataRoot:    dataRoot,
		trashDir:    trashDir,
		linkMode:    linkMode,
		audit:       auditLog,
		log:         log,
		now:         time.Now,
	}
}

// WithClock replaces the clock, for tests.
func (e *Executor) WithClock(now func() time.Time) *Executor { e.now = now; return e }

// LinkMode is the configured dedupe link mode.
func (e *Executor) LinkMode() string { return e.linkMode }

// Apply performs a Plan and returns what happened.
//
// The order is deliberate: the manifest is written *before* the first file moves,
// so that a crash halfway through leaves a record naming every path that was
// about to move. Recovering from an interrupted batch is then a matter of reading
// one JSON file, which is exactly what a restore already does.
func (e *Executor) Apply(ctx context.Context, plan *Plan, actor Actor) (*Result, error) {
	if plan == nil || plan.Empty() {
		return nil, errors.New("nothing to apply: the plan is empty")
	}
	if e.trashDir == "" {
		return nil, errors.New("no trash directory is configured")
	}

	now := e.now()
	batch := &Batch{
		ID:        batchID(now),
		State:     BatchPlanned,
		Reason:    plan.Reason,
		Actor:     actor.Username,
		CreatedAt: now.Unix(),
	}
	for _, op := range plan.Ops {
		batch.Entries = append(batch.Entries, Entry{
			Root: op.Root, Path: op.Path, Action: op.Action, KeepPath: op.KeepPath,
			Finding: op.Finding, Bytes: op.Bytes, Files: op.Files, IsDir: op.IsDir,
			AssetIDs: op.AssetIDs,
		})
	}

	batchDir := filepath.Join(e.trashDir, batch.ID)
	if err := os.MkdirAll(batchDir, 0o755); err != nil {
		return nil, fmt.Errorf("create trash batch directory: %w", err)
	}
	if err := writeManifest(batchDir, batch); err != nil {
		return nil, err
	}

	result := &Result{BatchID: batch.ID}
	for i := range batch.Entries {
		entry := &batch.Entries[i]
		if err := ctx.Err(); err != nil {
			entry.Error = err.Error()
			continue
		}

		var err error
		switch entry.Action {
		case ActionTrash:
			err = e.trash(batchDir, entry)
		case ActionLink:
			err = e.link(entry)
		default:
			err = fmt.Errorf("unknown action %q", entry.Action)
		}
		if err != nil {
			entry.Error = err.Error()
			result.Failed++
			e.log.ErrorContext(ctx, "removal entry failed",
				"batch", batch.ID, "path", entry.Path, "action", entry.Action, "error", err)
			continue
		}

		result.Applied++
		result.Bytes += entry.Bytes
		e.markAssets(ctx, entry)
		e.record(ctx, batch, *entry, actor)
	}

	batch.State = BatchApplied
	batch.AppliedAt = e.now().Unix()
	if err := writeManifest(batchDir, batch); err != nil {
		// The files have already moved; failing here would report a removal as not
		// having happened. Log loudly instead — the manifest of intent is still on
		// disk, so nothing is unrecoverable.
		e.log.ErrorContext(ctx, "could not finalise trash manifest", "batch", batch.ID, "error", err)
	}
	result.Batch = batch
	return result, nil
}

// trash moves one entry into the batch directory, preserving its relative path so
// a restore is unambiguous (§9.1).
func (e *Executor) trash(batchDir string, entry *Entry) error {
	root := e.rootFor(entry.Root)
	if root == "" {
		return fmt.Errorf("root %q is not configured", entry.Root)
	}
	// Re-resolved rather than trusted: this entry may have arrived as a job payload
	// since the Planner saw it.
	src, err := safepath.ResolveExisting(root, entry.Path)
	if err != nil {
		return err
	}
	if safepath.IsWithin(e.trashDir, src) {
		return errors.New("refusing to move a path that is already inside the trash")
	}

	relInBatch := filepath.Join(string(entry.Root), filepath.FromSlash(entry.Path))
	dest, err := safepath.Resolve(batchDir, filepath.ToSlash(relInBatch))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("create trash parent: %w", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		// Cannot happen with a timestamped batch id, and if it somehow does, moving
		// on top of it would destroy the earlier copy.
		return fmt.Errorf("%s already exists in the trash batch", relInBatch)
	}

	if err := os.Rename(src, dest); err != nil {
		// A rename across devices fails with EXDEV. That is the normal case when
		// AMBAR_TRASH_DIR is on a different volume from the library, so copy and
		// only then remove the source: the copy is fsynced first, so a crash can
		// leave two copies but never zero.
		if !isCrossDevice(err) {
			return fmt.Errorf("move to trash: %w", err)
		}
		if err := copyTree(src, dest); err != nil {
			return fmt.Errorf("copy to trash: %w", err)
		}
		if err := os.RemoveAll(src); err != nil {
			return fmt.Errorf("remove original after copying to trash: %w", err)
		}
	}

	entry.TrashPath = filepath.ToSlash(relInBatch)
	return nil
}

// markAssets records in the index that the files are gone. The filesystem stays
// the source of truth (invariant 2): this is the same missing_since a scan would
// set, applied now so the grid stops offering a file that has moved to the trash.
func (e *Executor) markAssets(ctx context.Context, entry *Entry) {
	if entry.Action != ActionTrash || len(entry.AssetIDs) == 0 {
		return
	}
	now := e.now().Unix()
	for _, id := range entry.AssetIDs {
		if _, err := e.db.Writer.ExecContext(ctx, `
			UPDATE assets SET missing_since = ?, updated_at = ?
			WHERE id = ? AND missing_since IS NULL`, now, now, id); err != nil {
			e.log.ErrorContext(ctx, "could not mark trashed asset missing", "asset", id, "error", err)
		}
	}
}

// record writes the §9.1 audit entry: every removal, with the reason and the
// finding that motivated it.
func (e *Executor) record(ctx context.Context, batch *Batch, entry Entry, actor Actor) {
	if e.audit == nil {
		return
	}
	action := audit.ActionRemovalTrashed
	if entry.Action == ActionLink {
		action = audit.ActionRemovalLinked
	}
	e.audit.Record(ctx, audit.Entry{
		UserID:   actor.UserID,
		Action:   action,
		Entity:   "path",
		EntityID: string(entry.Root) + ":" + entry.Path,
		IP:       actor.IP,
		Detail: map[string]any{
			"batch":      batch.ID,
			"reason":     batch.Reason,
			"finding":    entry.Finding,
			"bytes":      entry.Bytes,
			"files":      entry.Files,
			"trash_path": entry.TrashPath,
			"keep_path":  entry.KeepPath,
			"asset_ids":  entry.AssetIDs,
		},
	})
}

func (e *Executor) rootFor(r Root) string {
	switch r {
	case RootLibrary:
		return e.libraryRoot
	case RootData:
		return e.dataRoot
	default:
		return ""
	}
}

// --- reading the trash ------------------------------------------------------

// ListBatches reads every batch manifest, newest first. A directory without a
// readable manifest is skipped rather than guessed at.
func (e *Executor) ListBatches() ([]*Batch, error) {
	entries, err := os.ReadDir(e.trashDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read trash directory: %w", err)
	}

	var batches []*Batch
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		batch, err := readManifest(filepath.Join(e.trashDir, ent.Name()))
		if err != nil {
			e.log.Warn("skipping unreadable trash batch", "batch", ent.Name(), "error", err)
			continue
		}
		batches = append(batches, batch)
	}
	sort.Slice(batches, func(i, j int) bool { return batches[i].CreatedAt > batches[j].CreatedAt })
	return batches, nil
}

// Batch reads one batch by id.
func (e *Executor) Batch(id string) (*Batch, error) {
	dir, err := e.batchDir(id)
	if err != nil {
		return nil, err
	}
	return readManifest(dir)
}

// batchDir resolves a batch id to its directory, refusing anything that is not a
// single path segment directly under the trash (invariant 9: the id arrives from
// a URL).
func (e *Executor) batchDir(id string) (string, error) {
	if id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", fmt.Errorf("invalid trash batch id %q", id)
	}
	dir, err := safepath.ResolveExisting(e.trashDir, id)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("trash batch %q not found", id)
	}
	return dir, nil
}

// --- restore ----------------------------------------------------------------

// Restore puts entries back where they came from. paths selects which entries by
// their original path; an empty selection restores every outstanding entry in the
// batch.
//
// A restore never overwrites: if something occupies the original path now, the
// entry is left in the trash and reported, because guessing which copy the user
// wanted is exactly the kind of decision this package does not make.
func (e *Executor) Restore(ctx context.Context, id string, paths []string, actor Actor) (restored int, failures map[string]string, err error) {
	dir, err := e.batchDir(id)
	if err != nil {
		return 0, nil, err
	}
	batch, err := readManifest(dir)
	if err != nil {
		return 0, nil, err
	}

	wanted := map[string]struct{}{}
	for _, p := range paths {
		wanted[strings.Trim(strings.TrimSpace(filepath.ToSlash(p)), "/")] = struct{}{}
	}

	failures = map[string]string{}
	now := e.now().Unix()
	for i := range batch.Entries {
		entry := &batch.Entries[i]
		if entry.Action != ActionTrash || !entry.Done() || entry.Restored() {
			continue
		}
		if len(wanted) > 0 {
			if _, ok := wanted[entry.Path]; !ok {
				continue
			}
		}
		if err := e.restoreEntry(dir, *entry); err != nil {
			failures[entry.Path] = err.Error()
			continue
		}
		entry.RestoredAt = now
		restored++

		// The file is back, so the index rows are live again. A scan would work this
		// out too; doing it here means the asset reappears in the grid immediately.
		for _, assetID := range entry.AssetIDs {
			if _, err := e.db.Writer.ExecContext(ctx, `
				UPDATE assets SET missing_since = NULL, updated_at = ? WHERE id = ?`,
				now, assetID); err != nil {
				e.log.ErrorContext(ctx, "could not clear missing_since on restore", "asset", assetID, "error", err)
			}
		}
		if e.audit != nil {
			e.audit.Record(ctx, audit.Entry{
				UserID: actor.UserID, Action: audit.ActionTrashRestored, Entity: "path",
				EntityID: string(entry.Root) + ":" + entry.Path, IP: actor.IP,
				Detail: map[string]any{"batch": batch.ID, "bytes": entry.Bytes},
			})
		}
	}

	if restored > 0 {
		if err := writeManifest(dir, batch); err != nil {
			return restored, failures, err
		}
	}
	return restored, failures, nil
}

// restoreEntry moves one entry back out of the trash.
func (e *Executor) restoreEntry(batchDir string, entry Entry) error {
	root := e.rootFor(entry.Root)
	if root == "" {
		return fmt.Errorf("root %q is not configured", entry.Root)
	}
	// The manifest is a file on disk that a person could have edited, so its paths
	// get the same treatment as any other untrusted input.
	src, err := safepath.ResolveExisting(batchDir, entry.TrashPath)
	if err != nil {
		return err
	}
	dest, err := safepath.Resolve(root, entry.Path)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists; restore it by hand or move the new file aside", entry.Path)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("recreate parent directory: %w", err)
	}
	if err := os.Rename(src, dest); err != nil {
		if !isCrossDevice(err) {
			return fmt.Errorf("restore: %w", err)
		}
		if err := copyTree(src, dest); err != nil {
			return fmt.Errorf("restore copy: %w", err)
		}
		if err := os.RemoveAll(src); err != nil {
			return fmt.Errorf("restore cleanup: %w", err)
		}
	}
	return nil
}

// --- purge ------------------------------------------------------------------

// PurgeReport is what a purge did.
type PurgeReport struct {
	Batches []string
	Bytes   int64
	Kept    int
}

// Purge deletes trash batches that finished before cutoff, permanently.
//
// This is the one irreversible operation in Ambar, so it is never automatic and
// never scheduled (§9.1 rules out "any automatic purging of trash, including under
// low-disk conditions"): the caller is `ambar trash purge` or an explicit button,
// and it must pass the cutoff it computed from AMBAR_TRASH_RETENTION. A zero
// cutoff purges nothing rather than everything, because "no retention configured"
// must never read as "delete it all".
func (e *Executor) Purge(ctx context.Context, cutoff time.Time, actor Actor) (PurgeReport, error) {
	var report PurgeReport
	if cutoff.IsZero() {
		return report, errors.New("purge needs an explicit cutoff; refusing to purge everything")
	}

	batches, err := e.ListBatches()
	if err != nil {
		return report, err
	}
	for _, batch := range batches {
		at := batch.AppliedAt
		if at == 0 {
			at = batch.CreatedAt
		}
		// Never anything younger than the retention window, whatever the pressure.
		if at >= cutoff.Unix() {
			report.Kept++
			continue
		}
		dir, err := e.batchDir(batch.ID)
		if err != nil {
			return report, err
		}
		bytes := batch.Bytes()
		if err := os.RemoveAll(dir); err != nil {
			return report, fmt.Errorf("purge batch %s: %w", batch.ID, err)
		}
		report.Batches = append(report.Batches, batch.ID)
		report.Bytes += bytes
		if e.audit != nil {
			e.audit.Record(ctx, audit.Entry{
				UserID: actor.UserID, Action: audit.ActionTrashPurged, Entity: "trash_batch",
				EntityID: batch.ID, IP: actor.IP,
				Detail: map[string]any{"bytes": bytes, "entries": len(batch.Entries), "cutoff": cutoff.Unix()},
			})
		}
		e.log.InfoContext(ctx, "purged trash batch", "batch", batch.ID, "bytes", bytes)
	}
	return report, nil
}

// --- manifest and filesystem plumbing ---------------------------------------

// batchID is a sortable UTC timestamp plus a short random suffix, so two batches
// started in the same second cannot collide.
func batchID(at time.Time) string {
	var suffix [3]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		// crypto/rand does not fail in practice; a fixed suffix would still be
		// distinguishable by the timestamp.
		return at.UTC().Format("20060102T150405Z") + "-000000"
	}
	return at.UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix[:])
}

// writeManifest writes the record atomically: a half-written manifest would be a
// batch whose contents cannot be restored.
func writeManifest(batchDir string, batch *Batch) error {
	data, err := json.MarshalIndent(batch, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal trash manifest: %w", err)
	}
	tmp, err := os.CreateTemp(batchDir, ".tmp-manifest-*")
	if err != nil {
		return fmt.Errorf("create temp manifest: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) //nolint:errcheck // no-op once renamed

	if _, err := tmp.Write(data); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("write trash manifest: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return fmt.Errorf("sync trash manifest: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close trash manifest: %w", err)
	}
	if err := os.Rename(tmpName, filepath.Join(batchDir, ManifestName)); err != nil {
		return fmt.Errorf("rename trash manifest into place: %w", err)
	}
	return nil
}

func readManifest(batchDir string) (*Batch, error) {
	data, err := os.ReadFile(filepath.Join(batchDir, ManifestName))
	if err != nil {
		return nil, fmt.Errorf("read trash manifest: %w", err)
	}
	var batch Batch
	if err := json.Unmarshal(data, &batch); err != nil {
		return nil, fmt.Errorf("parse trash manifest: %w", err)
	}
	if batch.ID == "" {
		batch.ID = filepath.Base(batchDir)
	}
	return &batch, nil
}

// isCrossDevice reports whether a rename failed because source and destination are
// on different filesystems — the expected case for a trash directory on another
// volume, and the only rename failure worth falling back from.
func isCrossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }

// copyTree copies a file or a whole directory, syncing each file before the
// original is removed. Slow by design: it only runs when a rename cannot.
func copyTree(src, dest string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dest, info.Mode())
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		entryInfo, err := d.Info()
		if err != nil {
			return err
		}
		if !entryInfo.Mode().IsRegular() {
			// Sockets, devices and dangling links have no meaningful copy. Skipping
			// them means RemoveAll will drop them, which for junk is the point.
			return nil
		}
		return copyFile(path, target, entryInfo.Mode())
	})
}

func copyFile(src, dest string, mode fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close() //nolint:errcheck

	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode.Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	// Sync before the caller removes the original: this is the moment where a crash
	// could otherwise lose the only copy.
	if err := out.Sync(); err != nil {
		out.Close() //nolint:errcheck
		return err
	}
	return out.Close()
}

package derive

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/jobs"
	"github.com/datcal/ambar/internal/palette"
	"github.com/datcal/ambar/internal/safepath"
)

// JobType is the queue type name for a single asset's derivatives.
const JobType = "asset.derive"

// Derive states, matching the comment in 0003_derive.sql.
const (
	StatePending      = "pending"
	StateOK           = "ok"
	StateFailed       = "failed"
	StateUnsupported  = "unsupported"
	StateNeedsBlender = "needs_blender" // M6: FBX/.blend, until Blender is installed
)

// Payload is the job payload. Just the asset id: everything else is read fresh from
// the row, so a job queued before a rescan still does the right thing.
type Payload struct {
	AssetID int64 `json:"asset_id"`
}

// Deriver runs derivative jobs against the index.
type Deriver struct {
	db          *db.DB
	libraryRoot string
	dataRoot    string
	blenderBin  string
	maxPixels   int64
	log         *slog.Logger
}

// Options configures a Deriver.
type Options struct {
	LibraryRoot string
	DataRoot    string
	// MaxPixels is AMBAR_MAX_IMAGE_PIXELS. Zero uses DefaultMaxPixels.
	MaxPixels int64
	// BlenderBin is AMBAR_BLENDER_BIN, the optional external Blender CLI (§6).
	BlenderBin string
	Log        *slog.Logger
}

func New(database *db.DB, opts Options) *Deriver {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	maxPixels := opts.MaxPixels
	if maxPixels <= 0 {
		maxPixels = DefaultMaxPixels
	}
	return &Deriver{
		db:          database,
		libraryRoot: opts.LibraryRoot,
		dataRoot:    opts.DataRoot,
		blenderBin:  opts.BlenderBin,
		maxPixels:   maxPixels,
		log:         log,
	}
}

// Register attaches the handler to a queue.
func (d *Deriver) Register(q *jobs.Queue) {
	q.Register(JobType, d.Handle)
}

// DedupeKey is what makes §6's "idempotent, keyed on sha256 + derive_version, so
// rescans do no work" structural: while a job with this key is pending, an identical
// one cannot be enqueued.
func DedupeKey(sha256hex string) string {
	return fmt.Sprintf("%s:%s:%d", JobType, sha256hex, Version)
}

// Handle derives one asset.
//
// The distinction that matters: a decoding *failure* is retryable and recorded as
// derive_state=failed, while an *unsupported* format is a permanent, expected outcome
// recorded as derive_state=unsupported. Only the former returns an error to the queue,
// so the queue does not burn three attempts on a .xcf that will never decode.
func (d *Deriver) Handle(ctx context.Context, raw []byte) error {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return fmt.Errorf("unmarshal derive payload: %w", err)
	}
	if p.AssetID == 0 {
		return fmt.Errorf("derive payload has no asset id")
	}

	asset, err := d.loadAsset(ctx, p.AssetID)
	if errors.Is(err, sql.ErrNoRows) {
		// The asset was removed between enqueue and run. Nothing to do, and not a
		// failure — this happens whenever a scan enqueues work and the library
		// changes underneath it.
		d.log.DebugContext(ctx, "derive skipped: asset no longer exists", "asset_id", p.AssetID)
		return nil
	}
	if err != nil {
		return err
	}

	// Already current. The dedupe key normally prevents this, but a job queued
	// before a rescan can still arrive after another path already derived the same
	// content.
	if asset.deriveState == StateOK && asset.deriveVersion == Version {
		return nil
	}

	// A missing file cannot be derived, and §12 keeps its row. Recorded as
	// unsupported rather than failed so it does not sit in the failed list forever.
	// By id, not by content: another copy of the same bytes may well be present.
	if asset.missing {
		return d.record(ctx, asset.id, StateUnsupported, "the file was not present at the last scan")
	}

	absPath, err := safepath.ResolveExisting(d.libraryRoot, asset.libraryPath())
	if err != nil {
		// Either the file vanished since the scan, or the stored path escapes the
		// root. The second would be a bug or tampering, so it is logged loudly and
		// never followed.
		d.log.ErrorContext(ctx, "derive could not resolve the asset path",
			"asset_id", asset.id, "rel_path", asset.libraryPath(), "error", err)
		return d.record(ctx, asset.id, StateFailed,
			"the file could not be opened; run `ambar scan` to update the index")
	}

	result, err := Generate(GenerateOptions{
		AbsPath:    absPath,
		Ext:        asset.ext,
		SHA256:     asset.sha256,
		DataRoot:   d.dataRoot,
		MaxPixels:  d.maxPixels,
		BlenderBin: d.blenderBin,
	})

	switch {
	case errors.Is(err, ErrNeedsBlender):
		// Not a failure and not retryable until Blender is installed (§6). Recorded
		// with the reason so the UI can offer to fetch Blender.
		return d.recordForContent(ctx, asset.sha256, StateNeedsBlender, err.Error(), nil)

	case errors.Is(err, ErrUnsupported):
		// Expected and permanent. Recorded with the reason so the UI can say *why*
		// rather than just shrugging, and returning nil keeps it out of the retry
		// path.
		return d.recordForContent(ctx, asset.sha256, StateUnsupported, err.Error(), nil)

	case err != nil:
		// A real failure: corrupt file, unreadable disk, encoder error. Recorded and
		// returned, so the queue retries and §12's failed-job list shows it.
		if recordErr := d.recordForContent(ctx, asset.sha256, StateFailed, err.Error(), nil); recordErr != nil {
			d.log.ErrorContext(ctx, "could not record a derive failure",
				"asset_id", asset.id, "error", recordErr)
		}
		return fmt.Errorf("derive asset %d (%s): %w", asset.id, asset.filename, err)
	}

	if len(result.Notes) > 0 {
		d.log.InfoContext(ctx, "derive completed with notes",
			"asset_id", asset.id, "filename", asset.filename, "notes", strings.Join(result.Notes, "; "))
	}
	return d.recordForContent(ctx, asset.sha256, StateOK, strings.Join(result.Notes, "; "), result)
}

// assetRow is the subset of an asset the deriver needs.
type assetRow struct {
	id          int64
	packRelPath string
	relPath     string
	filename    string
	ext         string
	sha256      string
	missing     bool

	deriveState   string
	deriveVersion int
}

func (a assetRow) libraryPath() string {
	if a.packRelPath == "." || a.packRelPath == "" {
		return a.relPath
	}
	return a.packRelPath + "/" + a.relPath
}

func (d *Deriver) loadAsset(ctx context.Context, id int64) (assetRow, error) {
	var (
		a       assetRow
		missing sql.NullInt64
	)
	err := d.db.Reader.QueryRowContext(ctx, `
		SELECT a.id, p.library_rel_path, a.rel_path, a.filename, a.ext, a.sha256,
		       a.missing_since, a.derive_state, a.derive_version
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		WHERE a.id = ?`, id,
	).Scan(&a.id, &a.packRelPath, &a.relPath, &a.filename, &a.ext, &a.sha256,
		&missing, &a.deriveState, &a.deriveVersion)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return a, err
		}
		return a, fmt.Errorf("load asset %d: %w", id, err)
	}
	a.missing = missing.Valid
	return a, nil
}

// record writes the outcome onto one asset row, by id.
//
// Used for outcomes that are specific to *this* asset rather than to its content — a
// missing file, an unresolvable path.
//
// derive_version is set even for unsupported and failed states, so a rescan does not
// re-attempt every .xcf in the library on every pass. Bumping Version is what forces
// a retry of everything, which is exactly the §4 intent.
func (d *Deriver) record(ctx context.Context, id int64, state, message string) error {
	now := time.Now().Unix()
	if _, err := d.db.Writer.ExecContext(ctx, `
		UPDATE assets SET derive_state = ?, derive_error = ?, derive_version = ?, updated_at = ?
		WHERE id = ?`, state, truncateError(message), Version, now, id); err != nil {
		return fmt.Errorf("record derive state for asset %d: %w", id, err)
	}
	return nil
}

// recordForContent writes the outcome onto EVERY asset with this content hash.
//
// This is what makes §6's hash-keyed idempotency work end to end. Derivatives live at
// derivatives/<sha>/ and jobs dedupe on the same hash, so two identical files share one
// job — and if that job only updated the asset it happened to be dispatched for, the
// other copy would sit at derive_state=pending forever with no thumbnail in the grid,
// even though the thumbnail was on disk the whole time.
//
// Identical bytes have identical analysis, so applying one result to all of them is
// correct rather than an approximation.
//
// Missing assets are skipped: their state already records that the file is absent, and
// that is more useful than claiming a derivative they cannot show.
func (d *Deriver) recordForContent(ctx context.Context, sha256hex, state, message string,
	result *Result) error {

	now := time.Now().Unix()

	if result == nil {
		if _, err := d.db.Writer.ExecContext(ctx, `
			UPDATE assets SET derive_state = ?, derive_error = ?, derive_version = ?, updated_at = ?
			WHERE sha256 = ? AND missing_since IS NULL`,
			state, truncateError(message), Version, now, sha256hex); err != nil {
			return fmt.Errorf("record derive state for content %s: %w", sha256hex[:8], err)
		}
		return nil
	}

	// Model assets fill the §4 3D columns and render no image (§6).
	if result.Model != nil {
		m := result.Model
		var animationNames any
		if len(m.AnimationNames) > 0 {
			if encoded, err := json.Marshal(m.AnimationNames); err == nil {
				animationNames = string(encoded)
			}
		}
		if _, err := d.db.Writer.ExecContext(ctx, `
			UPDATE assets SET
			    tri_count       = ?,
			    vert_count      = ?,
			    bbox_x          = ?,
			    bbox_y          = ?,
			    bbox_z          = ?,
			    material_count  = ?,
			    animation_names = ?,
			    derive_state    = ?,
			    derive_error    = ?,
			    derive_version  = ?,
			    updated_at      = ?
			WHERE sha256 = ? AND missing_since IS NULL`,
			nullableInt(m.TriCount), nullableInt(m.VertCount),
			m.BBox[0], m.BBox[1], m.BBox[2], nullableInt(m.MaterialCount),
			animationNames, state, truncateError(message), Version, now, sha256hex); err != nil {
			return fmt.Errorf("record model derive for content %s: %w", sha256hex[:8], err)
		}
		return nil
	}

	// Audio assets fill their own columns and render no image (§6).
	if result.Audio != nil {
		a := result.Audio
		if _, err := d.db.Writer.ExecContext(ctx, `
			UPDATE assets SET
			    duration_ms    = ?,
			    sample_rate    = ?,
			    channels       = ?,
			    bit_depth      = ?,
			    peak_dbfs      = ?,
			    is_loopable    = ?,
			    derive_state   = ?,
			    derive_error   = ?,
			    derive_version = ?,
			    updated_at     = ?
			WHERE sha256 = ? AND missing_since IS NULL`,
			nullableInt(a.DurationMS), nullableInt(a.SampleRate), nullableInt(a.Channels),
			nullableInt(a.BitDepth), a.PeakDBFS, boolToInt(a.IsLoopable),
			state, truncateError(message), Version, now, sha256hex); err != nil {
			return fmt.Errorf("record audio derive for content %s: %w", sha256hex[:8], err)
		}
		return nil
	}

	var animationNames any
	if len(result.AnimationNames) > 0 {
		encoded, err := json.Marshal(result.AnimationNames)
		if err != nil {
			return fmt.Errorf("marshal animation names: %w", err)
		}
		animationNames = string(encoded)
	}

	var frameCount, fps any
	if result.FrameCount > 1 {
		frameCount = result.FrameCount
		if result.FPS > 0 {
			fps = result.FPS
		}
	}

	// The colour palette (§8). Stored as JSON alongside a kind label; a nil palette
	// (a decoder that produced no analysis) leaves both columns NULL rather than
	// writing an empty string that would read as "analysed, no colours".
	var paletteJSON, paletteKind any
	if result.Palette != nil {
		swatches := result.Palette.Swatches
		if swatches == nil {
			swatches = []palette.Swatch{}
		}
		encoded, err := json.Marshal(swatches)
		if err != nil {
			return fmt.Errorf("marshal palette: %w", err)
		}
		paletteJSON = string(encoded)
		paletteKind = result.Palette.Kind
	}

	// Spritesheet geometry (§6), when the image was detected as a sheet. The frame
	// count and fps come from the grid so the grid view can animate it.
	//
	// A human's or a sidecar's grid is never overwritten by a re-detection: §6 wants
	// a confirmed value distinguishable from a guess, and a Version bump that
	// re-derives everything must not silently revert corrections.
	var frameW, frameH, frameCols, frameRows, frameSource any
	if s := result.Sheet; s != nil {
		if ew, eh, ec, er, es, ok := d.confirmedFrames(ctx, sha256hex); ok {
			frameW, frameH, frameCols, frameRows, frameSource = ew, eh, ec, er, es
			frameCount = ec * er
		} else {
			frameW, frameH, frameCols, frameRows, frameSource = s.FrameW, s.FrameH, s.Cols, s.Rows, s.Source
			frameCount = s.FrameCount
		}
		if fps == nil {
			fps = float64(SheetFPS)
		}
	}

	if _, err := d.db.Writer.ExecContext(ctx, `
		UPDATE assets SET
		    width               = ?,
		    height              = ?,
		    has_alpha           = ?,
		    has_semitransparent = ?,
		    color_count         = ?,
		    is_pixel_art        = ?,
		    phash               = ?,
		    frame_count         = ?,
		    fps                 = ?,
		    animation_names     = ?,
		    frame_w             = ?,
		    frame_h             = ?,
		    frame_cols          = ?,
		    frame_rows          = ?,
		    frame_source        = ?,
		    palette_json        = ?,
		    palette_kind        = ?,
		    derive_state        = ?,
		    derive_error        = ?,
		    derive_version      = ?,
		    updated_at          = ?
		WHERE sha256 = ? AND missing_since IS NULL`,
		nullableInt(result.Analysis.Width),
		nullableInt(result.Analysis.Height),
		boolToInt(result.Analysis.HasAlpha),
		boolToInt(result.Analysis.HasSemitransparent),
		nullableInt(result.Analysis.ColorCount),
		boolToInt(result.Analysis.IsPixelArt),
		nullableString(result.PHash),
		frameCount,
		fps,
		animationNames,
		frameW,
		frameH,
		frameCols,
		frameRows,
		frameSource,
		paletteJSON,
		paletteKind,
		state,
		truncateError(message),
		Version,
		now,
		sha256hex,
	); err != nil {
		return fmt.Errorf("record derive result for content %s: %w", sha256hex[:8], err)
	}
	return nil
}

// confirmedFrames returns a manual or sidecar frame grid already recorded for
// this content, so a re-detection preserves it rather than reverting it.
func (d *Deriver) confirmedFrames(ctx context.Context, sha256hex string) (fw, fh, cols, rows int, source string, ok bool) {
	var (
		w, h, c, r sql.NullInt64
		src        sql.NullString
	)
	err := d.db.Reader.QueryRowContext(ctx, `
		SELECT frame_w, frame_h, frame_cols, frame_rows, frame_source
		FROM assets
		WHERE sha256 = ? AND frame_source IN ('manual', 'sidecar') AND frame_cols IS NOT NULL
		LIMIT 1`, sha256hex).Scan(&w, &h, &c, &r, &src)
	if err != nil || !c.Valid || !r.Valid {
		return 0, 0, 0, 0, "", false
	}
	return int(w.Int64), int(h.Int64), int(c.Int64), int(r.Int64), src.String, true
}

// EnqueueStale queues a derive job for every asset that needs one.
//
// Called after a scan. Assets already at the current Version are skipped by the query
// rather than by the dedupe key, so a 20,000-asset rescan enqueues nothing at all.
//
// Missing assets are skipped: there is no file to read, and §12 keeps their rows
// indefinitely, so including them would mean permanently re-queueing work that cannot
// succeed.
func EnqueueStale(ctx context.Context, database *db.DB, q *jobs.Queue) (int, error) {
	rows, err := database.Reader.QueryContext(ctx, `
		SELECT id, sha256 FROM assets
		WHERE missing_since IS NULL
		  AND (derive_version < ? OR derive_state = ?)
		ORDER BY id`, Version, StatePending)
	if err != nil {
		return 0, fmt.Errorf("find assets needing derivatives: %w", err)
	}
	defer rows.Close()

	type pending struct {
		id     int64
		sha256 string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.sha256); err != nil {
			return 0, err
		}
		todo = append(todo, p)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	enqueued := 0
	for _, p := range todo {
		_, err := q.Enqueue(ctx, JobType, Payload{AssetID: p.id}, jobs.EnqueueOptions{
			// Below an interactive scan, so a user-triggered rescan is not stuck
			// behind ten thousand thumbnails.
			Priority:  0,
			DedupeKey: DedupeKey(p.sha256),
		})
		switch {
		case errors.Is(err, jobs.ErrDuplicate):
			// Already scheduled — including by another asset with identical content,
			// which is the point of keying on the hash.
		case err != nil:
			return enqueued, err
		default:
			enqueued++
		}
	}
	return enqueued, nil
}

// Stats counts assets by derive state, for the health report and the UI.
type Stats struct {
	Pending     int
	OK          int
	Failed      int
	Unsupported int
}

func LoadStats(ctx context.Context, database *db.DB) (Stats, error) {
	rows, err := database.Reader.QueryContext(ctx, `
		SELECT derive_state, count(*) FROM assets
		WHERE missing_since IS NULL
		GROUP BY derive_state`)
	if err != nil {
		return Stats{}, fmt.Errorf("derive stats: %w", err)
	}
	defer rows.Close()

	var s Stats
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return Stats{}, err
		}
		switch state {
		case StatePending:
			s.Pending = n
		case StateOK:
			s.OK = n
		case StateFailed:
			s.Failed = n
		case StateUnsupported:
			s.Unsupported = n
		}
	}
	return s, rows.Err()
}

// ResetFailed puts failed assets back to pending so they are picked up again.
//
// The asset-side half of §6's "retry failed derivatives" action; the queue-side half
// is jobs.RetryFailed.
func ResetFailed(ctx context.Context, database *db.DB) (int64, error) {
	res, err := database.Writer.ExecContext(ctx, `
		UPDATE assets SET derive_state = ?, derive_error = '', derive_version = 0, updated_at = ?
		WHERE derive_state = ?`, StatePending, time.Now().Unix(), StateFailed)
	if err != nil {
		return 0, fmt.Errorf("reset failed derivatives: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// --- small helpers ----------------------------------------------------------

const maxDeriveErrorLen = 1000

func truncateError(s string) string {
	if len(s) <= maxDeriveErrorLen {
		return s
	}
	return s[:maxDeriveErrorLen] + " …(truncated)"
}

func nullableInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

package index

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/palette"
)

// ErrNotFound means no asset has that id.
var ErrNotFound = errors.New("asset not found")

// DefaultPageSize is the grid's page size. §8 requires the grid stay responsive at
// 20k+ rows, which pagination rather than page size is what delivers.
const DefaultPageSize = 100

// MaxPageSize caps a caller-supplied limit.
const MaxPageSize = 500

// Asset is one row joined with its pack, as the UI needs it.
type Asset struct {
	ID       int64
	PackID   int64
	PackName string
	PackSlug string
	// PackRelPath and RelPath together give the library-relative path.
	PackRelPath string
	RelPath     string
	Filename    string
	Ext         string
	Kind        string
	Size        int64
	ModTime     time.Time
	SHA256      string
	Width       int
	Height      int

	FirstSeenAt      time.Time
	LastVerifiedAt   time.Time
	MissingSince     *time.Time
	ContentChangedAt *time.Time

	// --- derivative state and image analysis (M2) ---

	HasAlpha           bool
	HasSemitransparent bool
	ColorCount         int
	IsPixelArt         bool
	PHash              string
	FrameCount         int
	FPS                float64
	AnimationNames     []string

	DeriveState   string
	DeriveError   string
	DeriveVersion int

	// --- audio analysis (M5) ---
	DurationMS int
	SampleRate int
	Channels   int
	BitDepth   int
	PeakDBFS   float64
	IsLoopable bool

	// --- 3D model (M6) ---
	TriCount      int
	VertCount     int
	BBoxX         float64
	BBoxY         float64
	BBoxZ         float64
	MaterialCount int

	// --- spritesheet (M7) ---
	FrameW      int
	FrameH      int
	FrameCols   int
	FrameRows   int
	FrameSource string // sidecar | detected | manual

	// --- palette (M11.5) ---
	// PaletteJSON is the raw swatch list as stored (§8); PaletteKind is exact or
	// quantized. Both empty until the image is analysed. Parse with Palette().
	PaletteJSON string
	PaletteKind string
}

// IsSheet reports whether a frame grid has been proposed or confirmed (§6, §8).
func (a Asset) IsSheet() bool { return a.FrameCols > 0 && a.FrameRows > 0 }

// SheetConfirmed reports whether the grid is trusted (from a sidecar or a human),
// as opposed to a detected guess awaiting confirmation (§6).
func (a Asset) SheetConfirmed() bool {
	return a.FrameSource == "manual" || a.FrameSource == "sidecar"
}

// IsModel reports whether this asset is a 3D model (§8 3D viewer).
func (a Asset) IsModel() bool { return a.Kind == "model" }

// HasModelPreview reports whether a normalised preview.glb was produced.
func (a Asset) HasModelPreview() bool { return a.IsModel() && a.DeriveState == "ok" }

// BBoxMetres renders the bounding box as "x × y × z m", or "" when unknown.
func (a Asset) BBoxMetres() string {
	if a.BBoxX == 0 && a.BBoxY == 0 && a.BBoxZ == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f × %.2f × %.2f m", a.BBoxX, a.BBoxY, a.BBoxZ)
}

// IsAudio reports whether this asset is a sound file (§6, §8 audio viewer).
func (a Asset) IsAudio() bool { return a.Kind == "audio" }

// Duration renders the audio length as m:ss, or "" when unknown.
func (a Asset) Duration() string {
	if a.DurationMS <= 0 {
		return ""
	}
	total := a.DurationMS / 1000
	return strconv.Itoa(total/60) + ":" + fmt.Sprintf("%02d", total%60)
}

// PeakLevel renders the peak in dBFS for display, or "" when unknown.
func (a Asset) PeakLevel() string {
	if a.SampleRate == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f dBFS", a.PeakDBFS)
}

// HasPreview reports whether derivatives exist to show.
func (a Asset) HasPreview() bool { return a.DeriveState == "ok" }

// HasPalette reports whether a non-empty colour palette was extracted (§8).
func (a Asset) HasPalette() bool {
	return a.PaletteKind != "" && a.PaletteJSON != "" && a.PaletteJSON != "[]"
}

// PaletteApprox reports whether the palette is a median-cut summary rather than an
// exact enumeration, which the UI must surface as approximate (§8).
func (a Asset) PaletteApprox() bool { return a.PaletteKind == palette.KindQuantized }

// AssetSwatch is one palette colour prepared for the detail page.
type AssetSwatch struct {
	palette.Swatch
}

// Percent renders the swatch's share of visible pixels for display (§8).
func (s AssetSwatch) Percent() string { return fmt.Sprintf("%.1f", s.Ratio*100) }

// PaletteSwatches parses the stored palette for rendering, most-used first. A parse
// failure yields no swatches rather than an error: a broken palette must not blank
// the whole detail page.
func (a Asset) PaletteSwatches() []AssetSwatch {
	if a.PaletteJSON == "" {
		return nil
	}
	var raw []palette.Swatch
	if err := json.Unmarshal([]byte(a.PaletteJSON), &raw); err != nil {
		return nil
	}
	out := make([]AssetSwatch, len(raw))
	for i, s := range raw {
		out[i] = AssetSwatch{Swatch: s}
	}
	return out
}

// Animated reports whether an animated preview was generated.
func (a Asset) Animated() bool { return a.FrameCount > 1 }

// LibraryPath is the asset's path relative to AMBAR_LIBRARY_ROOT. This is the
// value the download handler hands to safepath — never anything from a request.
func (a Asset) LibraryPath() string {
	return libraryPath(a.PackRelPath, a.RelPath)
}

// Missing reports whether the file was absent at the last scan.
func (a Asset) Missing() bool { return a.MissingSince != nil }

// Dimensions renders "128x64" or "" when unknown.
func (a Asset) Dimensions() string {
	if a.Width == 0 || a.Height == 0 {
		return ""
	}
	return strconv.Itoa(a.Width) + "×" + strconv.Itoa(a.Height)
}

// ListOptions selects and pages through assets.
type ListOptions struct {
	// Query is free text matched against filename and pack name via FTS5. Empty
	// browses everything.
	Query string
	// Kind filters by asset kind. Empty means all kinds.
	Kind string
	// PackID filters to one pack. Zero means all packs.
	PackID int64
	// IncludeMissing shows assets whose file was absent at the last scan. Off by
	// default: they are not there, so they would be noise in a browse. §12 keeps
	// the rows forever regardless.
	IncludeMissing bool

	Limit  int
	Cursor string
}

// Page is one page of results.
type Page struct {
	Assets []Asset
	// NextCursor is empty on the last page.
	NextCursor string
	// Total is the number of assets matching the filters, ignoring pagination.
	Total int
}

const assetColumns = `
	a.id, a.pack_id, p.name, p.slug, p.library_rel_path, a.rel_path,
	a.filename, a.ext, a.kind, a.size, a.mtime, a.sha256, a.width, a.height,
	a.first_seen_at, a.last_verified_at, a.missing_since, a.content_changed_at,
	a.has_alpha, a.has_semitransparent, a.color_count, a.is_pixel_art, a.phash,
	a.frame_count, a.fps, a.animation_names,
	a.derive_state, a.derive_error, a.derive_version,
	a.duration_ms, a.sample_rate, a.channels, a.bit_depth, a.peak_dbfs, a.is_loopable,
	a.tri_count, a.vert_count, a.bbox_x, a.bbox_y, a.bbox_z, a.material_count,
	a.frame_w, a.frame_h, a.frame_cols, a.frame_rows, a.frame_source,
	a.palette_json, a.palette_kind`

// List returns one page of assets.
//
// Pagination is keyset, not OFFSET: ordering by (filename, id) and resuming after
// the last row read stays constant-time at any depth, whereas OFFSET makes SQLite
// walk and discard every skipped row. §8 requires the grid stay responsive at 20k+
// assets, and this is the part that delivers it.
//
// Ordering is by filename in M1 even when searching. Relevance ranking belongs
// with the §7 query language in M3; using one ordering everywhere keeps a single
// pagination path, and for filename search alphabetical is genuinely usable.
func (ix *Indexer) List(ctx context.Context, opts ListOptions) (*Page, error) {
	limit := opts.Limit
	if limit <= 0 {
		limit = DefaultPageSize
	}
	if limit > MaxPageSize {
		limit = MaxPageSize
	}

	where, args, err := ix.assetWhere(ctx, opts)
	if err != nil {
		return nil, err
	}

	var total int
	if err := ix.db.Reader.QueryRowContext(ctx,
		`SELECT count(*) FROM assets a JOIN packs p ON p.id = a.pack_id WHERE `+
			strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		return nil, fmt.Errorf("count assets: %w", err)
	}

	// The cursor is applied on top of the filters, so it must not affect the total.
	pageWhere, pageArgs := where, args
	if opts.Cursor != "" {
		filename, id, err := decodeCursor(opts.Cursor)
		if err != nil {
			return nil, err
		}
		pageWhere = append(append([]string{}, where...), `(a.filename, a.id) > (?, ?)`)
		pageArgs = append(append([]any{}, args...), filename, id)
	}

	// One extra row tells us whether another page exists, without a second query.
	query := `SELECT ` + assetColumns + `
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		WHERE ` + strings.Join(pageWhere, " AND ") + `
		ORDER BY a.filename, a.id
		LIMIT ?`
	rows, err := ix.db.Reader.QueryContext(ctx, query, append(pageArgs, limit+1)...)
	if err != nil {
		return nil, fmt.Errorf("list assets: %w", err)
	}
	defer rows.Close()

	page := &Page{Total: total}
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, err
		}
		page.Assets = append(page.Assets, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(page.Assets) > limit {
		last := page.Assets[limit-1]
		page.Assets = page.Assets[:limit]
		page.NextCursor = encodeCursor(last.Filename, last.ID)
	}
	return page, nil
}

// listFilters builds the shared WHERE clause.
func listFilters(opts ListOptions) ([]string, []any, error) {
	where := []string{"1 = 1"}
	var args []any

	if !opts.IncludeMissing {
		where = append(where, "a.missing_since IS NULL")
	}
	if opts.Kind != "" {
		where = append(where, "a.kind = ?")
		args = append(args, opts.Kind)
	}
	if opts.PackID != 0 {
		where = append(where, "a.pack_id = ?")
		args = append(args, opts.PackID)
	}
	// The free-text and structured parts of opts.Query are compiled separately by
	// the caller through the §7 parser; listFilters covers only the sidebar facets.
	return where, args, nil
}

// Get returns one asset by id.
func (ix *Indexer) Get(ctx context.Context, id int64) (Asset, error) {
	row := ix.db.Reader.QueryRowContext(ctx, `SELECT `+assetColumns+`
		FROM assets a JOIN packs p ON p.id = a.pack_id
		WHERE a.id = ?`, id)

	a, err := scanAsset(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	return a, err
}

// ContentHashes returns the set of distinct content hashes still referenced by an
// asset, as lowercase hex. M12's junk view uses it to tell an orphaned derivative
// (a hash directory matching nothing) from a live one.
//
// Missing assets are included deliberately: §12 keeps their rows, and a derivative
// for a temporarily-absent file is not orphaned — the file may return on the next
// scan, and its thumbnail should still be there when it does.
func (ix *Indexer) ContentHashes(ctx context.Context) (map[string]struct{}, error) {
	rows, err := ix.db.Reader.QueryContext(ctx, `SELECT DISTINCT lower(sha256) FROM assets`)
	if err != nil {
		return nil, fmt.Errorf("load content hashes: %w", err)
	}
	defer rows.Close()

	hashes := map[string]struct{}{}
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		hashes[h] = struct{}{}
	}
	return hashes, rows.Err()
}

// Stats summarises the index for the home page and the scan summary.
type Stats struct {
	Assets         int
	Packs          int
	Missing        int
	ContentChanged int
	TotalBytes     int64
	ByKind         []KindCount
}

// KindCount is one row of the kind breakdown, largest first.
type KindCount struct {
	Kind  string
	Count int
}

func (ix *Indexer) Stats(ctx context.Context) (Stats, error) {
	var s Stats

	if err := ix.db.Reader.QueryRowContext(ctx, `
		SELECT
		    count(*),
		    coalesce(sum(size), 0),
		    coalesce(sum(missing_since IS NOT NULL), 0),
		    coalesce(sum(content_changed_at IS NOT NULL), 0)
		FROM assets`).Scan(&s.Assets, &s.TotalBytes, &s.Missing, &s.ContentChanged); err != nil {
		return s, fmt.Errorf("asset stats: %w", err)
	}
	if err := ix.db.Reader.QueryRowContext(ctx, `SELECT count(*) FROM packs`).Scan(&s.Packs); err != nil {
		return s, fmt.Errorf("pack stats: %w", err)
	}

	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT kind, count(*) FROM assets
		WHERE missing_since IS NULL
		GROUP BY kind ORDER BY count(*) DESC, kind`)
	if err != nil {
		return s, fmt.Errorf("kind stats: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var kc KindCount
		if err := rows.Scan(&kc.Kind, &kc.Count); err != nil {
			return s, err
		}
		s.ByKind = append(s.ByKind, kc)
	}
	return s, rows.Err()
}

// encodeCursor packs the keyset position into an opaque token.
func encodeCursor(filename string, id int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte(filename + "\x00" + strconv.FormatInt(id, 10)))
}

func decodeCursor(cursor string) (string, int64, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return "", 0, fmt.Errorf("malformed page cursor")
	}
	filename, idPart, ok := strings.Cut(string(raw), "\x00")
	if !ok {
		return "", 0, fmt.Errorf("malformed page cursor")
	}
	id, err := strconv.ParseInt(idPart, 10, 64)
	if err != nil {
		return "", 0, fmt.Errorf("malformed page cursor")
	}
	return filename, id, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanAsset(row scanner) (Asset, error) {
	var (
		a                Asset
		width, height    sql.NullInt64
		mtime            int64
		firstSeen        int64
		lastVerified     int64
		missingSince     sql.NullInt64
		contentChangedAt sql.NullInt64
		derived          deriveColumns
		durationMS       sql.NullInt64
		sampleRate       sql.NullInt64
		channels         sql.NullInt64
		bitDepth         sql.NullInt64
		peakDBFS         sql.NullFloat64
		isLoopable       sql.NullInt64
		triCount         sql.NullInt64
		vertCount        sql.NullInt64
		bboxX            sql.NullFloat64
		bboxY            sql.NullFloat64
		bboxZ            sql.NullFloat64
		materialCount    sql.NullInt64
		frameW           sql.NullInt64
		frameH           sql.NullInt64
		frameCols        sql.NullInt64
		frameRows        sql.NullInt64
		frameSource      sql.NullString
		paletteJSON      sql.NullString
		paletteKind      sql.NullString
	)
	if err := row.Scan(
		&a.ID, &a.PackID, &a.PackName, &a.PackSlug, &a.PackRelPath, &a.RelPath,
		&a.Filename, &a.Ext, &a.Kind, &a.Size, &mtime, &a.SHA256, &width, &height,
		&firstSeen, &lastVerified, &missingSince, &contentChangedAt,
		&derived.hasAlpha, &derived.hasSemitransparent, &derived.colorCount,
		&derived.isPixelArt, &derived.phash,
		&derived.frameCount, &derived.fps, &derived.animationNames,
		&a.DeriveState, &a.DeriveError, &a.DeriveVersion,
		&durationMS, &sampleRate, &channels, &bitDepth, &peakDBFS, &isLoopable,
		&triCount, &vertCount, &bboxX, &bboxY, &bboxZ, &materialCount,
		&frameW, &frameH, &frameCols, &frameRows, &frameSource,
		&paletteJSON, &paletteKind,
	); err != nil {
		return Asset{}, err
	}
	derived.apply(&a)
	a.DurationMS = int(durationMS.Int64)
	a.SampleRate = int(sampleRate.Int64)
	a.Channels = int(channels.Int64)
	a.BitDepth = int(bitDepth.Int64)
	a.PeakDBFS = peakDBFS.Float64
	a.IsLoopable = isLoopable.Valid && isLoopable.Int64 != 0
	a.TriCount = int(triCount.Int64)
	a.VertCount = int(vertCount.Int64)
	a.BBoxX, a.BBoxY, a.BBoxZ = bboxX.Float64, bboxY.Float64, bboxZ.Float64
	a.MaterialCount = int(materialCount.Int64)
	a.FrameW = int(frameW.Int64)
	a.FrameH = int(frameH.Int64)
	a.FrameCols = int(frameCols.Int64)
	a.FrameRows = int(frameRows.Int64)
	a.FrameSource = frameSource.String
	a.PaletteJSON = paletteJSON.String
	a.PaletteKind = paletteKind.String

	a.ModTime = time.Unix(mtime, 0)
	a.FirstSeenAt = time.Unix(firstSeen, 0)
	a.LastVerifiedAt = time.Unix(lastVerified, 0)
	if width.Valid {
		a.Width = int(width.Int64)
	}
	if height.Valid {
		a.Height = int(height.Int64)
	}
	if missingSince.Valid {
		t := time.Unix(missingSince.Int64, 0)
		a.MissingSince = &t
	}
	if contentChangedAt.Valid {
		t := time.Unix(contentChangedAt.Int64, 0)
		a.ContentChangedAt = &t
	}
	return a, nil
}

// deriveColumns collects the nullable M2 columns so both scanAsset and scanGroupRow
// read them the same way.
type deriveColumns struct {
	hasAlpha           sql.NullInt64
	hasSemitransparent sql.NullInt64
	colorCount         sql.NullInt64
	isPixelArt         sql.NullInt64
	phash              sql.NullString
	frameCount         sql.NullInt64
	fps                sql.NullFloat64
	animationNames     sql.NullString
}

func (d deriveColumns) apply(a *Asset) {
	a.HasAlpha = d.hasAlpha.Valid && d.hasAlpha.Int64 != 0
	a.HasSemitransparent = d.hasSemitransparent.Valid && d.hasSemitransparent.Int64 != 0
	a.IsPixelArt = d.isPixelArt.Valid && d.isPixelArt.Int64 != 0
	if d.colorCount.Valid {
		a.ColorCount = int(d.colorCount.Int64)
	}
	if d.phash.Valid {
		a.PHash = d.phash.String
	}
	if d.frameCount.Valid {
		a.FrameCount = int(d.frameCount.Int64)
	}
	if d.fps.Valid {
		a.FPS = d.fps.Float64
	}
	if d.animationNames.Valid && d.animationNames.String != "" {
		// Stored as a JSON array by the derive handler. A decode failure means the
		// column was hand-edited; the rest of the asset is still perfectly usable.
		var names []string
		if err := json.Unmarshal([]byte(d.animationNames.String), &names); err == nil {
			a.AnimationNames = names
		}
	}
}

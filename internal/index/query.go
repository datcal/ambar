package index

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
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

	// --- filled in by the handler, not by a query (M17) ---
	// ThumbOnDisk is whether an image thumbnail exists in the derivative directory.
	//
	// Not a column, because it is a fact about the filesystem and invariant 2 says the
	// filesystem is the source of truth; a column would be a second copy to drift. It
	// matters only for models, where derive can succeed without producing a picture, so
	// only the handlers that render model tiles pay for the stat — see
	// (*Server).markModelThumbs.
	ThumbOnDisk bool
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

// HasModelPreview reports whether *a* preview exists for this model — a normalised
// glb, or a thumbnail rendered by the browser (M15). It does not say which, so it
// must not be used to build a URL; see ViewerFile.
func (a Asset) HasModelPreview() bool { return a.IsModel() && a.DeriveState == "ok" }

// ViewerFormat says which three.js loader can show this model in the browser, or ""
// when none can (M14).
//
// The point of this method is that it does not depend on derive at all. §8's viewer
// used to require a normalised preview.glb, so every .obj and .fbx in the library
// showed "this format needs Blender to preview" and nothing else — while three.js
// has read both formats natively for years. Only `.blend` genuinely needs Blender,
// because it is an application file rather than an interchange format.
func (a Asset) ViewerFormat() string {
	if !a.IsModel() {
		return ""
	}
	switch strings.ToLower(a.Ext) {
	case "glb", "gltf":
		return "gltf"
	case "obj":
		return "obj"
	case "fbx":
		return "fbx"
	default:
		// .blend, and anything else a future scan classifies as a model.
		return ""
	}
}

// NeedsBrowserThumb reports whether the grid should ask the browser to render a
// thumbnail for this model (M15): a format three.js can read, with no image yet.
//
// Blender would produce a better one — a lit turntable rather than a flat snapshot —
// but Blender is optional (§6) and a grid full of extension chips is worse than a
// snapshot.
//
// M17: the test is ThumbOnDisk, not HasPreview. `derive_state = 'ok'` means "derive
// finished", and for a glTF or an OBJ derive finishing means it wrote a preview.glb —
// geometry, not a picture. So those rows claimed a thumbnail they never had (the tile
// rendered a 404) *and* were excluded from the browser renderer that would have made
// one. Measured: 212 of 221 glTF tiles and 42 of 42 OBJ tiles were permanently blank,
// while every browser-rendered .fbx was fine, because that path writes the image before
// it sets 'ok'.
func (a Asset) NeedsBrowserThumb() bool {
	return a.ViewerFormat() != "" && !a.ThumbOnDisk && !a.Missing()
}

// ShowsThumb reports whether a tile can render an <img> for this asset.
//
// For everything but a model, "derive succeeded" and "there is a picture" are the same
// statement. For a model they are not, so the answer comes from the filesystem — see
// ThumbOnDisk, which the handler fills in.
func (a Asset) ShowsThumb() bool {
	if a.IsModel() {
		return a.ThumbOnDisk
	}
	return a.HasPreview()
}

// ViewerFile is the companion-route URL for the original model, which is what the
// browser loads for a format three.js reads directly.
//
// There is deliberately no method here that picks between this and a derived
// preview.glb. `derive_state = 'ok'` means "some preview exists" — after M15 it can
// mean a browser-rendered *thumbnail* — and a glb is only produced for the formats
// deriveModel normalises. Guessing from the row gave every .obj and .fbx a
// data-src of /preview.glb, which 404s, which is why an FBX opened to an empty
// stage. The choice needs the filesystem, so the handler makes it (server.pageData
// .ViewerSrc).
func (a Asset) ViewerFile() string {
	return "/assets/" + strconv.FormatInt(a.ID, 10) + "/file/" + url.PathEscape(a.Filename)
}

// MTLName is the material library an .obj conventionally sits beside: the same
// basename with a .mtl extension. A guess, and a cheap one — the viewer carries on
// without materials when it is wrong.
func (a Asset) MTLName() string {
	if strings.ToLower(a.Ext) != "obj" {
		return ""
	}
	return strings.TrimSuffix(a.Filename, filepath.Ext(a.Filename)) + ".mtl"
}

// BBoxMetres renders the bounding box as "x × y × z m", or "" when unknown.
func (a Asset) BBoxMetres() string {
	if a.BBoxX == 0 && a.BBoxY == 0 && a.BBoxZ == 0 {
		return ""
	}
	return fmt.Sprintf("%.2f × %.2f × %.2f m", a.BBoxX, a.BBoxY, a.BBoxZ)
}

// IsFont reports whether this asset is a typeface, which gets a live specimen rather
// than a static preview (M15).
func (a Asset) IsFont() bool { return a.Kind == "font" }

// FontFamily is the family name recorded by the specimen renderer, taken from the
// derive notes ("family: Inter"). Empty when unknown.
func (a Asset) FontFamily() string {
	const prefix = "family: "
	for _, note := range strings.Split(a.DeriveError, "; ") {
		if strings.HasPrefix(note, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(note, prefix))
		}
	}
	return ""
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

// Has2DPreview reports whether the §8 pan-and-zoom image viewer applies. Models and
// audio are excluded: a model has its own 3D stage, and after M15 its "preview" may be
// a browser-rendered thumbnail, which in the 2D viewer looked like the asset itself.
func (a Asset) Has2DPreview() bool { return a.HasPreview() && !a.IsModel() && !a.IsAudio() }

// ThumbUpscales reports whether the grid will draw this asset's thumbnail larger than
// the pixels it actually has.
//
// The tile is between 4rem and 26rem wide (§8's size slider), so anything whose long
// edge is under about 256px is always being magnified there. Magnifying without
// `image-rendering: pixelated` is what made a 32x32 sprite a smudge in the grid, and it
// is not a question the pixel-art detector should be answering: at these sizes there is
// no version of "correct" that involves interpolation, whatever the artwork is.
//
// Deliberately not applied to large sources. A 2048px texture shown at 176px is being
// *reduced*, and nearest-neighbour reduction throws away every pixel it does not land
// on — smooth is right there.
func (a Asset) ThumbUpscales() bool {
	const alwaysMagnified = 256
	long := a.Width
	if a.Height > long {
		long = a.Height
	}
	return long > 0 && long <= alwaysMagnified
}

// HasPalette reports whether a non-empty colour palette was extracted (§8).
func (a Asset) HasPalette() bool {
	return a.PaletteKind != "" && a.PaletteJSON != "" && a.PaletteJSON != "[]"
}

// PaletteApprox reports whether the palette is a median-cut summary rather than an
// exact enumeration, which the UI must surface as approximate (§8).
func (a Asset) PaletteApprox() bool { return a.PaletteKind == palette.KindQuantized }

// PaletteFirstHex is the dominant swatch's hex, for the example in the panel's help
// line. Empty when there is no palette.
func (a Asset) PaletteFirstHex() string {
	swatches := a.PaletteSwatches()
	if len(swatches) == 0 {
		return ""
	}
	return swatches[0].Hex
}

// AssetSwatch is one palette colour prepared for the detail page.
type AssetSwatch struct {
	palette.Swatch
}

// Percent renders the swatch's share of visible pixels for display (§8).
func (s AssetSwatch) Percent() string { return fmt.Sprintf("%.1f", s.Ratio*100) }

// HexQuery is the hex without the leading '#', for building a `color:` query in a
// URL. The hash would have to be percent-encoded, and a bare hex is accepted by the
// §7 parser precisely so this reads cleanly.
func (s AssetSwatch) HexQuery() string { return strings.TrimPrefix(s.Hex, "#") }

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

// Animated reports whether the *source file* is itself an animation — a GIF, an
// animated WebP, an .aseprite with more than one frame — as against a still image
// whose pixels happen to divide into a grid.
//
// M17: this used to be `FrameCount > 1`, which conflated the two and was wrong for
// most of the library. `frame_count` is also set by §6's spritesheet grid *detection*,
// which is a guess about geometry, not a claim that anything moves — and in this
// library it guessed a 48x40 tileset was an animation of 1,920 frames. Measured:
// 6,706 assets said "animated" and 795 anim.gif files existed, so nearly six thousand
// tiles offered a hover animation that 404'd and blanked the image.
//
// `frame_source` is what separates them. Empty means the decoder found real frames in
// the file, which is exactly when derive writes anim.gif — 795 rows, 795 files. Any
// other value is a grid, detected or confirmed, and belongs to AnimatedPreview below.
func (a Asset) Animated() bool { return a.FrameCount > 1 && a.FrameSource == "" }

// AnimatedPreview is the URL of a moving preview that should exist for this asset, or
// "" when there is none to offer.
//
// Two cases, and no third: a real animation has anim.gif, and a frame grid a human
// stood behind has sheet.gif. A *detected* grid deliberately gets nothing — §6 says a
// guess is never trusted silently, and animating one in the grid is trusting it
// silently. The confirmation UI on the detail page is where a guess belongs.
//
// The caller must still tolerate a 404: confirming a grid does not currently rebuild
// sheet.gif, so a corrected geometry can leave a stale file or none at all. grid.js
// falls back to the still frame on error.
func (a Asset) AnimatedPreview() string {
	switch {
	case a.Animated():
		return fmt.Sprintf("/assets/%d/anim.gif", a.ID)
	case a.FrameCount > 1 && a.SheetConfirmed():
		return fmt.Sprintf("/assets/%d/sheet.gif", a.ID)
	default:
		return ""
	}
}

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
	// Dir restricts the results to a library directory and everything beneath it —
	// the folder tree's filter (M14). Slash-separated and library-relative; empty
	// browses the whole library.
	Dir string
	// IncludeMissing shows assets whose file was absent at the last scan. Off by
	// default: they are not there, so they would be noise in a browse. §12 keeps
	// the rows forever regardless.
	IncludeMissing bool
	// IncludeDisabled shows assets tagged `disable:true`. Off by default — that is the
	// whole point of the tag; see DisabledTag.
	IncludeDisabled bool

	Limit int
	// Cursor is the keyset position for the API's `next_cursor` paging (§10). The web
	// grid uses Page instead — see the Sort and Page notes below.
	Cursor string

	// Sort names the browse order. Empty means SortDefault.
	//
	// This exists because there was exactly one order — filename A→Z across the whole
	// library — so "what did I download yesterday" was unanswerable in a tool whose
	// whole job is finding assets, and a sprite from 2d/ interleaved alphabetically with
	// a wav from sounds/.
	Sort SortOrder

	// Page is 1-based. Zero and 1 both mean the first page.
	//
	// Numbered paging replaced the cursor for the UI in M16, and the two cannot be the
	// same mechanism: a keyset cursor can only step forward from where you are, so there
	// was no way back, no way to jump, and no shareable URL for "page 4". Offset paging
	// costs the database more, but `Total` is already computed for the result count, so
	// the page count is free, and the offsets stay small because the page size does.
	Page int
}

// SortOrder is a browse order for the grid.
type SortOrder string

// The orders the grid offers. Kept as strings because they travel in URLs.
const (
	SortNewest   SortOrder = "added"    // first indexed, newest first
	SortModified SortOrder = "modified" // file mtime, newest first
	SortName     SortOrder = "name"
	SortNameDesc SortOrder = "name-desc"
	SortLargest  SortOrder = "size"
	SortKind     SortOrder = "kind"
	SortPixels   SortOrder = "pixels" // biggest image area first
	// Triangle count, both ways (M17). Both directions matter and they are different
	// questions: "cheapest model that will do" is the budget one people actually ask,
	// and "heaviest thing in the library" is how you find the one that will not ship.
	SortTrisAsc  SortOrder = "tris"
	SortTrisDesc SortOrder = "tris-desc"

	// SortDefault is what an unqualified browse gets: most recently indexed first.
	//
	// Deliberately not filename order, which is what it used to be. A library grows by
	// arrival — a pack lands, you look at what landed — and alphabetical order buries
	// that under whatever happens to start with "a".
	SortDefault = SortNewest
)

// orderBy returns the SQL for a sort, always ending in a unique tiebreaker so paging
// cannot skip or repeat a row when two assets share a value.
func (s SortOrder) orderBy() string {
	switch s {
	case SortModified:
		return "a.mtime DESC, g.id DESC"
	case SortName:
		return "a.filename ASC, g.id ASC"
	case SortNameDesc:
		return "a.filename DESC, g.id DESC"
	case SortLargest:
		return "a.size DESC, g.id DESC"
	case SortKind:
		return "a.kind ASC, a.filename ASC, g.id ASC"
	case SortPixels:
		// NULL for anything without dimensions (audio, fonts), which sorts last here
		// rather than first — an unknown size is not a big one.
		return "coalesce(a.width, 0) * coalesce(a.height, 0) DESC, g.id DESC"
	case SortTrisAsc:
		// Ascending is the awkward direction: everything that is not a model has no
		// triangle count, and NULL sorting first would fill the first page with sprites.
		// So a missing count is pushed to the end explicitly.
		return "a.tri_count IS NULL, a.tri_count ASC, g.id ASC"
	case SortTrisDesc:
		return "coalesce(a.tri_count, 0) DESC, g.id DESC"
	default:
		return "a.first_seen_at DESC, g.id DESC"
	}
}

// Label is how the sort appears in the UI.
func (s SortOrder) Label() string {
	switch s {
	case SortModified:
		return "File date"
	case SortName:
		return "Name A→Z"
	case SortNameDesc:
		return "Name Z→A"
	case SortLargest:
		return "Largest first"
	case SortKind:
		return "Kind, then name"
	case SortPixels:
		return "Pixel size"
	case SortTrisAsc:
		return "Triangles, fewest first"
	case SortTrisDesc:
		return "Triangles, most first"
	default:
		return "Recently added"
	}
}

// SortOrders lists the orders in the sequence the dropdown shows them.
func SortOrders() []SortOrder {
	return []SortOrder{
		SortNewest, SortModified, SortName, SortNameDesc, SortLargest, SortKind,
		SortPixels, SortTrisAsc, SortTrisDesc,
	}
}

// ParseSort maps a URL value to an order, falling back to the default rather than
// erroring: a hand-edited or stale `sort=` is not worth a 400.
func ParseSort(raw string) SortOrder {
	candidate := SortOrder(strings.TrimSpace(strings.ToLower(raw)))
	for _, s := range SortOrders() {
		if s == candidate {
			return s
		}
	}
	return SortDefault
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

// assetListColumns is assetColumns for a *list* of assets — the grid, the variant list —
// where the palette is never rendered.
//
// Same shape, same order, same scanner: the two palette columns are replaced by empty string
// literals, so nothing about the scanning code has to know which query it is reading. That is
// the whole trick, and it is why this is two lines rather than a second Asset type.
//
// Measured on a 6,495-asset library (122,711 swatches), one page of 100 rows:
//
//	47 columns including palette_json   77.7 ms
//	the same 47 with the palettes empty 34.8 ms
//	a hand-picked 18 columns            26.0 ms
//
// So palette_json alone was 55% of the cost of a page of the grid — up to a few KB of JSON per
// row, allocated and then thrown away, a hundred times per page view. Trimming the other 27
// unused columns buys 9 ms more and would need a separate projection type; that trade is not
// worth it, this one obviously is.
const assetListColumns = `
	a.id, a.pack_id, p.name, p.slug, p.library_rel_path, a.rel_path,
	a.filename, a.ext, a.kind, a.size, a.mtime, a.sha256, a.width, a.height,
	a.first_seen_at, a.last_verified_at, a.missing_since, a.content_changed_at,
	a.has_alpha, a.has_semitransparent, a.color_count, a.is_pixel_art, a.phash,
	a.frame_count, a.fps, a.animation_names,
	a.derive_state, a.derive_error, a.derive_version,
	a.duration_ms, a.sample_rate, a.channels, a.bit_depth, a.peak_dbfs, a.is_loopable,
	a.tri_count, a.vert_count, a.bbox_x, a.bbox_y, a.bbox_z, a.material_count,
	a.frame_w, a.frame_h, a.frame_cols, a.frame_rows, a.frame_source,
	'' AS palette_json, '' AS palette_kind`

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
	query := `SELECT ` + assetListColumns + `
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

// DisabledNamespace / DisabledName are the tag that hides an asset: `disable:true`.
//
// A tag rather than a column, because the work is bulk. A 3D pack ships a dozen blank
// filler PNGs and a "hidden" column would need its own button, its own bulk form and its
// own page; the grid already has multi-select and a tag box, so tagging fifty tiles at
// once is a feature that already exists. It also travels: §3 writes tags to the sidecar,
// so re-indexing from scratch keeps the decision, which a column on a rebuildable index
// would not (invariant 2).
const (
	DisabledNamespace = "disable"
	DisabledName      = "true"
)

// notDisabledExpr excludes assets carrying the hide tag. `alias` is the assets alias in
// the query being built.
func notDisabledExpr(alias string) string {
	return "NOT EXISTS (SELECT 1 FROM asset_tags dt JOIN tags dtg ON dtg.id = dt.tag_id" +
		" WHERE dt.asset_id = " + alias + ".id AND dtg.namespace = '" + DisabledNamespace +
		"' AND dtg.name = '" + DisabledName + "')"
}

// listFilters builds the shared WHERE clause.
func listFilters(opts ListOptions) ([]string, []any, error) {
	where := []string{"1 = 1"}
	var args []any

	if !opts.IncludeMissing {
		where = append(where, "a.missing_since IS NULL")
	}
	if !opts.IncludeDisabled {
		where = append(where, notDisabledExpr("a"))
	}
	if opts.Kind != "" {
		where = append(where, "a.kind = ?")
		args = append(args, opts.Kind)
	}
	if opts.PackID != 0 {
		where = append(where, "a.pack_id = ?")
		args = append(args, opts.PackID)
	}
	if dir := cleanDir(opts.Dir); dir != "" {
		where = append(where, dirExpr("a"))
		args = append(args, dir, dir+"/", dir+"/")
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
	// Disabled counts assets tagged `disable:true`, so the grid can say how many it is
	// not showing. A hidden thing you cannot count is a thing you have lost.
	Disabled   int
	TotalBytes int64
	ByKind     []KindCount
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
	if err := ix.db.Reader.QueryRowContext(ctx, `
		SELECT count(*) FROM assets a
		WHERE a.missing_since IS NULL AND NOT (`+notDisabledExpr("a")+`)`).Scan(&s.Disabled); err != nil {
		return s, fmt.Errorf("hidden stats: %w", err)
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

// cleanDir normalises a directory filter from a URL: slashes only, no leading or
// trailing separator, no traversal. It is not a filesystem path — nothing is opened
// from it — but a "../.." in a LIKE prefix would still be a confusing filter, and
// rejecting it here keeps the SQL honest.
func cleanDir(dir string) string {
	dir = strings.Trim(strings.TrimSpace(strings.ReplaceAll(dir, "\\", "/")), "/")
	if dir == "" || dir == "." {
		return ""
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return ""
		}
	}
	return dir
}

// dirExpr matches assets at or below a directory, comparing against the library path
// assembled from the pack and the asset. Three bound arguments, in order: the
// directory, its prefix (for the length), and the prefix again.
//
// A prefix comparison rather than LIKE: no escaping of % or _ in real filenames, and
// SQLite can still use the index on the underlying columns for the pack half.
func dirExpr(alias string) string {
	libPath := fmt.Sprintf(`(CASE WHEN p.library_rel_path IN ('', '.') THEN %[1]s.rel_path
	                              ELSE p.library_rel_path || '/' || %[1]s.rel_path END)`, alias)
	return fmt.Sprintf("(%[1]s = ? OR substr(%[1]s, 1, length(?)) = ?)", libPath)
}

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
	"unicode"
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
}

// HasPreview reports whether derivatives exist to show.
func (a Asset) HasPreview() bool { return a.DeriveState == "ok" }

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
	a.derive_state, a.derive_error, a.derive_version`

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

	where, args, err := listFilters(opts)
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
	if match := FTSQuery(opts.Query); match != "" {
		// A subquery rather than a join, so the FTS table cannot multiply rows and
		// the (filename, id) keyset ordering stays unique.
		where = append(where, "a.id IN (SELECT rowid FROM assets_fts WHERE assets_fts MATCH ?)")
		args = append(args, match)
	}
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

// FTSQuery turns a user's search box input into a safe FTS5 MATCH expression.
//
// Raw input is never passed through. The M0 spike confirmed FTS5 returns a syntax
// error for an unbalanced quote or a stray operator, so `sword"` typed into a
// search box would otherwise 500. Instead every token is quoted as a literal — which
// also neutralises `AND`, `OR`, `NOT`, `NEAR` and `*` as accidental operators — and
// given a trailing `*` so partial words match as the user types.
//
// The result is an implicit AND of prefix-matched terms, which is what a filename
// search should do. The full §7 query language, where operators are deliberate,
// arrives in M3 with its own parser.
func FTSQuery(input string) string {
	var (
		tokens []string
		b      strings.Builder
	)
	flush := func() {
		if b.Len() == 0 {
			return
		}
		// Double-quoted, so the token is a literal string to FTS5 rather than a
		// possible operator. The trailing * sits outside the quotes, which is the
		// prefix syntax.
		tokens = append(tokens, `"`+b.String()+`"*`)
		b.Reset()
	}

	// Split on the same characters the tokenizer splits on, rather than deleting
	// them. Deleting would join across separators: "wooden_sword" would become the
	// single token "woodensword", which matches nothing, because the index holds
	// "wooden" and "sword" separately.
	for _, r := range input {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r > unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			// Non-ASCII letters are legitimate: café, Hörn.
			b.WriteRune(r)
		default:
			flush()
		}
	}
	flush()

	// Bound the query size. A pasted paragraph should search, not build a
	// thousand-term expression.
	if len(tokens) > maxSearchTokens {
		tokens = tokens[:maxSearchTokens]
	}
	return strings.Join(tokens, " ")
}

// maxSearchTokens caps how many terms one search box entry becomes.
const maxSearchTokens = 16

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
	)
	if err := row.Scan(
		&a.ID, &a.PackID, &a.PackName, &a.PackSlug, &a.PackRelPath, &a.RelPath,
		&a.Filename, &a.Ext, &a.Kind, &a.Size, &mtime, &a.SHA256, &width, &height,
		&firstSeen, &lastVerified, &missingSince, &contentChangedAt,
		&derived.hasAlpha, &derived.hasSemitransparent, &derived.colorCount,
		&derived.isPixelArt, &derived.phash,
		&derived.frameCount, &derived.fps, &derived.animationNames,
		&a.DeriveState, &a.DeriveError, &a.DeriveVersion,
	); err != nil {
		return Asset{}, err
	}
	derived.apply(&a)

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

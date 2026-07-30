// Package tags owns the tag model of §7: namespaced, hierarchical tags with
// aliases, applied to assets and packs, and the maintenance of the FTS tag_text
// column that makes them free-text searchable.
//
// A tag is written `namespace:name`, where name is the full colon-separated
// hierarchy path within the namespace — `type:sfx:impact` is namespace `type`,
// name `sfx:impact`, a child of `type:sfx`. The transitive hierarchy lives in
// the tag_closure table so "searching a parent returns children" (§7) is a
// single indexed lookup rather than a recursive query per request.
//
// Everything here goes through the single writer connection (§4). Reads for the
// UI and the query planner use the read pool.
package tags

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// ErrInvalidTag means a tag string is malformed: no namespace, an empty
// segment, or whitespace inside a segment. Callers that build tags from user
// input surface it; the auto-tag normaliser (§7) cleans input before it reaches
// here.
var ErrInvalidTag = errors.New("invalid tag")

// Tag sources, kept distinct so §7's "auto-tag ... overridable by manual tags"
// is enforceable: a manual tag outranks a machine-derived one on the same asset.
const (
	SourceManual    = "manual"
	SourceAutoPath  = "auto_path"
	SourceAutoType  = "auto_type"
	SourceInherited = "inherited"
)

// Tag is one row of the tags table.
type Tag struct {
	ID        int64
	Namespace string
	// Name is the full hierarchy path within the namespace: "sfx:impact".
	Name        string
	Description string
	ParentID    *int64
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Canonical is the `namespace:name` string, e.g. "type:sfx:impact".
func (t Tag) Canonical() string { return t.Namespace + ":" + t.Name }

// Leaf is the last hierarchy segment, for compact display: "impact".
func (t Tag) Leaf() string {
	if i := strings.LastIndex(t.Name, ":"); i >= 0 {
		return t.Name[i+1:]
	}
	return t.Name
}

// Store is the tag data layer.
type Store struct {
	db  *db.DB
	now func() time.Time
}

// NewStore wraps a database.
func NewStore(database *db.DB) *Store {
	return &Store{db: database, now: time.Now}
}

// WithClock replaces the clock, for tests.
func (s *Store) WithClock(now func() time.Time) *Store {
	s.now = now
	return s
}

// Parse splits and validates a canonical tag string into its namespace and its
// hierarchy-path name. It lowercases, because a tag is an identifier and
// `Author:Kenney` and `author:kenney` must not become two tags.
func Parse(raw string) (namespace, name string, err error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", "", fmt.Errorf("%w: empty", ErrInvalidTag)
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("%w: %q has no namespace (want namespace:name)", ErrInvalidTag, raw)
	}
	for _, p := range parts {
		if err := validSegment(p); err != nil {
			return "", "", fmt.Errorf("%w: %q: %v", ErrInvalidTag, raw, err)
		}
	}
	return parts[0], strings.Join(parts[1:], ":"), nil
}

// validSegment rejects the shapes that would make a tag ambiguous or unsearchable.
func validSegment(p string) error {
	if p == "" {
		return errors.New("empty segment")
	}
	for _, r := range p {
		if r <= ' ' {
			return fmt.Errorf("segment %q contains whitespace or control characters", p)
		}
	}
	return nil
}

// parentName returns the hierarchy path one segment shorter, and whether one
// exists. "sfx:impact" -> "sfx", true; "cc0" -> "", false.
func parentName(name string) (string, bool) {
	if i := strings.LastIndex(name, ":"); i >= 0 {
		return name[:i], true
	}
	return "", false
}

// Ensure gets-or-creates the tag for a canonical string, creating every missing
// ancestor and its closure rows along the way. It is idempotent: calling it
// twice returns the same tag and creates nothing the second time.
func (s *Store) Ensure(ctx context.Context, canonical string) (Tag, error) {
	namespace, name, err := Parse(canonical)
	if err != nil {
		return Tag{}, err
	}
	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return Tag{}, fmt.Errorf("ensure tag: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	t, err := s.ensureTx(ctx, tx, namespace, name)
	if err != nil {
		return Tag{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tag{}, fmt.Errorf("ensure tag: commit: %w", err)
	}
	return t, nil
}

// ensureTx is Ensure's recursive core, all inside one transaction so a tag and
// its freshly-created ancestors either all land or none do.
func (s *Store) ensureTx(ctx context.Context, tx *sql.Tx, namespace, name string) (Tag, error) {
	if t, ok, err := getTagTx(ctx, tx, namespace, name); err != nil {
		return Tag{}, err
	} else if ok {
		return t, nil
	}

	var parentID *int64
	if pn, ok := parentName(name); ok {
		parent, err := s.ensureTx(ctx, tx, namespace, pn)
		if err != nil {
			return Tag{}, err
		}
		parentID = &parent.ID
	}

	now := s.now().Unix()
	res, err := tx.ExecContext(ctx, `
		INSERT INTO tags (namespace, name, parent_id, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, namespace, name, parentID, now, now)
	if err != nil {
		return Tag{}, fmt.Errorf("insert tag %s:%s: %w", namespace, name, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Tag{}, err
	}

	// The self-edge, always present so a tag is its own descendant at depth 0.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO tag_closure (ancestor_id, descendant_id, depth) VALUES (?, ?, 0)`,
		id, id); err != nil {
		return Tag{}, fmt.Errorf("closure self-edge for %d: %w", id, err)
	}
	// Copy every (ancestor -> parent) edge as (ancestor -> this), one deeper.
	if parentID != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO tag_closure (ancestor_id, descendant_id, depth)
			SELECT ancestor_id, ?, depth + 1 FROM tag_closure WHERE descendant_id = ?`,
			id, *parentID); err != nil {
			return Tag{}, fmt.Errorf("closure ancestors for %d: %w", id, err)
		}
	}

	return Tag{ID: id, Namespace: namespace, Name: name, ParentID: parentID,
		CreatedAt: time.Unix(now, 0), UpdatedAt: time.Unix(now, 0)}, nil
}

// Resolve maps a token to a tag without creating anything. The token is either a
// canonical `namespace:name` or a bare alias (`sfx`, `cc0`). It reports whether
// the tag was found.
func (s *Store) Resolve(ctx context.Context, token string) (Tag, bool, error) {
	tok := strings.ToLower(strings.TrimSpace(token))
	if tok == "" {
		return Tag{}, false, nil
	}

	// An alias wins even when it looks like it could be a namespace fragment: the
	// alias table is the explicit shortcut a person configured.
	if t, ok, err := s.resolveAlias(ctx, tok); err != nil || ok {
		return t, ok, err
	}

	if strings.Contains(tok, ":") {
		namespace, name, err := Parse(tok)
		if err != nil {
			return Tag{}, false, nil // malformed is simply "not a tag", not an error
		}
		return s.getTag(ctx, namespace, name)
	}
	return Tag{}, false, nil
}

func (s *Store) resolveAlias(ctx context.Context, alias string) (Tag, bool, error) {
	row := s.db.Reader.QueryRowContext(ctx, `
		SELECT `+tagColumns+`
		FROM tags t JOIN tag_aliases al ON al.tag_id = t.id
		WHERE al.alias = ?`, alias)
	return scanTagRow(row)
}

// GetByCanonical returns the tag for a canonical string if it exists.
func (s *Store) GetByCanonical(ctx context.Context, canonical string) (Tag, bool, error) {
	namespace, name, err := Parse(canonical)
	if err != nil {
		return Tag{}, false, err
	}
	return s.getTag(ctx, namespace, name)
}

func (s *Store) getTag(ctx context.Context, namespace, name string) (Tag, bool, error) {
	row := s.db.Reader.QueryRowContext(ctx,
		`SELECT `+tagColumns+` FROM tags t WHERE t.namespace = ? AND t.name = ?`, namespace, name)
	return scanTagRow(row)
}

func getTagTx(ctx context.Context, tx *sql.Tx, namespace, name string) (Tag, bool, error) {
	row := tx.QueryRowContext(ctx,
		`SELECT `+tagColumns+` FROM tags t WHERE t.namespace = ? AND t.name = ?`, namespace, name)
	return scanTagRow(row)
}

const tagColumns = `t.id, t.namespace, t.name, t.description, t.parent_id, t.created_at, t.updated_at`

type tagScanner interface{ Scan(dest ...any) error }

func scanTagRow(row tagScanner) (Tag, bool, error) {
	var (
		t        Tag
		parentID sql.NullInt64
		created  int64
		updated  int64
	)
	err := row.Scan(&t.ID, &t.Namespace, &t.Name, &t.Description, &parentID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Tag{}, false, nil
	}
	if err != nil {
		return Tag{}, false, err
	}
	if parentID.Valid {
		id := parentID.Int64
		t.ParentID = &id
	}
	t.CreatedAt = time.Unix(created, 0)
	t.UpdatedAt = time.Unix(updated, 0)
	return t, true, nil
}

// SetAlias points a bare alias at a canonical tag, creating the tag if needed.
// It is idempotent, and re-pointing an existing alias to a different tag moves
// it rather than failing.
func (s *Store) SetAlias(ctx context.Context, alias, canonical string) (Tag, error) {
	al := strings.ToLower(strings.TrimSpace(alias))
	if err := validSegment(al); err != nil {
		return Tag{}, fmt.Errorf("%w: alias %q: %v", ErrInvalidTag, alias, err)
	}
	if strings.Contains(al, ":") {
		return Tag{}, fmt.Errorf("%w: alias %q must not contain ':'", ErrInvalidTag, alias)
	}
	t, err := s.Ensure(ctx, canonical)
	if err != nil {
		return Tag{}, err
	}
	now := s.now().Unix()
	if _, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO tag_aliases (tag_id, alias, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(alias) DO UPDATE SET tag_id = excluded.tag_id, updated_at = excluded.updated_at`,
		t.ID, al, now, now); err != nil {
		return Tag{}, fmt.Errorf("set alias %q: %w", al, err)
	}
	return t, nil
}

// Aliases lists the aliases pointing at a tag, sorted.
func (s *Store) Aliases(ctx context.Context, tagID int64) ([]string, error) {
	rows, err := s.db.Reader.QueryContext(ctx,
		`SELECT alias FROM tag_aliases WHERE tag_id = ? ORDER BY alias`, tagID)
	if err != nil {
		return nil, fmt.Errorf("list aliases: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DescendantIDs returns the tag and all tags beneath it in the hierarchy — the
// set a §7 tag filter expands to, so searching `type:sfx` also returns
// `type:sfx:impact`.
func (s *Store) DescendantIDs(ctx context.Context, tagID int64) ([]int64, error) {
	rows, err := s.db.Reader.QueryContext(ctx,
		`SELECT descendant_id FROM tag_closure WHERE ancestor_id = ? ORDER BY depth, descendant_id`, tagID)
	if err != nil {
		return nil, fmt.Errorf("descendants of %d: %w", tagID, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// sourceRank orders the sources so a manual tag is never demoted by a later
// machine pass. Equal ranks keep whatever is already stored.
func sourceRank(source string) int {
	switch source {
	case SourceManual:
		return 3
	case SourceAutoPath, SourceAutoType:
		return 2
	case SourceInherited:
		return 1
	default:
		return 0
	}
}

// TagAsset attaches a tag to an asset, ensuring the tag exists first. If the
// asset already carries the tag, the higher-ranked source wins (§7). createdBy
// is the acting user for a manual tag, nil for machine sources. It returns the
// tag and rewrites the asset's FTS row.
func (s *Store) TagAsset(ctx context.Context, assetID int64, canonical, source string, createdBy *int64) (Tag, error) {
	if sourceRank(source) == 0 {
		return Tag{}, fmt.Errorf("tag asset: unknown source %q", source)
	}
	t, err := s.Ensure(ctx, canonical)
	if err != nil {
		return Tag{}, err
	}

	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return Tag{}, fmt.Errorf("tag asset: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if err := upsertAssetTagTx(ctx, tx, assetID, t.ID, source, createdBy, s.now().Unix()); err != nil {
		return Tag{}, err
	}
	if err := reindexAssetFTS(ctx, tx, assetID); err != nil {
		return Tag{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tag{}, fmt.Errorf("tag asset: commit: %w", err)
	}
	return t, nil
}

// AssetTagItem is one already-ensured tag to apply to an asset in a batch.
type AssetTagItem struct {
	TagID     int64
	Source    string
	CreatedBy *int64
}

// ApplyAssetTags attaches several pre-ensured tags to one asset in a single
// transaction, reindexing its FTS row once at the end. It is the bulk path the
// auto-tagger uses, where re-ensuring and re-indexing per tag would be needless
// work; source precedence (§7) is applied per tag exactly as TagAsset does.
func (s *Store) ApplyAssetTags(ctx context.Context, assetID int64, items []AssetTagItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("apply asset tags: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := s.now().Unix()
	for _, it := range items {
		if sourceRank(it.Source) == 0 {
			return fmt.Errorf("apply asset tags: unknown source %q", it.Source)
		}
		if err := upsertAssetTagTx(ctx, tx, assetID, it.TagID, it.Source, it.CreatedBy, now); err != nil {
			return err
		}
	}
	if err := reindexAssetFTS(ctx, tx, assetID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("apply asset tags: commit: %w", err)
	}
	return nil
}

// upsertAssetTagTx inserts an asset tag, or upgrades its source when the new
// source outranks the stored one (§7: a manual tag is never demoted by a machine
// pass). It never reindexes FTS — the caller does that once for the asset.
func upsertAssetTagTx(ctx context.Context, tx *sql.Tx, assetID, tagID int64, source string, createdBy *int64, now int64) error {
	var existing string
	err := tx.QueryRowContext(ctx,
		`SELECT source FROM asset_tags WHERE asset_id = ? AND tag_id = ?`, assetID, tagID).Scan(&existing)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO asset_tags (asset_id, tag_id, source, created_by, created_at)
			VALUES (?, ?, ?, ?, ?)`, assetID, tagID, source, createdBy, now); err != nil {
			return fmt.Errorf("insert asset_tag: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read existing asset_tag: %w", err)
	default:
		if sourceRank(source) > sourceRank(existing) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE asset_tags SET source = ?, created_by = ? WHERE asset_id = ? AND tag_id = ?`,
				source, createdBy, assetID, tagID); err != nil {
				return fmt.Errorf("upgrade asset_tag source: %w", err)
			}
		}
	}
	return nil
}

// TagAssets applies one tag to many assets in a single transaction — §7's bulk
// tagging, both "tag the selected tiles" and "tag everything matching this
// search". The tag is ensured once; each asset's source precedence and FTS row
// are handled exactly as TagAsset does. It returns the number of assets touched.
func (s *Store) TagAssets(ctx context.Context, assetIDs []int64, canonical, source string, createdBy *int64) (int, error) {
	if sourceRank(source) == 0 {
		return 0, fmt.Errorf("tag assets: unknown source %q", source)
	}
	if len(assetIDs) == 0 {
		return 0, nil
	}
	t, err := s.Ensure(ctx, canonical)
	if err != nil {
		return 0, err
	}

	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("tag assets: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	now := s.now().Unix()
	for _, id := range assetIDs {
		if err := upsertAssetTagTx(ctx, tx, id, t.ID, source, createdBy, now); err != nil {
			return 0, err
		}
		if err := reindexAssetFTS(ctx, tx, id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("tag assets: commit: %w", err)
	}
	return len(assetIDs), nil
}

// UntagAsset removes a tag from an asset and rewrites its FTS row. Removing a
// tag the asset never had is not an error.
func (s *Store) UntagAsset(ctx context.Context, assetID, tagID int64) error {
	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("untag asset: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM asset_tags WHERE asset_id = ? AND tag_id = ?`, assetID, tagID); err != nil {
		return fmt.Errorf("delete asset_tag: %w", err)
	}
	if err := reindexAssetFTS(ctx, tx, assetID); err != nil {
		return err
	}
	return tx.Commit()
}

// TagPack attaches a tag to a pack, ensuring it exists, and rewrites the FTS
// rows of every member asset so the inherited tag is searchable on them (§7).
func (s *Store) TagPack(ctx context.Context, packID int64, canonical, source string, createdBy *int64) (Tag, error) {
	if source != SourceManual && source != SourceAutoPath && source != SourceAutoType {
		return Tag{}, fmt.Errorf("tag pack: invalid source %q", source)
	}
	t, err := s.Ensure(ctx, canonical)
	if err != nil {
		return Tag{}, err
	}

	tx, err := s.db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return Tag{}, fmt.Errorf("tag pack: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pack_tags (pack_id, tag_id, source, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(pack_id, tag_id) DO NOTHING`,
		packID, t.ID, source, createdBy, s.now().Unix()); err != nil {
		return Tag{}, fmt.Errorf("insert pack_tag: %w", err)
	}
	if err := reindexPackMembersFTS(ctx, tx, packID); err != nil {
		return Tag{}, err
	}
	if err := tx.Commit(); err != nil {
		return Tag{}, fmt.Errorf("tag pack: commit: %w", err)
	}
	return t, nil
}

// AssetTag is a tag on an asset as the detail page needs it: the tag itself, how
// it got there, and whether it is inherited from the pack rather than direct.
type AssetTag struct {
	Tag       Tag
	Source    string
	Inherited bool
}

// AssetTags lists an asset's direct tags and the pack tags it inherits (§7).
// Inherited tags an asset also holds directly appear once, as direct.
func (s *Store) AssetTags(ctx context.Context, assetID int64) ([]AssetTag, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT `+tagColumns+`, at.source, 0 AS inherited
		FROM asset_tags at JOIN tags t ON t.id = at.tag_id
		WHERE at.asset_id = ?
		UNION ALL
		SELECT `+tagColumns+`, pt.source, 1 AS inherited
		FROM pack_tags pt
		JOIN tags t ON t.id = pt.tag_id
		JOIN assets a ON a.pack_id = pt.pack_id
		WHERE a.id = ?
		  AND pt.tag_id NOT IN (SELECT tag_id FROM asset_tags WHERE asset_id = ?)
		ORDER BY 2, 3`, // namespace, then name
		assetID, assetID, assetID)
	if err != nil {
		return nil, fmt.Errorf("asset tags: %w", err)
	}
	defer rows.Close()

	var out []AssetTag
	for rows.Next() {
		var (
			t         Tag
			parentID  sql.NullInt64
			created   int64
			updated   int64
			source    string
			inherited int64
		)
		if err := rows.Scan(&t.ID, &t.Namespace, &t.Name, &t.Description, &parentID,
			&created, &updated, &source, &inherited); err != nil {
			return nil, err
		}
		if parentID.Valid {
			id := parentID.Int64
			t.ParentID = &id
		}
		t.CreatedAt = time.Unix(created, 0)
		t.UpdatedAt = time.Unix(updated, 0)
		out = append(out, AssetTag{Tag: t, Source: source, Inherited: inherited != 0})
	}
	return out, rows.Err()
}

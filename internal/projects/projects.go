// Package projects records which assets a Godot project uses (§10), keyed on the
// project's UUID rather than any filesystem path (invariant 10). It is what feeds
// CREDITS.md and the "already imported / outdated" badges, and what M13 will
// consult to keep an in-use asset off every removal list (invariant 5).
package projects

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// Store reads and writes projects and their uses.
type Store struct {
	db  *db.DB
	now func() time.Time
}

// NewStore wraps a database.
func NewStore(database *db.DB) *Store { return &Store{db: database, now: time.Now} }

// WithClock replaces the clock, for tests.
func (s *Store) WithClock(now func() time.Time) *Store { s.now = now; return s }

// ensureProject gets-or-creates the project row for a UUID and returns its id.
func (s *Store) ensureProject(ctx context.Context, uuid, name string) (int64, error) {
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return 0, fmt.Errorf("project uuid is required")
	}
	now := s.now().Unix()
	if _, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO projects (uuid, name, created_at, updated_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(uuid) DO UPDATE SET
		    name = CASE WHEN excluded.name != '' THEN excluded.name ELSE projects.name END,
		    updated_at = excluded.updated_at`,
		uuid, name, now, now); err != nil {
		return 0, fmt.Errorf("ensure project %s: %w", uuid, err)
	}
	var id int64
	if err := s.db.Reader.QueryRowContext(ctx, `SELECT id FROM projects WHERE uuid = ?`, uuid).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// RecordUse registers an asset placed into a project (§10). It is idempotent on
// (project, asset, res_path): a repeat re-activates a previously removed row
// rather than duplicating it, so two people importing the same asset produce one
// row. It returns the use id.
func (s *Store) RecordUse(ctx context.Context, uuid, projectName string, assetID int64, resPath, sha256 string) (int64, error) {
	resPath = strings.TrimSpace(resPath)
	if resPath == "" {
		return 0, fmt.Errorf("res_path is required")
	}
	projectID, err := s.ensureProject(ctx, uuid, projectName)
	if err != nil {
		return 0, err
	}
	assetID = s.resolveByContent(ctx, assetID, sha256)
	now := s.now().Unix()
	if _, err := s.db.Writer.ExecContext(ctx, `
		INSERT INTO project_uses (project_id, asset_id, res_path, asset_sha256, added_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(project_id, asset_id, res_path) DO UPDATE SET
		    asset_sha256 = excluded.asset_sha256,
		    added_at     = excluded.added_at,
		    removed_at   = NULL`,
		projectID, assetID, resPath, sha256, now); err != nil {
		return 0, fmt.Errorf("record use: %w", err)
	}
	var id int64
	if err := s.db.Reader.QueryRowContext(ctx,
		`SELECT id FROM project_uses WHERE project_id = ? AND asset_id = ? AND res_path = ?`,
		projectID, assetID, resPath).Scan(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// resolveByContent corrects an asset id that disagrees with the content hash recorded
// with it, and otherwise returns the id unchanged.
//
// A use row is an attribution: it is what CREDITS.md is built from and what keeps an
// asset off every removal list (invariant 5). Recording one against the wrong asset
// credits the wrong pack — silently, and in a file people ship. That happened: a project
// recorded asset A while holding the bytes of asset B, and the credits named a pack
// nobody had ever chosen.
//
// The hash discriminates the two cases that look alike:
//
//   - the hash belongs to *another* asset → the id is wrong and the content says which
//     one is right, so record that one.
//   - the hash belongs to nothing here → the client is holding an older copy than the
//     library has (§10's outdated badge, and what `Sync` replays). Record it as given;
//     that is a legitimate state, not a mistake.
//
// A hash shared by several assets — exact duplicates, which §9.1 says this library is
// full of — resolves to the lowest id, deterministically. Any of them is the same bytes;
// the pack they are credited to may differ, and a stable choice at least means two
// clients agree.
func (s *Store) resolveByContent(ctx context.Context, assetID int64, sha256 string) int64 {
	if sha256 == "" {
		return assetID
	}
	var current string
	err := s.db.Reader.QueryRowContext(ctx,
		`SELECT sha256 FROM assets WHERE id = ?`, assetID).Scan(&current)
	if err != nil || current == sha256 {
		// Unknown id or an exact match: nothing to correct. An id that names no asset is
		// left alone so the insert fails on its foreign key rather than here.
		return assetID
	}

	var byContent int64
	if err := s.db.Reader.QueryRowContext(ctx,
		`SELECT id FROM assets WHERE sha256 = ? ORDER BY id LIMIT 1`, sha256).Scan(&byContent); err != nil {
		return assetID // no asset has this content: an outdated copy, recorded as given
	}
	return byContent
}

// RemoveUse soft-removes a use by id, scoped to the project so an id from another
// project cannot be removed by mistake.
func (s *Store) RemoveUse(ctx context.Context, uuid string, useID int64) error {
	projectID, err := s.projectID(ctx, uuid)
	if err != nil {
		return err
	}
	_, err = s.db.Writer.ExecContext(ctx, `
		UPDATE project_uses SET removed_at = ?
		WHERE id = ? AND project_id = ? AND removed_at IS NULL`,
		s.now().Unix(), useID, projectID)
	return err
}

func (s *Store) projectID(ctx context.Context, uuid string) (int64, error) {
	var id int64
	err := s.db.Reader.QueryRowContext(ctx, `SELECT id FROM projects WHERE uuid = ?`, uuid).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("unknown project %s", uuid)
	}
	return id, err
}

// CreditLine is one asset's provenance as CREDITS.md needs it.
type CreditLine struct {
	Pack            string
	Author          string
	License         string // SPDX id, "" when unlicensed
	SourceURL       string
	AttributionText string
}

// Credits returns the used assets' provenance grouped for CREDITS.md (§9): only
// assets actually used, deduplicated by pack, so a project using forty sprites
// from one pack credits it once. Ordered by licence then author.
func (s *Store) Credits(ctx context.Context, uuid string) ([]CreditLine, error) {
	projectID, err := s.projectID(ctx, uuid)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT DISTINCT p.name, p.source_author, coalesce(l.spdx_id, ''), p.source_url, p.attribution_text
		FROM project_uses pu
		JOIN assets a ON a.id = pu.asset_id
		JOIN packs p ON p.id = a.pack_id
		LEFT JOIN licenses l ON l.id = p.license_id
		WHERE pu.project_id = ? AND pu.removed_at IS NULL
		ORDER BY coalesce(l.spdx_id, 'zzz'), p.source_author, p.name`, projectID)
	if err != nil {
		return nil, fmt.Errorf("credits: %w", err)
	}
	defer rows.Close()

	var out []CreditLine
	for rows.Next() {
		var c CreditLine
		if err := rows.Scan(&c.Pack, &c.Author, &c.License, &c.SourceURL, &c.AttributionText); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RenderCredits turns the credit lines into a CREDITS.md document (§9: group by
// licence then author, emit attribution text and source URLs).
func RenderCredits(projectName string, lines []CreditLine) string {
	var b strings.Builder
	title := projectName
	if title == "" {
		title = "Project"
	}
	fmt.Fprintf(&b, "# Credits — %s\n\n", title)
	if len(lines) == 0 {
		b.WriteString("_No assets recorded yet._\n")
		return b.String()
	}

	byLicense := map[string][]CreditLine{}
	var order []string
	for _, l := range lines {
		key := l.License
		if key == "" {
			key = "Unverified"
		}
		if _, seen := byLicense[key]; !seen {
			order = append(order, key)
		}
		byLicense[key] = append(byLicense[key], l)
	}
	for _, lic := range order {
		fmt.Fprintf(&b, "## %s\n\n", lic)
		for _, l := range byLicense[lic] {
			line := "- **" + l.Pack + "**"
			if l.Author != "" {
				line += " by " + l.Author
			}
			if l.SourceURL != "" {
				line += " — " + l.SourceURL
			}
			b.WriteString(line + "\n")
			if l.AttributionText != "" {
				b.WriteString("  > " + l.AttributionText + "\n")
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// AssetUse is one project that uses an asset, for the asset page (M14). §10's plugin
// pushes assets into a project; this is the library's side of that conversation —
// "this file is already in these projects", which is also why removal refuses to
// touch it (invariant 5).
type AssetUse struct {
	ProjectName string
	ProjectUUID string
	ResPath     string
	AddedAt     time.Time
	// Outdated is true when the content hash recorded at import time no longer
	// matches the library's current one (§10's outdated badge).
	Outdated bool
}

// ProjectUse is one asset a project holds, as the project's own side needs to see it.
//
// The reverse of UsesOfAsset, and it exists for the editor plugin's "in this project"
// screen (§10, M18). The plugin has always had `res://.ambar/manifest.json` — committed,
// merged additively, the local record of every import — and no way to compare it with
// anything. Without this it cannot answer the two questions that matter: has the library
// moved on since I imported this, and did the server ever hear about it?
type ProjectUse struct {
	// ID is the use row, which is what DELETE /uses/{id} takes.
	ID      int64
	AssetID int64
	ResPath string
	AddedAt time.Time
	// ImportedSHA256 is the content hash recorded when it was imported; SHA256 is the
	// library's now. Different means the library has a newer version (§10's outdated
	// badge) — the plugin holds the old bytes.
	ImportedSHA256 string
	SHA256         string

	Filename string
	Ext      string
	Kind     string
	Size     int64
	PackName string
	// Missing is the library's own missing-file state (§12), not the project's: the file
	// this was imported from is no longer on disk in the library.
	Missing bool
}

// Outdated reports whether the library's copy has changed since the import.
func (u ProjectUse) Outdated() bool {
	return u.ImportedSHA256 != "" && u.ImportedSHA256 != u.SHA256
}

// UsesOfProject lists everything one project holds, newest import first.
func (s *Store) UsesOfProject(ctx context.Context, uuid string) ([]ProjectUse, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT u.id, u.asset_id, u.res_path, u.added_at, u.asset_sha256,
		       a.filename, a.ext, a.kind, a.size, a.sha256,
		       p.name, a.missing_since IS NOT NULL
		FROM project_uses u
		JOIN projects pr ON pr.id = u.project_id
		JOIN assets a ON a.id = u.asset_id
		JOIN packs p ON p.id = a.pack_id
		WHERE pr.uuid = ? AND u.removed_at IS NULL
		ORDER BY u.added_at DESC, u.id DESC`, strings.TrimSpace(uuid))
	if err != nil {
		return nil, fmt.Errorf("list uses of project %s: %w", uuid, err)
	}
	defer rows.Close()

	var out []ProjectUse
	for rows.Next() {
		var (
			use     ProjectUse
			addedAt int64
			missing int
		)
		if err := rows.Scan(&use.ID, &use.AssetID, &use.ResPath, &addedAt, &use.ImportedSHA256,
			&use.Filename, &use.Ext, &use.Kind, &use.Size, &use.SHA256,
			&use.PackName, &missing); err != nil {
			return nil, fmt.Errorf("scan project use: %w", err)
		}
		use.AddedAt = time.Unix(addedAt, 0)
		use.Missing = missing == 1
		out = append(out, use)
	}
	return out, rows.Err()
}

// UsesOfAsset lists the active project uses of one asset, newest first.
func (s *Store) UsesOfAsset(ctx context.Context, assetID int64) ([]AssetUse, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT CASE WHEN p.name != '' THEN p.name ELSE p.uuid END, p.uuid, u.res_path, u.added_at,
		       CASE WHEN u.asset_sha256 != '' AND u.asset_sha256 != a.sha256 THEN 1 ELSE 0 END
		FROM project_uses u
		JOIN projects p ON p.id = u.project_id
		JOIN assets a ON a.id = u.asset_id
		WHERE u.asset_id = ? AND u.removed_at IS NULL
		ORDER BY u.added_at DESC`, assetID)
	if err != nil {
		return nil, fmt.Errorf("list uses of asset %d: %w", assetID, err)
	}
	defer rows.Close()

	var out []AssetUse
	for rows.Next() {
		var (
			use      AssetUse
			addedAt  int64
			outdated int
		)
		if err := rows.Scan(&use.ProjectName, &use.ProjectUUID, &use.ResPath, &addedAt, &outdated); err != nil {
			return nil, fmt.Errorf("scan asset use: %w", err)
		}
		use.AddedAt = time.Unix(addedAt, 0)
		use.Outdated = outdated == 1
		out = append(out, use)
	}
	return out, rows.Err()
}

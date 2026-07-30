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

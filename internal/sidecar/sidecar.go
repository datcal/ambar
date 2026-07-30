// Package sidecar reads and writes the per-pack .ambar.json of §3 — the file that
// makes the database genuinely disposable and lets a copied folder carry its own
// metadata. It holds only human-authored metadata: provenance, licence, notes,
// and manual tags. Auto tags are derived (they regenerate from `ambar retag`), so
// they are deliberately not written here.
//
// On a read-only library the sidecar is written into $DATA_ROOT/sidecars/
// mirroring the tree instead of beside the originals (§3), because invariant 1
// forbids touching the library there.
package sidecar

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/datcal/ambar/internal/db"
	"github.com/datcal/ambar/internal/provenance"
	"github.com/datcal/ambar/internal/safepath"
	"github.com/datcal/ambar/internal/tags"
)

// FileName is the sidecar's name within a pack directory.
const FileName = ".ambar.json"

// schemaVersion is bumped when the on-disk shape changes incompatibly.
const schemaVersion = 1

// Sidecar is the on-disk document.
type Sidecar struct {
	Version   int         `json:"version"`
	UpdatedAt int64       `json:"updated_at"` // unix seconds; the conflict tiebreaker (§3)
	Pack      PackMeta    `json:"pack"`
	Assets    []AssetMeta `json:"assets,omitempty"`
}

// PackMeta is the pack's provenance and manual tags.
type PackMeta struct {
	Name                string   `json:"name,omitempty"`
	SourceURL           string   `json:"source_url,omitempty"`
	SourceSite          string   `json:"source_site,omitempty"`
	SourceAuthor        string   `json:"source_author,omitempty"`
	SourceAuthorURL     string   `json:"source_author_url,omitempty"`
	License             string   `json:"license,omitempty"` // SPDX id
	LicenseNote         string   `json:"license_note,omitempty"`
	AttributionRequired bool     `json:"attribution_required,omitempty"`
	AttributionText     string   `json:"attribution_text,omitempty"`
	AcquiredAt          *int64   `json:"acquired_at,omitempty"`
	PricePaidCents      *int64   `json:"price_paid_cents,omitempty"`
	Currency            string   `json:"currency,omitempty"`
	OrderRef            string   `json:"order_ref,omitempty"`
	OriginalArchiveName string   `json:"original_archive_name,omitempty"`
	OriginalArchiveSHA  string   `json:"original_archive_sha256,omitempty"`
	OriginalArchiveSize *int64   `json:"original_archive_size,omitempty"`
	ProvenanceState     string   `json:"provenance_state,omitempty"`
	Notes               string   `json:"notes,omitempty"`
	Tags                []string `json:"tags,omitempty"` // canonical, manual only
}

// AssetMeta is one file's manual tags.
type AssetMeta struct {
	Path string   `json:"path"` // pack-relative rel_path
	Tags []string `json:"tags,omitempty"`
}

// Manager builds, writes, reads and imports sidecars.
type Manager struct {
	db          *db.DB
	prov        *provenance.Store
	tags        *tags.Store
	libraryRoot string
	dataRoot    string
	readonly    bool
	log         *slog.Logger
	now         func() time.Time
}

// Options configures a Manager.
type Options struct {
	LibraryRoot string
	DataRoot    string
	Readonly    bool
	Log         *slog.Logger
}

// New builds a Manager.
func New(database *db.DB, opts Options) *Manager {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &Manager{
		db:          database,
		prov:        provenance.NewStore(database),
		tags:        tags.NewStore(database),
		libraryRoot: opts.LibraryRoot,
		dataRoot:    opts.DataRoot,
		readonly:    opts.Readonly,
		log:         log,
		now:         time.Now,
	}
}

// WithClock replaces the clock, for tests.
func (m *Manager) WithClock(now func() time.Time) *Manager {
	m.now = now
	return m
}

// Build reconstructs a pack's sidecar document from the database.
func (m *Manager) Build(ctx context.Context, packID int64) (Sidecar, error) {
	prov, err := m.prov.Get(ctx, packID)
	if err != nil {
		return Sidecar{}, err
	}

	var name string
	if err := m.db.Reader.QueryRowContext(ctx, `SELECT name FROM packs WHERE id = ?`, packID).Scan(&name); err != nil {
		return Sidecar{}, fmt.Errorf("sidecar: read pack name: %w", err)
	}

	license := ""
	if prov.LicenseID != nil {
		if err := m.db.Reader.QueryRowContext(ctx,
			`SELECT spdx_id FROM licenses WHERE id = ?`, *prov.LicenseID).Scan(&license); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return Sidecar{}, err
		}
	}

	packTags, err := m.manualPackTags(ctx, packID)
	if err != nil {
		return Sidecar{}, err
	}
	assets, err := m.manualAssetTags(ctx, packID)
	if err != nil {
		return Sidecar{}, err
	}

	return Sidecar{
		Version:   schemaVersion,
		UpdatedAt: m.now().Unix(),
		Pack: PackMeta{
			Name:                name,
			SourceURL:           prov.SourceURL,
			SourceSite:          prov.SourceSite,
			SourceAuthor:        prov.SourceAuthor,
			SourceAuthorURL:     prov.SourceAuthorURL,
			License:             license,
			LicenseNote:         prov.LicenseNote,
			AttributionRequired: prov.AttributionRequired,
			AttributionText:     prov.AttributionText,
			AcquiredAt:          unixPtr(prov.AcquiredAt),
			PricePaidCents:      prov.PricePaidCents,
			Currency:            prov.Currency,
			OrderRef:            prov.OrderRef,
			OriginalArchiveName: prov.OriginalArchiveName,
			OriginalArchiveSHA:  prov.OriginalArchiveSHA,
			OriginalArchiveSize: prov.OriginalArchiveSize,
			ProvenanceState:     prov.State,
			Notes:               prov.Notes,
			Tags:                packTags,
		},
		Assets: assets,
	}, nil
}

func (m *Manager) manualPackTags(ctx context.Context, packID int64) ([]string, error) {
	rows, err := m.db.Reader.QueryContext(ctx, `
		SELECT t.namespace || ':' || t.name
		FROM pack_tags pt JOIN tags t ON t.id = pt.tag_id
		WHERE pt.pack_id = ? AND pt.source = 'manual'
		ORDER BY 1`, packID)
	if err != nil {
		return nil, fmt.Errorf("sidecar: pack tags: %w", err)
	}
	defer rows.Close()
	return scanStrings(rows)
}

func (m *Manager) manualAssetTags(ctx context.Context, packID int64) ([]AssetMeta, error) {
	rows, err := m.db.Reader.QueryContext(ctx, `
		SELECT a.rel_path, t.namespace || ':' || t.name
		FROM asset_tags at
		JOIN tags t ON t.id = at.tag_id
		JOIN assets a ON a.id = at.asset_id
		WHERE a.pack_id = ? AND at.source = 'manual'
		ORDER BY a.rel_path, 2`, packID)
	if err != nil {
		return nil, fmt.Errorf("sidecar: asset tags: %w", err)
	}
	defer rows.Close()

	byPath := map[string][]string{}
	var order []string
	for rows.Next() {
		var relPath, canon string
		if err := rows.Scan(&relPath, &canon); err != nil {
			return nil, err
		}
		if _, seen := byPath[relPath]; !seen {
			order = append(order, relPath)
		}
		byPath[relPath] = append(byPath[relPath], canon)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]AssetMeta, 0, len(order))
	for _, p := range order {
		out = append(out, AssetMeta{Path: p, Tags: byPath[p]})
	}
	return out, nil
}

// Write builds and writes a pack's sidecar to its resting place (§3), atomically.
func (m *Manager) Write(ctx context.Context, packID int64, packRelPath string) error {
	sc, err := m.Build(ctx, packID)
	if err != nil {
		return err
	}
	path, err := m.pathFor(packRelPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("sidecar: create dir: %w", err)
	}
	data, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}

// Read parses a sidecar file.
func Read(path string) (Sidecar, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Sidecar{}, err
	}
	var sc Sidecar
	if err := json.Unmarshal(data, &sc); err != nil {
		return Sidecar{}, fmt.Errorf("sidecar: parse %s: %w", path, err)
	}
	return sc, nil
}

// pathFor returns where a pack's sidecar lives: beside the originals, or in the
// $DATA_ROOT/sidecars mirror when the library is read-only (§3).
func (m *Manager) pathFor(packRelPath string) (string, error) {
	if m.readonly {
		// The mirror is under our own data root, not the library. safepath.Resolve
		// needs its root to exist, so ensure the mirror base is there before
		// resolving the (still traversal-guarded) relative path under it.
		mirror := filepath.Join(m.dataRoot, "sidecars")
		if err := os.MkdirAll(mirror, 0o755); err != nil {
			return "", fmt.Errorf("sidecar: create mirror base: %w", err)
		}
		abs, err := safepath.Resolve(mirror, packRelPath+"/"+FileName)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	abs, err := safepath.Resolve(m.libraryRoot, packRelPath+"/"+FileName)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// ReadForPack reads a pack's sidecar if present, reporting whether it existed.
func (m *Manager) ReadForPack(packRelPath string) (Sidecar, bool, error) {
	path, err := m.pathFor(packRelPath)
	if err != nil {
		return Sidecar{}, false, err
	}
	sc, err := Read(path)
	if os.IsNotExist(err) {
		return Sidecar{}, false, nil
	}
	if err != nil {
		return Sidecar{}, false, err
	}
	return sc, true, nil
}

func scanStrings(rows *sql.Rows) ([]string, error) {
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func unixPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	u := t.Unix()
	return &u
}

// atomicWrite writes data to a temp file in the same directory and renames it
// over the target, so a reader never sees a half-written sidecar.
func atomicWrite(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".ambar-*.tmp")
	if err != nil {
		return fmt.Errorf("sidecar: temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("sidecar: rename: %w", err)
	}
	return nil
}

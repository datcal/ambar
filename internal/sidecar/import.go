package sidecar

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/datcal/ambar/internal/provenance"
	"github.com/datcal/ambar/internal/tags"
)

// ImportForPack applies a sidecar to a pack when the database does not already
// carry that metadata, or when the sidecar is strictly newer (§3: "if a sidecar
// exists and the DB row does not, import ... on conflict, newest updated_at
// wins"). It reports whether it imported.
func (m *Manager) ImportForPack(ctx context.Context, packID int64, packRelPath string, sc Sidecar) (bool, error) {
	prov, err := m.prov.Get(ctx, packID)
	if err != nil {
		return false, err
	}
	packTags, err := m.manualPackTags(ctx, packID)
	if err != nil {
		return false, err
	}
	assetTags, err := m.manualAssetTags(ctx, packID)
	if err != nil {
		return false, err
	}

	dbHasMeta := hasMetadata(prov, packTags, assetTags)
	if dbHasMeta {
		var updatedAt int64
		if err := m.db.Reader.QueryRowContext(ctx,
			`SELECT updated_at FROM packs WHERE id = ?`, packID).Scan(&updatedAt); err != nil {
			return false, err
		}
		if sc.UpdatedAt <= updatedAt {
			// The database is at least as new: keep it, but note the divergence so a
			// silent drift is visible (§3).
			m.log.InfoContext(ctx, "sidecar older than the index; keeping the database",
				"pack", packRelPath)
			return false, nil
		}
		m.log.InfoContext(ctx, "sidecar newer than the index; importing it", "pack", packRelPath)
	}

	if err := m.apply(ctx, packID, sc); err != nil {
		return false, err
	}
	return true, nil
}

// hasMetadata reports whether a pack already carries human-authored metadata, so
// import knows whether "the DB row does not carry it" (§3).
func hasMetadata(p provenance.Provenance, packTags []string, assetTags []AssetMeta) bool {
	return p.SourceURL != "" || p.LicenseID != nil || p.Notes != "" ||
		p.AttributionText != "" || p.State == provenance.StateComplete ||
		len(packTags) > 0 || len(assetTags) > 0
}

// apply writes a sidecar's provenance and manual tags into the database.
func (m *Manager) apply(ctx context.Context, packID int64, sc Sidecar) error {
	var licenseID *int64
	if sc.Pack.License != "" {
		if l, ok, err := m.prov.LicenseBySPDX(ctx, sc.Pack.License); err != nil {
			return err
		} else if ok {
			licenseID = &l.ID
		} else {
			m.log.WarnContext(ctx, "sidecar names an unknown licence", "spdx", sc.Pack.License)
		}
	}

	state := sc.Pack.ProvenanceState
	if state != provenance.StateComplete && state != provenance.StateNeedsProvenance {
		state = provenance.StateNeedsProvenance
	}

	p := provenance.Provenance{
		PackID:              packID,
		SourceURL:           sc.Pack.SourceURL,
		SourceSite:          sc.Pack.SourceSite,
		SourceAuthor:        sc.Pack.SourceAuthor,
		SourceAuthorURL:     sc.Pack.SourceAuthorURL,
		LicenseID:           licenseID,
		LicenseNote:         sc.Pack.LicenseNote,
		AttributionRequired: sc.Pack.AttributionRequired,
		AttributionText:     sc.Pack.AttributionText,
		PricePaidCents:      sc.Pack.PricePaidCents,
		Currency:            sc.Pack.Currency,
		OrderRef:            sc.Pack.OrderRef,
		OriginalArchiveName: sc.Pack.OriginalArchiveName,
		OriginalArchiveSHA:  sc.Pack.OriginalArchiveSHA,
		OriginalArchiveSize: sc.Pack.OriginalArchiveSize,
		State:               state,
		Notes:               sc.Pack.Notes,
	}
	if sc.Pack.AcquiredAt != nil {
		t := time.Unix(*sc.Pack.AcquiredAt, 0)
		p.AcquiredAt = &t
	}
	if err := m.prov.Update(ctx, p); err != nil {
		return err
	}

	for _, canon := range sc.Pack.Tags {
		if _, err := m.tags.TagPack(ctx, packID, canon, tags.SourceManual, nil); err != nil {
			return fmt.Errorf("sidecar: apply pack tag %q: %w", canon, err)
		}
	}
	for _, a := range sc.Assets {
		var assetID int64
		err := m.db.Reader.QueryRowContext(ctx,
			`SELECT id FROM assets WHERE pack_id = ? AND rel_path = ?`, packID, a.Path).Scan(&assetID)
		if errors.Is(err, sql.ErrNoRows) {
			// The file named in the sidecar is not (yet) indexed; skip it rather than
			// fail the whole import.
			continue
		}
		if err != nil {
			return err
		}
		for _, canon := range a.Tags {
			if _, err := m.tags.TagAsset(ctx, assetID, canon, tags.SourceManual, nil); err != nil {
				return fmt.Errorf("sidecar: apply asset tag %q: %w", canon, err)
			}
		}
	}
	return nil
}

// ImportAll walks every pack and imports its sidecar where present. Run after a
// scan so a rebuilt or freshly-populated index recovers the metadata beside the
// files (§3). It returns how many packs it imported.
func (m *Manager) ImportAll(ctx context.Context) (int, error) {
	packs, err := m.allPacks(ctx)
	if err != nil {
		return 0, err
	}
	imported := 0
	for _, p := range packs {
		sc, ok, err := m.ReadForPack(p.relPath)
		if err != nil {
			m.log.WarnContext(ctx, "could not read sidecar", "pack", p.relPath, "error", err)
			continue
		}
		if !ok {
			continue
		}
		did, err := m.ImportForPack(ctx, p.id, p.relPath, sc)
		if err != nil {
			m.log.WarnContext(ctx, "could not import sidecar", "pack", p.relPath, "error", err)
			continue
		}
		if did {
			imported++
		}
	}
	return imported, nil
}

// SyncAll writes a sidecar for every pack from the current database state. Used
// by `ambar sidecar sync` to materialise sidecars for a library indexed before
// sidecars existed.
func (m *Manager) SyncAll(ctx context.Context) (int, error) {
	packs, err := m.allPacks(ctx)
	if err != nil {
		return 0, err
	}
	for _, p := range packs {
		if err := m.Write(ctx, p.id, p.relPath); err != nil {
			return 0, err
		}
	}
	return len(packs), nil
}

type packRow struct {
	id      int64
	relPath string
}

func (m *Manager) allPacks(ctx context.Context) ([]packRow, error) {
	rows, err := m.db.Reader.QueryContext(ctx, `SELECT id, library_rel_path FROM packs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []packRow
	for rows.Next() {
		var p packRow
		if err := rows.Scan(&p.id, &p.relPath); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

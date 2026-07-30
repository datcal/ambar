// Package provenance reads and writes the pack provenance and licensing of §9:
// where a pack came from, under what licence, at what cost. The licence table is
// an application lookup; which licence a pack carries is user metadata on the
// pack (and, from M4-4f, in its sidecar).
package provenance

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/datcal/ambar/internal/db"
)

// Provenance states (§9). A pack is usable in either state; the state only drives
// the capture form and the licence-risk view.
const (
	StateNeedsProvenance = "needs_provenance"
	StateComplete        = "complete"
)

// License is one row of the licence lookup.
type License struct {
	ID                  int64
	SPDXID              string
	Name                string
	CommercialOK        bool
	AttributionRequired bool
	ShareAlike          bool
	URL                 string
}

// Provenance is a pack's §9 record.
type Provenance struct {
	PackID              int64
	SourceURL           string
	SourceSite          string
	SourceAuthor        string
	SourceAuthorURL     string
	LicenseID           *int64
	LicenseNote         string
	AttributionRequired bool
	AttributionText     string
	AcquiredAt          *time.Time
	PricePaidCents      *int64
	Currency            string
	OrderRef            string
	OriginalArchiveName string
	OriginalArchiveSHA  string
	OriginalArchiveSize *int64
	State               string
	Notes               string
}

// HasLicense reports whether the pack carries the given licence id, for the
// capture form's licence dropdown.
func (p Provenance) HasLicense(id int64) bool {
	return p.LicenseID != nil && *p.LicenseID == id
}

// PriceMajor renders the paid price in major currency units ("15.00"), or "" when
// no price is recorded.
func (p Provenance) PriceMajor() string {
	if p.PricePaidCents == nil {
		return ""
	}
	return fmt.Sprintf("%.2f", float64(*p.PricePaidCents)/100)
}

// ArchiveSizeBytes is the original archive size for display, or 0 when unknown.
func (p Provenance) ArchiveSizeBytes() int64 {
	if p.OriginalArchiveSize == nil {
		return 0
	}
	return *p.OriginalArchiveSize
}

// Store reads and writes provenance.
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

// Licenses lists the licence lookup, ordered so the permissive ones a game
// library reaches for most sit first.
func (s *Store) Licenses(ctx context.Context) ([]License, error) {
	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT id, spdx_id, name, commercial_ok, attribution_required, share_alike, url
		FROM licenses
		ORDER BY commercial_ok DESC, attribution_required, spdx_id`)
	if err != nil {
		return nil, fmt.Errorf("list licenses: %w", err)
	}
	defer rows.Close()

	var out []License
	for rows.Next() {
		var l License
		if err := rows.Scan(&l.ID, &l.SPDXID, &l.Name,
			&l.CommercialOK, &l.AttributionRequired, &l.ShareAlike, &l.URL); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LicenseBySPDX resolves an SPDX id to a licence, for ingest sniffing and the UI.
func (s *Store) LicenseBySPDX(ctx context.Context, spdx string) (License, bool, error) {
	var l License
	err := s.db.Reader.QueryRowContext(ctx, `
		SELECT id, spdx_id, name, commercial_ok, attribution_required, share_alike, url
		FROM licenses WHERE spdx_id = ?`, spdx).Scan(
		&l.ID, &l.SPDXID, &l.Name, &l.CommercialOK, &l.AttributionRequired, &l.ShareAlike, &l.URL)
	if err == sql.ErrNoRows {
		return License{}, false, nil
	}
	if err != nil {
		return License{}, false, err
	}
	return l, true, nil
}

// Get returns a pack's provenance.
func (s *Store) Get(ctx context.Context, packID int64) (Provenance, error) {
	var (
		p           Provenance
		licenseID   sql.NullInt64
		acquiredAt  sql.NullInt64
		price       sql.NullInt64
		archiveSize sql.NullInt64
	)
	err := s.db.Reader.QueryRowContext(ctx, `
		SELECT source_url, source_site, source_author, source_author_url,
		       license_id, license_note, attribution_required, attribution_text,
		       acquired_at, price_paid_cents, currency, order_ref,
		       original_archive_name, original_archive_sha256, original_archive_size,
		       provenance_state, notes
		FROM packs WHERE id = ?`, packID).Scan(
		&p.SourceURL, &p.SourceSite, &p.SourceAuthor, &p.SourceAuthorURL,
		&licenseID, &p.LicenseNote, &p.AttributionRequired, &p.AttributionText,
		&acquiredAt, &price, &p.Currency, &p.OrderRef,
		&p.OriginalArchiveName, &p.OriginalArchiveSHA, &archiveSize,
		&p.State, &p.Notes)
	if err == sql.ErrNoRows {
		return Provenance{}, fmt.Errorf("pack %d not found", packID)
	}
	if err != nil {
		return Provenance{}, fmt.Errorf("get provenance for pack %d: %w", packID, err)
	}
	p.PackID = packID
	if licenseID.Valid {
		p.LicenseID = &licenseID.Int64
	}
	if acquiredAt.Valid {
		t := time.Unix(acquiredAt.Int64, 0)
		p.AcquiredAt = &t
	}
	if price.Valid {
		p.PricePaidCents = &price.Int64
	}
	if archiveSize.Valid {
		p.OriginalArchiveSize = &archiveSize.Int64
	}
	return p, nil
}

// Update writes every provenance field of a pack in one statement. It is a whole
// replace, not a patch: the capture form (§9, 4g) always submits the full record,
// which keeps the write unambiguous and the sidecar (4f) a faithful mirror.
func (s *Store) Update(ctx context.Context, p Provenance) error {
	if p.State != StateComplete && p.State != StateNeedsProvenance {
		return fmt.Errorf("update provenance: invalid state %q", p.State)
	}
	res, err := s.db.Writer.ExecContext(ctx, `
		UPDATE packs SET
		    source_url = ?, source_site = ?, source_author = ?, source_author_url = ?,
		    license_id = ?, license_note = ?, attribution_required = ?, attribution_text = ?,
		    acquired_at = ?, price_paid_cents = ?, currency = ?, order_ref = ?,
		    original_archive_name = ?, original_archive_sha256 = ?, original_archive_size = ?,
		    provenance_state = ?, notes = ?, updated_at = ?
		WHERE id = ?`,
		p.SourceURL, p.SourceSite, p.SourceAuthor, p.SourceAuthorURL,
		nullInt64(p.LicenseID), p.LicenseNote, boolToInt(p.AttributionRequired), p.AttributionText,
		unixPtr(p.AcquiredAt), nullInt64(p.PricePaidCents), p.Currency, p.OrderRef,
		p.OriginalArchiveName, p.OriginalArchiveSHA, nullInt64(p.OriginalArchiveSize),
		p.State, p.Notes, s.now().Unix(),
		p.PackID)
	if err != nil {
		return fmt.Errorf("update provenance for pack %d: %w", p.PackID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("update provenance: pack %d not found", p.PackID)
	}
	return nil
}

func nullInt64(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

func unixPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

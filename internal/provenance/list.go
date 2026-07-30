package provenance

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"strings"
)

// Filter selects which packs a listing returns.
type Filter string

const (
	// FilterNeeds is the §9 capture backlog: packs still in needs_provenance.
	FilterNeeds Filter = "needs"
	// FilterRisk is the §9 licence-risk view: no licence, non-commercial, or
	// attribution required with no attribution text.
	FilterRisk Filter = "risk"
	// FilterAll is every pack.
	FilterAll Filter = "all"
)

// PackSummary is one row of a provenance listing, joined with its licence.
type PackSummary struct {
	PackID              int64
	Name                string
	RelPath             string
	SourceAuthor        string
	SourceURL           string
	LicenseSPDX         string // "" when unlicensed
	CommercialOK        bool
	State               string
	AttributionRequired bool
	AttributionText     string
}

// Unlicensed reports whether the pack has no licence recorded.
func (s PackSummary) Unlicensed() bool { return s.LicenseSPDX == "" }

// AttentionReason explains, for the risk view, why a pack is flagged — empty when
// it is fine.
func (s PackSummary) AttentionReason() string {
	switch {
	case s.LicenseSPDX == "":
		return "no licence"
	case !s.CommercialOK:
		return "not cleared for commercial use"
	case s.AttributionRequired && strings.TrimSpace(s.AttributionText) == "":
		return "attribution required, none written"
	}
	return ""
}

// Summaries lists packs matching a filter, name-ordered.
func (s *Store) Summaries(ctx context.Context, filter Filter) ([]PackSummary, error) {
	where := "1 = 1"
	switch filter {
	case FilterNeeds:
		where = "p.provenance_state = 'needs_provenance'"
	case FilterRisk:
		where = "(p.license_id IS NULL OR l.commercial_ok = 0 OR " +
			"(p.attribution_required = 1 AND p.attribution_text = ''))"
	}

	rows, err := s.db.Reader.QueryContext(ctx, `
		SELECT p.id, p.name, p.library_rel_path, p.source_author, p.source_url,
		       coalesce(l.spdx_id, ''), coalesce(l.commercial_ok, 0),
		       p.provenance_state, p.attribution_required, p.attribution_text
		FROM packs p
		LEFT JOIN licenses l ON l.id = p.license_id
		WHERE `+where+`
		ORDER BY p.name, p.id`)
	if err != nil {
		return nil, fmt.Errorf("provenance summaries: %w", err)
	}
	defer rows.Close()

	var out []PackSummary
	for rows.Next() {
		var s PackSummary
		if err := rows.Scan(&s.PackID, &s.Name, &s.RelPath, &s.SourceAuthor, &s.SourceURL,
			&s.LicenseSPDX, &s.CommercialOK, &s.State, &s.AttributionRequired, &s.AttributionText); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Sniffed is what SniffURL can infer from a source URL.
type Sniffed struct {
	Site   string
	Author string
}

// SniffURL infers the site and, where it can, the author from a source URL, to
// pre-fill the capture form (§9: "Pre-fill by sniffing the URL — an itch.io URL
// implies the site and usually the author from the subdomain").
func SniffURL(raw string) Sniffed {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Sniffed{}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return Sniffed{}
	}
	host := strings.ToLower(strings.TrimPrefix(u.Host, "www."))

	switch {
	case strings.HasSuffix(host, ".itch.io"):
		return Sniffed{Site: "itch.io", Author: strings.TrimSuffix(host, ".itch.io")}
	case host == "itch.io":
		// itch.io/…/<author> style is not reliably parseable; leave the author blank.
		return Sniffed{Site: "itch.io"}
	case strings.Contains(host, "opengameart.org"):
		return Sniffed{Site: "OpenGameArt"}
	case strings.Contains(host, "polyhaven.com"):
		return Sniffed{Site: "Poly Haven"}
	case strings.Contains(host, "poly.pizza"):
		return Sniffed{Site: "Poly Pizza"}
	case strings.Contains(host, "kenney.nl"):
		return Sniffed{Site: "Kenney", Author: "Kenney"}
	case strings.Contains(host, "freesound.org"):
		return Sniffed{Site: "Freesound"}
	default:
		return Sniffed{Site: host}
	}
}

// LicenseByID resolves a licence id to its row, for display.
func (s *Store) LicenseByID(ctx context.Context, id int64) (License, bool, error) {
	var l License
	err := s.db.Reader.QueryRowContext(ctx, `
		SELECT id, spdx_id, name, commercial_ok, attribution_required, share_alike, url
		FROM licenses WHERE id = ?`, id).Scan(
		&l.ID, &l.SPDXID, &l.Name, &l.CommercialOK, &l.AttributionRequired, &l.ShareAlike, &l.URL)
	if err == sql.ErrNoRows {
		return License{}, false, nil
	}
	if err != nil {
		return License{}, false, err
	}
	return l, true, nil
}

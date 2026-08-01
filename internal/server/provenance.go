package server

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/provenance"
)

// handlePackProvenanceForm renders the capture form for one pack, pre-filling
// what it can sniff from the source URL (§9).
func (s *Server) handlePackProvenanceForm(w http.ResponseWriter, r *http.Request) {
	packID, name, rel, ok := s.lookupPack(w, r)
	if !ok {
		return
	}
	prov, err := s.prov.Get(r.Context(), packID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "get provenance failed", "pack", packID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	licenses, err := s.prov.Licenses(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Prov = &prov
	data.Licenses = licenses
	data.ProvPackID = packID
	data.ProvPackName = name
	data.ProvPackRel = rel
	data.ProvView = r.URL.Query().Get("view")
	// Suggestions only where the field is still empty, so an edit never fights the
	// user's own entry.
	if prov.SourceURL != "" && (prov.SourceSite == "" || prov.SourceAuthor == "") {
		data.Sniff = provenance.SniffURL(prov.SourceURL)
	}
	s.render(w, r, "pack_provenance.html", http.StatusOK, data)
}

// handlePackProvenanceSave persists the capture form and writes the sidecar (§3).
func (s *Server) handlePackProvenanceSave(w http.ResponseWriter, r *http.Request) {
	packID, _, rel, ok := s.lookupPack(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p := provenance.Provenance{
		PackID:              packID,
		SourceURL:           strings.TrimSpace(r.PostFormValue("source_url")),
		SourceSite:          strings.TrimSpace(r.PostFormValue("source_site")),
		SourceAuthor:        strings.TrimSpace(r.PostFormValue("source_author")),
		SourceAuthorURL:     strings.TrimSpace(r.PostFormValue("source_author_url")),
		LicenseNote:         strings.TrimSpace(r.PostFormValue("license_note")),
		AttributionRequired: r.PostFormValue("attribution_required") == "on",
		AttributionText:     strings.TrimSpace(r.PostFormValue("attribution_text")),
		Currency:            strings.TrimSpace(r.PostFormValue("currency")),
		OrderRef:            strings.TrimSpace(r.PostFormValue("order_ref")),
		Notes:               strings.TrimSpace(r.PostFormValue("notes")),
		State:               provenance.StateNeedsProvenance,
	}
	if r.PostFormValue("complete") == "on" {
		p.State = provenance.StateComplete
	}
	if spdx := r.PostFormValue("license"); spdx != "" {
		if l, found, err := s.prov.LicenseBySPDX(r.Context(), spdx); err == nil && found {
			p.LicenseID = &l.ID
		}
	}
	if cents, ok := parsePriceCents(r.PostFormValue("price")); ok {
		p.PricePaidCents = &cents
	}
	if acq, ok := parseDate(r.PostFormValue("acquired")); ok {
		p.AcquiredAt = &acq
	}

	back := "/provenance?view=" + r.PostFormValue("view")
	if err := s.prov.Update(r.Context(), p); err != nil {
		s.log.ErrorContext(r.Context(), "update provenance failed", "pack", packID, "error", err)
		s.redirectWithMessage(w, r, back, "Could not save — check the fields and try again.")
		return
	}
	// §3: persist the human-authored metadata to the sidecar. A write failure is
	// logged but does not lose the database update.
	if err := s.sidecars.Write(r.Context(), packID, rel); err != nil {
		s.log.WarnContext(r.Context(), "sidecar write failed", "pack", packID, "error", err)
	}
	s.redirectWithMessage(w, r, back, "Saved provenance.")
}

// The /provenance backlog list and its bulk form lived here until M16.
//
// They assumed every arrival is a downloaded pack you sit down and process. In practice a single
// PNG turns up on its own, and then the only place anyone asks "where did this come from, may we
// ship it" is the asset page they are already looking at — which is where those two fields are
// now (handleAssetProvenanceSave). The backlog itself is a search: `-has:provenance` lists the
// affected assets in the grid, each one fixable in place, and the sidebar links to it with a
// count. The full capture form stays, one click from the asset page.

func (s *Server) lookupPack(w http.ResponseWriter, r *http.Request) (id int64, name, rel string, ok bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, "", "", false
	}
	_, name, rel, ok = s.packByID(r.Context(), id)
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return 0, "", "", false
	}
	return id, name, rel, true
}

// packByID reads a pack's name and library-relative path.
func (s *Server) packByID(ctx context.Context, id int64) (int64, string, string, bool) {
	var name, rel string
	err := s.db.Reader.QueryRowContext(ctx, `SELECT name, library_rel_path FROM packs WHERE id = ?`, id).
		Scan(&name, &rel)
	if err != nil {
		return 0, "", "", false
	}
	return id, name, rel, true
}

// parsePriceCents turns a decimal major-unit string ("15", "15.00") into integer
// cents. An empty string is "not provided".
func parsePriceCents(raw string) (int64, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	f, err := strconv.ParseFloat(raw, 64)
	if err != nil || f < 0 {
		return 0, false
	}
	return int64(f*100 + 0.5), true
}

// parseDate parses a yyyy-mm-dd form value into a time, reporting presence.
func parseDate(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.ParseInLocation("2006-01-02", raw, time.UTC)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// --- provenance from the asset page (M16) ------------------------------------

// handleAssetProvenanceSave records a licence and a source URL for the pack an asset
// belongs to, from the asset's own page.
//
// Why this exists: the /provenance backlog view assumed every arrival is a downloaded pack
// you sit down and process. In practice a single PNG turns up on its own, and then the only
// place the question "where did this come from and may we ship it" is ever asked is the page
// you are already looking at. So the two fields that actually get filled in — licence and
// link — are here, and the full capture form stays for the rest.
//
// A *partial* update on purpose. handlePackProvenanceSave writes the whole record from its
// form, so posting two fields through it would silently blank the author, the price, the
// order reference and the notes. This reads what is there, patches those two, and writes it
// back.
func (s *Server) handleAssetProvenanceSave(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	p, err := s.prov.Get(r.Context(), asset.PackID)
	if err != nil {
		s.log.ErrorContext(r.Context(), "loading provenance failed", "pack_id", asset.PackID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	p.SourceURL = strings.TrimSpace(r.PostFormValue("source_url"))

	// An empty selection means "not recorded yet" rather than "no licence", so it clears
	// the field instead of failing.
	p.LicenseID = nil
	if spdx := strings.TrimSpace(r.PostFormValue("license")); spdx != "" {
		l, found, err := s.prov.LicenseBySPDX(r.Context(), spdx)
		if err != nil {
			s.log.ErrorContext(r.Context(), "resolving licence failed", "spdx", spdx, "error", err)
		} else if found {
			p.LicenseID = &l.ID
		}
	}

	// The pack stops being a backlog item once both halves are known. Anything less stays
	// on the list — §9's point is that the gap is visible, not that it is dismissible.
	if p.LicenseID != nil && p.SourceURL != "" {
		p.State = provenance.StateComplete
	} else {
		p.State = provenance.StateNeedsProvenance
	}

	if err := s.prov.Update(r.Context(), p); err != nil {
		s.log.ErrorContext(r.Context(), "saving provenance failed", "pack_id", asset.PackID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// The sidebar counts packs that still need provenance.
	s.nav.invalidate()

	msg := "Saved. This applies to the whole pack."
	if p.State == provenance.StateNeedsProvenance {
		msg = "Saved. Still incomplete: a licence and a source link mark a pack done."
	}
	http.Redirect(w, r, fmt.Sprintf("/assets/%d?msg=%s", asset.ID, url.QueryEscape(msg)), http.StatusSeeOther)
}

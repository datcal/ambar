package server

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/provenance"
)

// handleProvenanceList renders the §9 capture backlog and licence-risk views.
func (s *Server) handleProvenanceList(w http.ResponseWriter, r *http.Request) {
	view := r.URL.Query().Get("view")
	filter := provenance.FilterNeeds
	switch view {
	case "risk":
		filter = provenance.FilterRisk
	case "all":
		filter = provenance.FilterAll
	default:
		view = "needs"
	}

	summaries, err := s.prov.Summaries(r.Context(), filter)
	if err != nil {
		s.log.ErrorContext(r.Context(), "provenance list failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	licenses, err := s.prov.Licenses(r.Context())
	if err != nil {
		s.log.ErrorContext(r.Context(), "licenses failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.PackSummaries = summaries
	data.ProvView = view
	data.Licenses = licenses
	data.Flash = r.URL.Query().Get("msg")
	s.render(w, r, "provenance.html", http.StatusOK, data)
}

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

// handleProvenanceBulk sets one licence across selected packs (§9 multi-selection).
func (s *Server) handleProvenanceBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	view := r.PostFormValue("view")
	back := "/provenance?view=" + view

	spdx := r.PostFormValue("license")
	if spdx == "" {
		s.redirectWithMessage(w, r, back, "Choose a licence to apply.")
		return
	}
	license, found, err := s.prov.LicenseBySPDX(r.Context(), spdx)
	if err != nil || !found {
		s.redirectWithMessage(w, r, back, "That licence is not recognised.")
		return
	}

	applied := 0
	for _, raw := range r.PostForm["id"] {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || id <= 0 {
			continue
		}
		prov, err := s.prov.Get(r.Context(), id)
		if err != nil {
			continue
		}
		prov.PackID = id
		prov.LicenseID = &license.ID
		prov.AttributionRequired = license.AttributionRequired
		prov.State = provenance.StateComplete
		if err := s.prov.Update(r.Context(), prov); err != nil {
			s.log.WarnContext(r.Context(), "bulk provenance update failed", "pack", id, "error", err)
			continue
		}
		if _, _, rel, ok := s.packByID(r.Context(), id); ok {
			if err := s.sidecars.Write(r.Context(), id, rel); err != nil {
				s.log.WarnContext(r.Context(), "sidecar write failed", "pack", id, "error", err)
			}
		}
		applied++
	}
	s.redirectWithMessage(w, r, back, "Set "+license.SPDXID+" on "+strconv.Itoa(applied)+" pack(s).")
}

// lookupPack parses the {id} path value and loads the pack's identity, writing
// the error response if it fails.
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

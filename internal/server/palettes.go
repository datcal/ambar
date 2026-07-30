package server

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/datcal/ambar/internal/index"
)

// handlePalettes is §7's pack-level palette consistency view: "does this tileset sit
// next to that character set".
//
// The page has two states and one GET, because the comparison is a bookmarkable
// question rather than an action: with no parameters it lists every pack's palette
// strip; with ?a=&b= it also compares those two. Nothing here writes.
func (s *Server) handlePalettes(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(r)

	// Ten colours is what fits on a strip without the row wrapping on a laptop; the
	// comparison itself looks wider (see index.ComparePacks).
	palettes, err := s.index.PackPalettes(r.Context(), 10)
	if err != nil {
		s.log.ErrorContext(r.Context(), "loading pack palettes failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data.PackPalettes = palettes

	packA, aOK := packIDParam(r, "a")
	packB, bOK := packIDParam(r, "b")
	data.PaletteA, data.PaletteB = packA, packB

	if aOK && bOK {
		if packA == packB {
			data.Flash = "Pick two different packs to compare."
		} else {
			// The tolerance is a query parameter so the view can be argued with: two
			// packs that look wrong together at ±24 may be fine at ±40.
			tolerance := 0
			if raw := r.URL.Query().Get("tolerance"); raw != "" {
				if v, err := strconv.Atoi(raw); err == nil && v >= 0 && v <= 255 {
					tolerance = v
				}
			}
			comparison, err := s.index.ComparePacks(r.Context(), packA, packB, tolerance)
			switch {
			case errors.Is(err, index.ErrPackNotFound):
				// A pack id from a URL that names nothing is a bad link, not a server
				// fault; the page still lists everything that does exist.
				data.Flash = "One of those packs does not exist any more."
			case err != nil:
				s.log.ErrorContext(r.Context(), "comparing pack palettes failed", "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			default:
				data.PaletteComparison = &comparison
			}
		}
	} else if aOK != bOK {
		data.Flash = "Choose a pack on both sides to compare."
	}

	s.render(w, r, "palettes.html", http.StatusOK, data)
}

// packIDParam reads a positive pack id from the query string.
func packIDParam(r *http.Request, name string) (int64, bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

// paletteComparison is the type the template ranges over; named here so pageData
// stays readable.
type paletteComparison = index.PackComparison

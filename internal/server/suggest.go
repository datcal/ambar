package server

import (
	"net/http"
	"strings"
)

// handleSearchSuggest returns the search box's completion list as an HTML fragment.
//
// HTML rather than JSON because the island that renders it is twenty lines of DOM handling
// either way, and a fragment keeps the markup — grouping, escaping, the count column — in the
// template with everything else. §2's "no SPA" applies to search as much as to the grid.
//
// GET with no side effects, so it needs no CSRF token; it is behind auth like every other
// route, because the suggestions are the library's contents.
func (s *Server) handleSearchSuggest(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	// An empty box has nothing to complete. The island shows recent searches there instead,
	// which it holds itself — the server has no business remembering what one browser typed.
	if query == "" {
		s.renderPartial(w, r, "assets.html", "search-suggestions", http.StatusOK, s.newPageData(r))
		return
	}

	suggestions, err := s.index.Suggest(r.Context(), query)
	if err != nil {
		// A failed suggestion is not a failed search: log it and return an empty list so
		// the box keeps working.
		s.log.ErrorContext(r.Context(), "suggest failed", "error", err)
		s.renderPartial(w, r, "assets.html", "search-suggestions", http.StatusOK, s.newPageData(r))
		return
	}

	data := s.newPageData(r)
	data.Suggestions = suggestions
	s.renderPartial(w, r, "assets.html", "search-suggestions", http.StatusOK, data)
}

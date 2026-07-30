package server

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/savedsearch"
	"github.com/datcal/ambar/internal/tags"
)

// handleBulkTag applies one tag to many assets (§7 bulk tagging): either the
// checked tiles (scope=selected, repeated `id`) or every asset matching the
// current search (scope=all, carried in q/kind/missing). It redirects back to
// the search with a short outcome message rather than returning a fragment, so a
// browser refresh does not re-apply the tag.
func (s *Server) handleBulkTag(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	tag := strings.TrimSpace(r.PostFormValue("tag"))
	q := strings.TrimSpace(r.PostFormValue("q"))
	kind := r.PostFormValue("kind")
	missing := r.PostFormValue("missing") == "1"
	scope := r.PostFormValue("scope")
	back := backURL(q, kind, missing)

	if tag == "" {
		s.redirectWithMessage(w, r, back, "Enter a tag to apply.")
		return
	}

	var ids []int64
	if scope == "all" {
		matched, err := s.index.MatchingAssetIDs(r.Context(),
			index.ListOptions{Query: q, Kind: kind, IncludeMissing: missing})
		if err != nil {
			s.log.ErrorContext(r.Context(), "bulk tag: matching ids failed", "error", err)
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		ids = matched
	} else {
		for _, raw := range r.PostForm["id"] {
			if id, err := strconv.ParseInt(raw, 10, 64); err == nil && id > 0 {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		s.redirectWithMessage(w, r, back, "Nothing selected to tag.")
		return
	}

	var createdBy *int64
	if u, ok := auth.UserFromContext(r.Context()); ok {
		createdBy = &u.ID
	}
	n, err := s.tags.TagAssets(r.Context(), ids, tag, tags.SourceManual, createdBy)
	if err != nil {
		if errors.Is(err, tags.ErrInvalidTag) {
			s.redirectWithMessage(w, r, back, "Not a valid tag. Use namespace:name, e.g. theme:sci-fi.")
			return
		}
		s.log.ErrorContext(r.Context(), "bulk tag failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.redirectWithMessage(w, r, back, fmt.Sprintf("Tagged %d asset%s with %s.", n, plural(n), tag))
}

// handleSaveSearch stores the current query under a name (§7 saved searches).
func (s *Server) handleSaveSearch(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	query := strings.TrimSpace(r.PostFormValue("q"))
	back := backURL(query, r.PostFormValue("kind"), r.PostFormValue("missing") == "1")

	if _, err := s.saved.Save(r.Context(), name, query); err != nil {
		if errors.Is(err, savedsearch.ErrEmpty) {
			s.redirectWithMessage(w, r, back, "A saved search needs both a name and a query.")
			return
		}
		s.log.ErrorContext(r.Context(), "save search failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.redirectWithMessage(w, r, back, fmt.Sprintf("Saved search %q.", name))
}

// handleDeleteSearch removes a saved search.
func (s *Server) handleDeleteSearch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.saved.Delete(r.Context(), id); err != nil {
		s.log.ErrorContext(r.Context(), "delete saved search failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.redirectWithMessage(w, r, "/assets", "Deleted saved search.")
}

// backURL rebuilds the assets URL for a given search so a redirect lands the user
// back where they were.
func backURL(query, kind string, missing bool) string {
	v := url.Values{}
	if query != "" {
		v.Set("q", query)
	}
	if kind != "" {
		v.Set("kind", kind)
	}
	if missing {
		v.Set("missing", "1")
	}
	if len(v) == 0 {
		return "/assets"
	}
	return "/assets?" + v.Encode()
}

// redirectWithMessage adds a one-shot message to the target URL and navigates to
// it. A query-param flash is enough: it survives the redirect, shows once, and
// needs no session storage. htmx callers (the multipart upload) get an
// HX-Redirect header, since htmx would otherwise swap the redirected page's body
// into the current DOM instead of navigating.
func (s *Server) redirectWithMessage(w http.ResponseWriter, r *http.Request, target, msg string) {
	sep := "?"
	if strings.Contains(target, "?") {
		sep = "&"
	}
	dest := target + sep + "msg=" + url.QueryEscape(msg)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

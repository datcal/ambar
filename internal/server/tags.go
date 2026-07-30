package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/tags"
)

// handleAssetTagAdd attaches a manual tag to an asset and re-renders the tag
// panel. The whole exchange is htmx: the response is the tag-panel fragment,
// swapped in place, so the page never reloads (§7 bulk/interactive tagging).
func (s *Server) handleAssetTagAdd(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	data := s.newPageData(r)
	data.Asset = &asset

	raw := strings.TrimSpace(r.PostFormValue("tag"))
	status := http.StatusOK
	if raw != "" {
		var createdBy *int64
		if u, ok := auth.UserFromContext(r.Context()); ok {
			createdBy = &u.ID
		}
		if _, err := s.tags.TagAsset(r.Context(), asset.ID, raw, tags.SourceManual, createdBy); err != nil {
			if errors.Is(err, tags.ErrInvalidTag) {
				data.TagError = "Not a valid tag. Use namespace:name, for example theme:sci-fi or license:cc0."
				status = http.StatusUnprocessableEntity
			} else {
				s.log.ErrorContext(r.Context(), "tagging asset failed", "asset_id", asset.ID, "error", err)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
		}
	}

	s.loadAssetTags(r, &data, asset.ID)
	s.renderPartial(w, r, "asset.html", "asset-tags", status, data)
}

// handleAssetTagRemove detaches a direct tag from an asset. Inherited pack tags
// have no remove control here; they are removed at the pack, not the asset.
func (s *Server) handleAssetTagRemove(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	tagID, err := strconv.ParseInt(r.PostFormValue("tag_id"), 10, 64)
	if err != nil || tagID <= 0 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := s.tags.UntagAsset(r.Context(), asset.ID, tagID); err != nil {
		s.log.ErrorContext(r.Context(), "untagging asset failed", "asset_id", asset.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Asset = &asset
	s.loadAssetTags(r, &data, asset.ID)
	s.renderPartial(w, r, "asset.html", "asset-tags", http.StatusOK, data)
}

// handleTagSuggest returns <option> elements for the tag input's datalist,
// matched by prefix against canonical tags and aliases (§7 autocomplete).
func (s *Server) handleTagSuggest(w http.ResponseWriter, r *http.Request) {
	// The datalist input posts its own field name ("tag"); a plain caller may use
	// "q". Accept either.
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		q = strings.TrimSpace(r.URL.Query().Get("tag"))
	}
	suggestions, err := s.tags.Suggest(r.Context(), q, 20)
	if err != nil {
		s.log.ErrorContext(r.Context(), "tag suggest failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := s.newPageData(r)
	data.Suggest = suggestions
	s.renderPartial(w, r, "asset.html", "tag-options", http.StatusOK, data)
}

// loadAssetTags fills data.AssetTags, logging but not failing the request on error.
func (s *Server) loadAssetTags(r *http.Request, data *pageData, assetID int64) {
	if ats, err := s.tags.AssetTags(r.Context(), assetID); err != nil {
		s.log.ErrorContext(r.Context(), "loading asset tags failed", "asset_id", assetID, "error", err)
	} else {
		data.AssetTags = ats
	}
}

package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/index"
)

// writeJSON renders a value as JSON with a status.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.log.DebugContext(r.Context(), "api encode failed", "error", err)
	}
}

func (s *Server) apiError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// assetJSON is the wire shape of an asset (§10). Kind-specific fields are omitted
// when empty, so an audio asset does not carry null triangle counts.
type assetJSON struct {
	ID       int64  `json:"id"`
	Filename string `json:"filename"`
	Ext      string `json:"ext"`
	Kind     string `json:"kind"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
	Width    int    `json:"width,omitempty"`
	Height   int    `json:"height,omitempty"`

	Pack struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Slug string `json:"slug"`
		// No source_url here on purpose: index.Asset does not carry the pack's provenance, and
		// adding a column to the *list* query to supply it would undo the trimming M16 did to
		// that query for a field the client does not need. Credits are assembled server-side —
		// only the server knows the licences — and the plugin's manifest keeps the pack name,
		// which is enough to reconcile from.
	} `json:"pack"`
	Path string `json:"path"`

	DeriveState string `json:"derive_state"`
	IsPixelArt  bool   `json:"is_pixel_art,omitempty"`
	HasAlpha    bool   `json:"has_alpha,omitempty"`

	// Group identity, set only when the search collapsed format variants (§5.1). A
	// client that asked for one row per file gets neither, which is the honest answer:
	// in that mode the row it is holding is a file, not a logical asset.
	GroupID      int64 `json:"group_id,omitempty"`
	VariantCount int   `json:"variant_count,omitempty"`

	// Kind-specific.
	DurationMS int     `json:"duration_ms,omitempty"`
	SampleRate int     `json:"sample_rate,omitempty"`
	TriCount   int     `json:"tri_count,omitempty"`
	VertCount  int     `json:"vert_count,omitempty"`
	FrameCount int     `json:"frame_count,omitempty"`
	FrameCols  int     `json:"frame_cols,omitempty"`
	FrameRows  int     `json:"frame_rows,omitempty"`
	FPS        float64 `json:"fps,omitempty"`

	Links map[string]string `json:"links"`
}

func toAssetJSON(a index.Asset) assetJSON {
	j := assetJSON{
		ID: a.ID, Filename: a.Filename, Ext: a.Ext, Kind: a.Kind, Size: a.Size, SHA256: a.SHA256,
		Width: a.Width, Height: a.Height, Path: a.LibraryPath(), DeriveState: a.DeriveState,
		IsPixelArt: a.IsPixelArt, HasAlpha: a.HasAlpha,
		DurationMS: a.DurationMS, SampleRate: a.SampleRate,
		TriCount: a.TriCount, VertCount: a.VertCount,
		FrameCount: a.FrameCount, FrameCols: a.FrameCols, FrameRows: a.FrameRows, FPS: a.FPS,
	}
	j.Pack.ID, j.Pack.Name, j.Pack.Slug = a.PackID, a.PackName, a.PackSlug

	base := "/api/v1/assets/" + strconv.FormatInt(a.ID, 10)
	j.Links = map[string]string{"file": base + "/file", "thumb": base + "/thumb"}
	if a.IsModel() {
		j.Links["preview_glb"] = base + "/preview.glb"
	}
	if a.IsAudio() {
		j.Links["peaks"] = base + "/peaks.json"
	}
	return j
}

// handleAPISearch is GET /api/v1/search (§10).
//
// Two modes, and which one you get is decided by the request rather than by a default
// that changed under existing callers:
//
//   - `q`, `kind`, `tags`, `limit`, `cursor` alone behave exactly as they always have —
//     one row per *file*, filename order, keyset cursor.
//   - `group=1`, `page=` or `sort=` switch to the grid's query: one row per logical
//     asset (§5.1, invariant 7), any of the nine browse orders, numbered pages.
//
// M18 added the second mode because the Godot plugin had the first one's limits and
// nothing else: no order but filename, no way back to a page, and the same sprite listed
// three times because it ships as PNG, PSD and ASEPRITE. Any of the three parameters
// implies the whole mode — silently ignoring `page=3` and answering with page 1 is worse
// than either supporting it or refusing it.
func (s *Server) handleAPISearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := index.ListOptions{
		Query:  strings.TrimSpace(q.Get("q")),
		Kind:   q.Get("kind"),
		Cursor: q.Get("cursor"),
	}
	// tags= is folded into the query language: each tag becomes a term.
	for _, tag := range q["tags"] {
		if t := strings.TrimSpace(tag); t != "" {
			opts.Query = strings.TrimSpace(opts.Query + " " + t)
		}
	}
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			opts.Limit = n
		}
	}

	rawSort, rawPage := q.Get("sort"), q.Get("page")
	grouped := rawSort != "" || rawPage != "" ||
		q.Get("group") == "1" || q.Get("group") == "true"
	if !grouped {
		s.searchFlat(w, r, opts)
		return
	}

	opts.Sort = index.ParseSort(rawSort)
	// Page 0 is what ListGroups reads as "the cursor caller", so a grouped search
	// without an explicit page must still ask for page 1 — otherwise it silently pages
	// by cursor in filename order and the sort is ignored.
	opts.Page = 1
	if n, err := strconv.Atoi(rawPage); err == nil && n > 1 {
		opts.Page = n
	}

	page, err := s.index.ListGroups(r.Context(), opts)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api grouped search failed", "error", err)
		s.apiError(w, http.StatusInternalServerError, "search failed")
		return
	}

	assets := make([]assetJSON, 0, len(page.Groups))
	for _, g := range page.Groups {
		j := toAssetJSON(g.Primary)
		j.GroupID, j.VariantCount = g.ID, g.VariantCount
		assets = append(assets, j)
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"assets": assets,
		"total":  page.Total,
		// Empty in this mode, and deliberately still present: a client that walks pages
		// must not also follow a cursor, and an empty string is how it finds that out.
		"next_cursor": "",
		"grouped":     true,
		"sort":        string(page.Sort),
		"page":        page.Page,
		"pages":       page.Pages(),
		"page_size":   page.PageSize,
		// The pager's own numbers, with 0 standing for a gap — computed here because
		// GroupPage already does it for the web grid, and two implementations of "which
		// page links does a 57-page result show" is one too many.
		"page_numbers": page.PageNumbers(),
		"first_shown":  page.FirstShown(),
		"last_shown":   page.LastShown(),
	})
}

// searchFlat is the original per-file, cursor-paged search (§10), unchanged.
func (s *Server) searchFlat(w http.ResponseWriter, r *http.Request, opts index.ListOptions) {
	page, err := s.index.List(r.Context(), opts)
	if err != nil {
		if strings.Contains(err.Error(), "cursor") {
			s.apiError(w, http.StatusBadRequest, "malformed cursor")
			return
		}
		s.log.ErrorContext(r.Context(), "api search failed", "error", err)
		s.apiError(w, http.StatusInternalServerError, "search failed")
		return
	}

	assets := make([]assetJSON, 0, len(page.Assets))
	for _, a := range page.Assets {
		assets = append(assets, toAssetJSON(a))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"assets": assets, "total": page.Total, "next_cursor": page.NextCursor,
	})
}

// sortJSON is one entry of the browse-order list a client renders as a dropdown.
type sortJSON struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// handleAPISorts is GET /api/v1/sorts: the orders `sort=` accepts, with their labels.
//
// Served rather than hardcoded in the plugin because the list has grown twice already
// (M16 added six, M17 added the two triangle-count orders), and a dropdown that has to be
// edited in a second language every time is a dropdown that goes stale.
func (s *Server) handleAPISorts(w http.ResponseWriter, r *http.Request) {
	orders := index.SortOrders()
	out := make([]sortJSON, 0, len(orders))
	for _, o := range orders {
		out = append(out, sortJSON{Value: string(o), Label: o.Label()})
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"sorts": out, "default": string(index.SortDefault),
	})
}

// handleAPIAsset is GET /api/v1/assets/{id} (§10).
func (s *Server) handleAPIAsset(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	asset, err := s.index.Get(r.Context(), id)
	if errors.Is(err, index.ErrNotFound) {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		s.apiError(w, http.StatusInternalServerError, "lookup failed")
		return
	}

	dto := toAssetJSON(asset)
	tagsList, _ := s.tags.AssetTags(r.Context(), id)
	canon := make([]string, 0, len(tagsList))
	for _, t := range tagsList {
		canon = append(canon, t.Tag.Canonical())
	}

	out := map[string]any{"asset": dto, "tags": canon}

	// Everything a detail panel needs, in one response. M18: the alternative was the
	// plugin firing three requests per click — asset, variants, licence — on a NAS, for
	// a panel that opens every time somebody moves the selection.
	//
	// A missing group is not an error: an asset indexed but not yet regrouped still has
	// a usable detail panel, exactly as the web page treats it.
	if group, err := s.index.GroupOf(r.Context(), id); err == nil {
		dto.GroupID, dto.VariantCount = group.ID, group.VariantCount
		out["asset"] = dto
		if variants, err := s.index.Variants(r.Context(), group.ID); err == nil {
			list := make([]assetJSON, 0, len(variants))
			for _, v := range variants {
				list = append(list, toAssetJSON(v))
			}
			out["variants"] = list
		}
	}

	// Provenance is pack-level (§9), and the panel says so rather than implying the
	// licence was recorded for this one file.
	if prov, err := s.prov.Get(r.Context(), asset.PackID); err == nil {
		license := ""
		if prov.LicenseID != nil {
			if l, ok, _ := s.prov.LicenseByID(r.Context(), *prov.LicenseID); ok {
				license = l.SPDXID
			}
		}
		out["provenance"] = map[string]any{
			"source_url": prov.SourceURL, "author": prov.SourceAuthor,
			"license": license, "state": prov.State,
		}
	}

	s.writeJSON(w, r, http.StatusOK, out)
}

// handleAPIPack is GET /api/v1/packs/{id} (§10).
func (s *Server) handleAPIPack(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	var (
		name, slug, kind, relPath string
		assetCount                int
	)
	if err := s.db.Reader.QueryRowContext(r.Context(),
		`SELECT name, slug, kind, library_rel_path FROM packs WHERE id = ?`, id).
		Scan(&name, &slug, &kind, &relPath); err != nil {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	s.db.Reader.QueryRowContext(r.Context(),
		`SELECT count(*) FROM assets WHERE pack_id = ?`, id).Scan(&assetCount)

	out := map[string]any{
		"id": id, "name": name, "slug": slug, "kind": kind, "path": relPath, "asset_count": assetCount,
	}
	if prov, err := s.prov.Get(r.Context(), id); err == nil {
		license := ""
		if prov.LicenseID != nil {
			if l, ok, _ := s.prov.LicenseByID(r.Context(), *prov.LicenseID); ok {
				license = l.SPDXID
			}
		}
		out["provenance"] = map[string]any{
			"source_url": prov.SourceURL, "author": prov.SourceAuthor,
			"license": license, "state": prov.State,
		}
	}
	s.writeJSON(w, r, http.StatusOK, out)
}

// handleAPITags is GET /api/v1/tags?prefix=&namespace= (§10 autocomplete).
func (s *Server) handleAPITags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := strings.TrimSpace(q.Get("prefix"))
	if ns := strings.TrimSpace(q.Get("namespace")); ns != "" && prefix == "" {
		prefix = ns + ":"
	}
	suggestions, err := s.tags.Suggest(r.Context(), prefix, 50)
	if err != nil {
		s.apiError(w, http.StatusInternalServerError, "tag lookup failed")
		return
	}
	if suggestions == nil {
		suggestions = []string{}
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"tags": suggestions})
}

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
	} `json:"pack"`
	Path string `json:"path"`

	DeriveState string `json:"derive_state"`
	IsPixelArt  bool   `json:"is_pixel_art,omitempty"`
	HasAlpha    bool   `json:"has_alpha,omitempty"`

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
	s.writeJSON(w, r, http.StatusOK, map[string]any{"asset": dto, "tags": canon})
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

package server

import (
	"net/http"
	"path/filepath"
	"strconv"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
)

// The model-thumbnail redraw queue (the jobs page's "Model thumbnails" card).
//
// Model thumbnails are drawn by a browser, not by the job queue: the server has no 3D
// renderer (§6). Until now that meant one moment — a tile scrolled into view — so a
// thumbnail drawn badly stayed bad, and every FBX tile drawn before textures were resolved
// by name is white. This lists, a batch at a time, every model whose thumbnail is missing
// or was drawn by an older renderer, and the jobs page works through it in the tab that
// asked: the user's own graphics card, nothing on the server but the uploads.

// renderQueueScan bounds the rows one request reads, so a library of current thumbnails
// is walked in short requests rather than one long one (ARCHITECTURE rule 8).
const renderQueueScan = 500

type renderQueueItem struct {
	Asset  int64  `json:"asset"`
	Src    string `json:"src"`
	Format string `json:"format"`
	MTL    string `json:"mtl,omitempty"`
	// Why it is listed: "missing" or "stale".
	Reason string `json:"reason"`
}

type renderQueuePage struct {
	Items []renderQueueItem `json:"items"`
	// After is where the next request continues; Done says the walk reached the end.
	After int64 `json:"after"`
	Done  bool  `json:"done"`
	// MaxID is the highest model id, for a progress bar over the walk.
	MaxID int64 `json:"max_id"`
}

// handleModelRenderQueue answers GET /models/render-queue?after=<id>&limit=<n>.
func (s *Server) handleModelRenderQueue(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit < 1 || limit > 200 {
		limit = 50
	}

	page := renderQueuePage{After: after, Items: []renderQueueItem{}}
	if err := s.db.Reader.QueryRowContext(r.Context(),
		`SELECT coalesce(max(id), 0) FROM assets WHERE kind = 'model'`).Scan(&page.MaxID); err != nil {
		s.log.ErrorContext(r.Context(), "render queue: max id", "error", err)
		s.apiError(w, http.StatusInternalServerError, "could not read the library")
		return
	}

	rows, err := s.db.Reader.QueryContext(r.Context(), `
		SELECT id, sha256, filename, ext FROM assets
		WHERE kind = 'model' AND missing_since IS NULL AND id > ?
		  AND ext IN ('fbx', 'obj', 'gltf', 'glb')
		ORDER BY id
		LIMIT ?`, after, renderQueueScan)
	if err != nil {
		s.log.ErrorContext(r.Context(), "render queue: list models", "error", err)
		s.apiError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	defer rows.Close()

	scanned := 0
	seen := map[string]bool{}
	for rows.Next() {
		var a index.Asset
		if err := rows.Scan(&a.ID, &a.SHA256, &a.Filename, &a.Ext); err != nil {
			s.apiError(w, http.StatusInternalServerError, "could not read the library")
			return
		}
		scanned++
		page.After = a.ID
		a.Kind = "model"

		// Copies of the same bytes share one thumbnail; drawing it once is enough.
		if seen[a.SHA256] {
			continue
		}
		seen[a.SHA256] = true
		relDir, err := derive.Dir(a.SHA256)
		if err != nil {
			continue
		}
		var reason string
		switch modelThumbStatus(filepath.Join(s.cfg.DataRoot, relDir)) {
		case thumbMissing:
			reason = "missing"
		case thumbStale:
			reason = "stale"
		default:
			continue
		}
		page.Items = append(page.Items, renderQueueItem{
			Asset: a.ID, Src: a.ViewerFile(), Format: a.ViewerFormat(), MTL: a.MTLName(), Reason: reason,
		})
		if len(page.Items) >= limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		s.apiError(w, http.StatusInternalServerError, "could not read the library")
		return
	}
	page.Done = scanned < renderQueueScan && len(page.Items) < limit
	w.Header().Set("Cache-Control", "no-store")
	s.writeJSON(w, r, http.StatusOK, page)
}

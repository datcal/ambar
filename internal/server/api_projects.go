package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/projects"
)

// useRequest is the POST body for recording an asset use (§10).
type useRequest struct {
	AssetID     int64  `json:"asset_id"`
	ResPath     string `json:"res_path"`
	SHA256      string `json:"sha256"`
	ProjectName string `json:"project_name"`
}

// handleAPIRecordUse is POST /api/v1/projects/{project}/uses (§10). The project
// is a UUID and is created on first use. Requires the write scope.
func (s *Server) handleAPIRecordUse(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("project")
	var req useRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.AssetID <= 0 || strings.TrimSpace(req.ResPath) == "" {
		s.apiError(w, http.StatusBadRequest, "asset_id and res_path are required")
		return
	}

	id, err := s.projects.RecordUse(r.Context(), uuid, req.ProjectName, req.AssetID, req.ResPath, req.SHA256)
	if err != nil {
		s.log.ErrorContext(r.Context(), "record use failed", "project", uuid, "error", err)
		s.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.writeJSON(w, r, http.StatusCreated, map[string]any{"id": id})
}

// handleAPIProjectUses is GET /api/v1/projects/{project}/uses (§10, M18).
//
// What a project holds, from the server's side, so the plugin's "in this project" screen can
// compare it against the committed manifest. Two answers come only from here:
//
//   - `outdated`: the library's content hash differs from the one recorded at import, so the
//     project is holding an older copy of the asset.
//   - anything in the manifest and *not* in this response was imported while the server was
//     unreachable. §10 promised the manifest made that replayable; until this endpoint there
//     was no way to find out which entries needed replaying.
//
// An unknown project is an empty list, not a 404: a project that has never imported anything is
// a perfectly ordinary state and the plugin should show an empty screen, not an error.
func (s *Server) handleAPIProjectUses(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("project")
	uses, err := s.projects.UsesOfProject(r.Context(), uuid)
	if err != nil {
		s.log.ErrorContext(r.Context(), "listing project uses failed", "project", uuid, "error", err)
		s.apiError(w, http.StatusInternalServerError, "could not list this project's assets")
		return
	}

	out := make([]map[string]any, 0, len(uses))
	for _, u := range uses {
		out = append(out, map[string]any{
			"id": u.ID, "asset_id": u.AssetID, "res_path": u.ResPath,
			"added_at": u.AddedAt.Unix(),
			"filename": u.Filename, "ext": u.Ext, "kind": u.Kind, "size": u.Size,
			"pack": u.PackName,
			// Both hashes, not just the verdict: a client that wants to explain *why*
			// something is outdated has the two values to show.
			"imported_sha256": u.ImportedSHA256, "sha256": u.SHA256,
			"outdated": u.Outdated(), "missing": u.Missing,
		})
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"uses": out, "project": uuid})
}

// handleAPIRemoveUse is DELETE /api/v1/projects/{project}/uses/{id} (§10).
func (s *Server) handleAPIRemoveUse(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("project")
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.projects.RemoveUse(r.Context(), uuid, id); err != nil {
		s.apiError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleAPICredits is GET /api/v1/projects/{project}/credits.md (§9, §10).
func (s *Server) handleAPICredits(w http.ResponseWriter, r *http.Request) {
	uuid := r.PathValue("project")
	lines, err := s.projects.Credits(r.Context(), uuid)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "unknown project")
		return
	}
	var name string
	s.db.Reader.QueryRowContext(r.Context(), `SELECT name FROM projects WHERE uuid = ?`, uuid).Scan(&name)

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(projects.RenderCredits(name, lines)))
}

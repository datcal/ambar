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

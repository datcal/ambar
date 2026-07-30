package server

import (
	"net/http"
	"strconv"
	"time"

	"github.com/datcal/ambar/internal/derive"
)

// handleSheet serves the animated spritesheet preview (§6).
func (s *Server) handleSheet(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	s.serveDerivative(w, r, asset, derive.FileSheet, "image/gif")
}

// handleFrames records a confirmed or corrected frame grid (§6: "let the user
// confirm in one click or correct manually", frame_source promoted to manual so a
// human's grid is never silently re-guessed).
func (s *Server) handleFrames(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	back := "/assets/" + strconv.FormatInt(asset.ID, 10)

	cols, errC := strconv.Atoi(r.PostFormValue("cols"))
	rows, errR := strconv.Atoi(r.PostFormValue("rows"))
	if errC != nil || errR != nil || cols < 1 || rows < 1 || cols > 256 || rows > 256 {
		s.redirectWithMessage(w, r, back, "Columns and rows must be between 1 and 256.")
		return
	}
	if asset.Width == 0 || asset.Height == 0 {
		s.redirectWithMessage(w, r, back, "This image has no known dimensions to divide.")
		return
	}

	frameW := asset.Width / cols
	frameH := asset.Height / rows
	if _, err := s.db.Writer.ExecContext(r.Context(), `
		UPDATE assets SET
		    frame_cols = ?, frame_rows = ?, frame_w = ?, frame_h = ?, frame_count = ?,
		    frame_source = 'manual', updated_at = ?
		WHERE id = ?`,
		cols, rows, frameW, frameH, cols*rows, time.Now().Unix(), asset.ID); err != nil {
		s.log.ErrorContext(r.Context(), "recording frame grid failed", "asset_id", asset.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.redirectWithMessage(w, r, back, "Saved the frame grid.")
}

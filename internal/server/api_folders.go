package server

import (
	"net/http"
	"strings"

	"github.com/datcal/ambar/internal/index"
)

type folderJSON struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Assets      int    `json:"assets"`
	Direct      int    `json:"direct"`
	HasChildren bool   `json:"has_children"`
}

// handleAPIFolders is GET /api/v1/folders?path=: the sub-folders of one library directory,
// with how many assets sit at or below each. One level per request, so a client can open
// folders lazily and cache the names — the library can have thousands of directories.
//
// `path` is library-relative and slash-separated; empty is the library root. The assets in
// a folder come from /api/v1/search?dir=<path>.
//
// The tree is the sidebar's cached one (sidebar.go). It used to have a cache of its own, which
// meant the same GROUP BY over every asset ran twice — once for the browser, once for the
// editor plugins — and the folder view made that query heavier, so there is one now.
func (s *Server) handleAPIFolders(w http.ResponseWriter, r *http.Request) {
	root := s.folderTree(r.Context())
	if root == nil {
		s.log.ErrorContext(r.Context(), "api folders failed: no folder tree")
		s.apiError(w, http.StatusInternalServerError, "could not read the folder tree")
		return
	}

	want := strings.Trim(strings.ReplaceAll(r.URL.Query().Get("path"), "\\", "/"), "/")
	node := root
	if want != "" {
		for _, seg := range strings.Split(want, "/") {
			var next *index.TreeNode
			for _, c := range node.Children {
				if c.Name == seg {
					next = c
					break
				}
			}
			if next == nil {
				s.apiError(w, http.StatusNotFound, "no such folder")
				return
			}
			node = next
		}
	}

	out := make([]folderJSON, 0, len(node.Children))
	for _, c := range node.Children {
		out = append(out, folderJSON{
			Name: c.Name, Path: c.Path, Assets: c.Assets, Direct: c.Direct, HasChildren: c.HasChildren(),
		})
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"path": node.Path, "assets": node.Assets, "direct": node.Direct, "folders": out,
	})
}

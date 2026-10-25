package server

import (
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/datcal/ambar/internal/index"
)

// folderTreeTTL is how long the folder tree is reused. The tree is a GROUP BY over every
// asset, and a client that opens folder after folder would otherwise pay for it each time.
// A rescan changes it rarely, and thirty seconds of staleness is invisible in a browser.
const folderTreeTTL = 30 * time.Second

type folderTreeCache struct {
	mu   sync.Mutex
	at   time.Time
	root *index.TreeNode
}

type folderJSON struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Assets      int    `json:"assets"`
	Direct      int    `json:"direct"`
	HasChildren bool   `json:"has_children"`
}

func (c *folderTreeCache) get(r *http.Request, ix *index.Indexer) (*index.TreeNode, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.root != nil && time.Since(c.at) < folderTreeTTL {
		return c.root, nil
	}
	root, err := ix.Tree(r.Context(), 0)
	if err != nil {
		return nil, err
	}
	c.root, c.at = root, time.Now()
	return root, nil
}

// handleAPIFolders is GET /api/v1/folders?path=: the sub-folders of one library directory,
// with how many assets sit at or below each. One level per request, so a client can open
// folders lazily and cache the names — the library can have thousands of directories.
//
// `path` is library-relative and slash-separated; empty is the library root. The assets in
// a folder come from /api/v1/search?dir=<path>.
func (s *Server) handleAPIFolders(w http.ResponseWriter, r *http.Request) {
	root, err := s.folders.get(r, s.index)
	if err != nil {
		s.log.ErrorContext(r.Context(), "api folders failed", "error", err)
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

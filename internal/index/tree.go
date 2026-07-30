package index

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// The folder tree (M14).
//
// The library on disk has a structure the operator built on purpose — buckets,
// vendor pack directories, format folders — and until now the UI threw all of it
// away and showed one flat grid. §5.1 is explicit that the folder layout carries
// meaning ("do not treat organisational parents as packs" only makes sense if the
// parents are visible somewhere), and browsing by folder is how anyone actually
// finds "that tileset I downloaded last year".
//
// The tree is derived, never stored: it is the set of directories that contain
// indexed assets, which is exactly what the filesystem says and what a rescan
// updates for free.

// DefaultTreeDepth is how deep the sidebar renders. Four levels covers
// bucket/pack/format-folder/subject, which is the shape §5.1 describes; deeper
// directories are still reachable through the grid and its filters.
const DefaultTreeDepth = 4

// TreeNode is one directory in the library tree.
type TreeNode struct {
	// Name is the directory's own name; Path is library-relative and slash-separated.
	Name string
	Path string
	// Assets is how many live assets sit at or below this directory.
	Assets int
	// Direct is how many sit *in* it, excluding subdirectories — the difference is
	// what tells you whether a node is a container or a leaf worth opening.
	Direct   int
	Children []*TreeNode
}

// HasChildren reports whether the node has subdirectories, for templates.
func (n *TreeNode) HasChildren() bool { return len(n.Children) > 0 }

// Depth is how deep the node sits, so a template can indent without recursion.
func (n *TreeNode) Depth() int {
	if n.Path == "" {
		return 0
	}
	return strings.Count(n.Path, "/") + 1
}

// Contains reports whether path is this node or below it — what a template needs to
// decide which branches to open.
func (n *TreeNode) Contains(path string) bool {
	if n.Path == "" {
		return true
	}
	return path == n.Path || strings.HasPrefix(path, n.Path+"/")
}

// Tree returns the library's directory tree with asset counts.
//
// maxDepth limits how deep the tree is built (0 means unlimited). A vendor pack can
// nest ten levels of format folders, and a sidebar that renders all of them is a
// scrollbar rather than a navigation aid — the grid's own path filter covers the
// rest.
func (ix *Indexer) Tree(ctx context.Context, maxDepth int) (*TreeNode, error) {
	// One row per directory holding assets. Doing the grouping in SQL keeps the Go
	// side proportional to the number of directories (thousands) rather than to the
	// number of assets (tens of thousands).
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT CASE WHEN p.library_rel_path IN ('', '.') THEN a.rel_path
		            ELSE p.library_rel_path || '/' || a.rel_path END AS lib_path,
		       count(*)
		FROM assets a
		JOIN packs p ON p.id = a.pack_id
		WHERE a.missing_since IS NULL
		GROUP BY lib_path`)
	if err != nil {
		return nil, fmt.Errorf("load library tree: %w", err)
	}
	defer rows.Close()

	root := &TreeNode{}
	index := map[string]*TreeNode{"": root}

	for rows.Next() {
		var libPath string
		var n int
		if err := rows.Scan(&libPath, &n); err != nil {
			return nil, fmt.Errorf("scan tree row: %w", err)
		}

		// The directory holding this file; a file at the root belongs to the root node.
		dir := ""
		if i := strings.LastIndex(libPath, "/"); i > 0 {
			dir = libPath[:i]
		}

		node := root
		if dir != "" {
			node = ensureBranch(index, dir, maxDepth)
		}
		node.Direct += n

		// Roll the count up every ancestor, so a collapsed branch still shows how much
		// is inside it.
		for cur := node; cur != nil; cur = index[parentOf(cur.Path)] {
			cur.Assets += n
			if cur.Path == "" {
				break
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load library tree: %w", err)
	}

	sortTree(root)
	return root, nil
}

// ensureBranch creates the node for dir and every missing ancestor, honouring
// maxDepth by attributing anything deeper to its deepest allowed ancestor.
func ensureBranch(index map[string]*TreeNode, dir string, maxDepth int) *TreeNode {
	segments := strings.Split(dir, "/")
	if maxDepth > 0 && len(segments) > maxDepth {
		segments = segments[:maxDepth]
	}

	path := ""
	node := index[""]
	for _, seg := range segments {
		if path == "" {
			path = seg
		} else {
			path += "/" + seg
		}
		child, ok := index[path]
		if !ok {
			child = &TreeNode{Name: seg, Path: path}
			index[path] = child
			node.Children = append(node.Children, child)
		}
		node = child
	}
	return node
}

func parentOf(path string) string {
	if i := strings.LastIndex(path, "/"); i > 0 {
		return path[:i]
	}
	return ""
}

// sortTree orders children by name, case-insensitively, so the sidebar reads like a
// file manager rather than like a database.
func sortTree(n *TreeNode) {
	sort.Slice(n.Children, func(i, j int) bool {
		a, b := strings.ToLower(n.Children[i].Name), strings.ToLower(n.Children[j].Name)
		if a == b {
			return n.Children[i].Name < n.Children[j].Name
		}
		return a < b
	})
	for _, c := range n.Children {
		sortTree(c)
	}
}

// FlatNode is a tree node flattened for rendering, carrying only what the template
// needs. Templates cannot recurse without a named template calling itself, and a
// flat list with a depth is easier to read than that.
type FlatNode struct {
	*TreeNode
	// Open is whether this branch is on the path to the selected directory, so the
	// template can render it expanded.
	Open bool
	// Selected marks the current directory.
	Selected bool
}

// Flatten walks the tree depth-first, emitting only the nodes that should be
// visible: the top level always, plus the children of every open branch. selected
// is the currently browsed directory ("" for the whole library).
func Flatten(root *TreeNode, selected string) []FlatNode {
	var out []FlatNode
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		for _, c := range n.Children {
			open := c.Contains(selected) && selected != ""
			out = append(out, FlatNode{TreeNode: c, Open: open, Selected: c.Path == selected})
			if open {
				walk(c)
			}
		}
	}
	walk(root)
	return out
}

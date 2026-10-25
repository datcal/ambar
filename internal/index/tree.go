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

// DefaultTreeDepth is how deep the sidebar's tree goes. It was four — bucket, pack,
// format folder, subject — but a vendor pack nests one level more than that
// (3d/Synty/<pack>/Source/FBX), and the folder view is now how the library is browsed, so
// a directory the tree cannot reach is a directory you cannot open. Flatten only ever
// renders the branch you are in, so depth costs memory in a cached snapshot rather than
// rows on screen; the cap is only there for a pathological library.
const DefaultTreeDepth = 12

// TreeNode is one directory in the library tree.
type TreeNode struct {
	// Name is the directory's own name; Path is library-relative and slash-separated.
	Name string
	Path string
	// Assets is how many live assets sit at or below this directory.
	Assets int
	// Direct is how many sit *in* it, excluding subdirectories — the difference is
	// what tells you whether a node is a container or a leaf worth opening.
	Direct int

	// Groups counts logical assets — format variants collapsed, hidden ones left out — at
	// or below this directory, placed where their primary file sits. That is exactly what
	// the grid shows for `dir=<Path>`, which Assets is not: a model shipped as FBX and OBJ
	// is two files and one tile. Kinds splits Groups by kind the way the grid's kind filter
	// does — a group counts under every kind one of its variants has — so the folder tree
	// can say where the *models* are, and the number is what clicking it lists.
	Groups int
	Kinds  map[string]int
	// DirectGroups and DirectKinds are the same, for this folder alone: the grid with
	// "include subfolders" switched off.
	DirectGroups int
	DirectKinds  map[string]int

	Children []*TreeNode
}

// Tally is the folder's kind switch, largest kind first: what KindCounts returns for
// Dir=Path with no search, read from the cached tree instead of counted again. direct is
// "include subfolders" switched off.
func (n *TreeNode) Tally(direct bool) KindTally {
	total, kinds := n.Groups, n.Kinds
	if direct {
		total, kinds = n.DirectGroups, n.DirectKinds
	}
	t := KindTally{Total: total}
	for kind, count := range kinds {
		if count > 0 {
			t.Kinds = append(t.Kinds, KindCount{Kind: kind, Count: count})
		}
	}
	sort.Slice(t.Kinds, func(i, j int) bool {
		if t.Kinds[i].Count != t.Kinds[j].Count {
			return t.Kinds[i].Count > t.Kinds[j].Count
		}
		return t.Kinds[i].Kind < t.Kinds[j].Kind
	})
	return t
}

// HasChildren reports whether the node has subdirectories, for templates.
func (n *TreeNode) HasChildren() bool { return len(n.Children) > 0 }

// Count is the number the folder view shows for this directory: its grid total for a
// kind, or for everything when kind is empty.
func (n *TreeNode) Count(kind string) int {
	if kind == "" {
		return n.Groups
	}
	return n.Kinds[kind]
}

// Find returns the node at a library-relative path, or nil when there is none.
func (n *TreeNode) Find(path string) *TreeNode {
	path = strings.Trim(path, "/")
	if path == "" {
		return n
	}
	node := n
	for _, seg := range strings.Split(path, "/") {
		var next *TreeNode
		for _, c := range node.Children {
			if c.Name == seg {
				next = c
				break
			}
		}
		if next == nil {
			return nil
		}
		node = next
	}
	return node
}

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
	// Doing the grouping in SQL keeps the Go side proportional to the number of directories
	// (thousands) rather than to the number of assets (a hundred thousand).
	//
	// The directory is cut in SQL rather than grouping by file path, which would be one row
	// per file: rtrim(path, replace(path, '/', '')) strips the trailing name — every
	// character that is not a slash, from the right — leaving "dir/", or "" at the root.
	root := &TreeNode{Kinds: map[string]int{}, DirectKinds: map[string]int{}}
	index := map[string]*TreeNode{"": root}
	nodeFor := func(dir string) *TreeNode {
		dir = strings.TrimSuffix(dir, "/")
		if dir == "" {
			return root
		}
		return ensureBranch(index, dir, maxDepth)
	}
	// up calls f on a node and every ancestor, so a collapsed branch still shows how much
	// is inside it.
	up := func(node *TreeNode, f func(*TreeNode)) {
		for cur := node; cur != nil; cur = index[parentOf(cur.Path)] {
			f(cur)
			if cur.Path == "" {
				break
			}
		}
	}

	// Files, per directory.
	rows, err := ix.db.Reader.QueryContext(ctx, `
		SELECT rtrim(lib_path, replace(lib_path, '/', '')) AS dir, count(*)
		FROM (
			SELECT `+libPathExpr("a")+` AS lib_path
			FROM assets a
			JOIN packs p ON p.id = a.pack_id
			WHERE a.missing_since IS NULL
		)
		GROUP BY dir`)
	if err != nil {
		return nil, fmt.Errorf("load library tree: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var dir string
		var files int
		if err := rows.Scan(&dir, &files); err != nil {
			return nil, fmt.Errorf("scan tree row: %w", err)
		}
		node := nodeFor(dir)
		node.Direct += files
		up(node, func(n *TreeNode) { n.Assets += files })
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load library tree: %w", err)
	}

	// Groups, per directory of their primary: in total (kind '') and per kind of any
	// variant — the grid's own rules (groupFilters), hidden assets left out as it leaves
	// them out by default. Each group sits in exactly one directory, so the sums roll up.
	groupRows, err := ix.db.Reader.QueryContext(ctx, `
		WITH prim AS MATERIALIZED (
			SELECT gid, rtrim(lib_path, replace(lib_path, '/', '')) AS dir
			FROM (
				SELECT g.id AS gid, `+libPathExpr("a")+` AS lib_path
				FROM asset_groups g
				JOIN assets a ON a.id = g.primary_asset_id
				JOIN packs p ON p.id = g.pack_id
				WHERE a.missing_since IS NULL AND `+notDisabledExpr("a")+`
			)
		)
		SELECT dir, '' AS kind, count(*) FROM prim GROUP BY dir
		UNION ALL
		SELECT dir, kind, count(*)
		FROM (SELECT DISTINCT prim.gid, prim.dir, kv.kind
		      FROM prim JOIN assets kv ON kv.group_id = prim.gid)
		GROUP BY dir, kind`)
	if err != nil {
		return nil, fmt.Errorf("load library tree groups: %w", err)
	}
	defer groupRows.Close()
	for groupRows.Next() {
		var dir, kind string
		var n int
		if err := groupRows.Scan(&dir, &kind, &n); err != nil {
			return nil, fmt.Errorf("scan tree group row: %w", err)
		}
		node := nodeFor(dir)
		if kind == "" {
			node.DirectGroups += n
			up(node, func(t *TreeNode) { t.Groups += n })
		} else {
			node.DirectKinds[kind] += n
			up(node, func(t *TreeNode) { t.Kinds[kind] += n })
		}
	}
	if err := groupRows.Err(); err != nil {
		return nil, fmt.Errorf("load library tree groups: %w", err)
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
			child = &TreeNode{Name: seg, Path: path, Kinds: map[string]int{}, DirectKinds: map[string]int{}}
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

// FolderSearchLimit caps FindFolders for the dropdowns that call it.
const FolderSearchLimit = 8

// FindFolders finds directories by name: every word of query must appear in the
// directory's path, and the last word in its own name. That second rule is what keeps
// "spy" from also returning every folder *inside* POLYGON - Spy Kit, while still letting
// "spy fbx" reach POLYGON - Spy Kit/Source/FBX.
//
// It walks the cached tree rather than querying: the tree is already in memory for the
// sidebar, it holds every directory there is, and this runs on a keystroke.
//
// Ranked by how well the name matches (exact, then prefix, then anywhere), then shallower
// first — a pack beats a folder inside some other pack with the same word in it — then by
// size.
func FindFolders(root *TreeNode, query string, limit int) []*TreeNode {
	words := strings.Fields(strings.ToLower(query))
	if root == nil || len(words) == 0 {
		return nil
	}
	last := words[len(words)-1]

	type hit struct {
		node  *TreeNode
		score int
	}
	var hits []hit
	var walk func(n *TreeNode)
	walk = func(n *TreeNode) {
		for _, c := range n.Children {
			name := strings.ToLower(c.Name)
			if strings.Contains(name, last) {
				path := strings.ToLower(c.Path)
				all := true
				for _, w := range words {
					if !strings.Contains(path, w) {
						all = false
						break
					}
				}
				if all {
					score := 3
					switch {
					case name == last:
						score = 0
					case strings.HasPrefix(name, last):
						score = 1
					case strings.Contains(name, " "+last) || strings.Contains(name, "-"+last) ||
						strings.Contains(name, "_"+last):
						score = 2 // the start of a word inside the name: "POLYGON - Spy Kit"
					}
					hits = append(hits, hit{c, score})
				}
			}
			walk(c)
		}
	}
	walk(root)

	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score < b.score
		}
		if da, db := a.node.Depth(), b.node.Depth(); da != db {
			return da < db
		}
		if a.node.Groups != b.node.Groups {
			return a.node.Groups > b.node.Groups
		}
		return strings.ToLower(a.node.Path) < strings.ToLower(b.node.Path)
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]*TreeNode, len(hits))
	for i, h := range hits {
		out[i] = h.node
	}
	return out
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

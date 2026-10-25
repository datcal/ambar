package server

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/datcal/ambar/internal/index"
)

// The folder view: the library page as a file manager with a search box, rather than a
// search box with a folder list hidden under the tags.
//
// What it answers is "show me only the models in /mnt/game-assets/3d/Synty/POLYGON - Spy
// Kit". The pieces were all there — `dir` and `kind` filter the grid, the sidebar had a
// tree — but the tree sat collapsed under kinds, colours and tags, stopped four levels
// down, counted files where the grid shows assets, and the kind counts described the whole
// library whatever folder you were in. So "models" always meant the same sixty thousand.
//
// Here the current folder is the scope of everything: the breadcrumb says where you are,
// the kind switch counts what is *in it*, the tree counts the kind you picked, and a path
// pasted from your own file manager takes you there.

// crumb is one segment of the breadcrumb.
type crumb struct {
	Name string
	URL  string
	Last bool
}

// kindTab is one choice on the kind switch, counted within the current scope.
type kindTab struct {
	Kind  string
	Label string
	Count int
	URL   string
	On    bool
}

// folderLink is a subfolder offered under the toolbar.
type folderLink struct {
	Name  string
	Path  string
	URL   string
	Count int
}

// folderView is the folder half of the library page.
type folderView struct {
	Dir   string
	Title string
	// Groups and Files are the folder's totals regardless of filters: what is in here.
	Groups int
	Files  int

	Crumbs     []crumb
	Kinds      []kindTab
	Subfolders []folderLink

	// DirOnly is "include subfolders" switched off; HiddenBelow is how many matches the
	// subfolders hold when that leaves the folder looking empty.
	DirOnly     bool
	HiddenBelow int
	List        bool

	// LocalPath is the folder as the operator's machine sees it, for the copy button.
	LocalPath string

	SubURL       string
	GridURL      string
	ListURL      string
	ClearDirURL  string
	ClearKindURL string
	ClearAllURL  string
}

// kindLabels names the kinds the switch can show, plural because they label counts.
var kindLabels = map[string]string{
	"model": "Models", "image": "Images", "audio": "Audio", "font": "Fonts",
	"spritesheet": "Spritesheets", "texture": "Textures", "material": "Materials",
	"tilemap": "Tilemaps", "rig": "Rigs", "video": "Video", "hdri": "HDRIs",
	"script": "Code", "other": "Other",
}

func kindLabel(kind string) string {
	if l, ok := kindLabels[kind]; ok {
		return l
	}
	return kind
}

// libraryLink rebuilds a library URL from the current query with some parameters changed,
// given as name/value pairs; an empty value removes the parameter. Paging always resets —
// page 4 of one folder is not page 4 of another — and a one-off message is never carried.
//
// Slashes stay readable in the result, so a folder link reads
// /?dir=3d/Synty/POLYGON+-+Spy+Kit rather than a wall of %2F. A slash is legal in a query
// string; only the encoder is cautious.
func libraryLink(current url.Values, set ...string) string {
	v := url.Values{}
	for k, vals := range current {
		switch k {
		case "page", "cursor", "msg":
			continue
		}
		v[k] = append([]string(nil), vals...)
	}
	for i := 0; i+1 < len(set); i += 2 {
		if set[i+1] == "" {
			v.Del(set[i])
		} else {
			v.Set(set[i], set[i+1])
		}
	}
	if len(v) == 0 {
		return "/"
	}
	return "/?" + strings.ReplaceAll(v.Encode(), "%2F", "/")
}

// folderTree is the cached library tree, or nil when it could not be built.
func (s *Server) folderTree(ctx context.Context) *index.TreeNode {
	snap, err := s.nav.get(ctx, s.buildSidebar)
	if err != nil || snap == nil {
		return nil
	}
	return snap.Tree
}

// applyFolderView fills the folder view for the library page. It runs after applySidebar,
// whose snapshot it reads the tree and the unfiltered kind counts from.
func (s *Server) applyFolderView(r *http.Request, data *pageData, opts index.ListOptions, page *index.GroupPage) {
	ctx := r.Context()
	q := r.URL.Query()
	dir := strings.Trim(opts.Dir, "/")

	fv := &folderView{
		Dir:     dir,
		DirOnly: opts.DirOnly,
		List:    q.Get("view") == "list",
	}
	data.Folder = fv
	data.DirURL = func(path string) string { return libraryLink(q, "dir", path) }

	root := s.folderTree(ctx)

	// The breadcrumb, from the library root down.
	fv.Crumbs = append(fv.Crumbs, crumb{Name: "Library", URL: libraryLink(q, "dir", "", "sub", "")})
	if dir != "" {
		segs := strings.Split(dir, "/")
		for i, seg := range segs {
			fv.Crumbs = append(fv.Crumbs, crumb{
				Name: seg,
				URL:  libraryLink(q, "dir", strings.Join(segs[:i+1], "/")),
			})
		}
		fv.Title = segs[len(segs)-1]
	} else {
		fv.Title = "Library"
	}
	fv.Crumbs[len(fv.Crumbs)-1].Last = true

	var node *index.TreeNode
	if root != nil {
		node = root.Find(dir)
	}
	if node != nil {
		fv.Groups, fv.Files = node.Groups, node.Assets
		for _, c := range node.Children {
			n := c.Count(opts.Kind)
			if n == 0 {
				continue
			}
			fv.Subfolders = append(fv.Subfolders, folderLink{
				Name: c.Name, Path: c.Path, Count: n, URL: libraryLink(q, "dir", c.Path),
			})
		}
		if opts.DirOnly && page != nil && page.Total == 0 {
			fv.HiddenBelow = node.Count(opts.Kind)
		}
	}

	// The kind switch counts the current scope — folder, search, subfolder switch — with
	// the kind itself left out, so every choice shows what it would list. Browsing a folder
	// is most page views, and the cached tree already holds those numbers counted by the
	// grid's own rules; only a search has to be counted live. Measured, that difference is
	// the whole cost of the switch: a live count is as expensive as the grid's own query.
	tally := index.KindTally{}
	byTree := node != nil && opts.Query == "" && opts.PackID == 0 &&
		!opts.IncludeMissing && !opts.IncludeDisabled
	if byTree {
		tally = node.Tally(opts.DirOnly)
	} else if t, err := s.index.KindCounts(ctx, opts); err != nil {
		s.log.ErrorContext(ctx, "kind counts failed", "error", err)
	} else {
		tally = t
	}
	fv.Kinds = append(fv.Kinds, kindTab{
		Label: "All", Count: tally.Total, URL: libraryLink(q, "kind", ""), On: opts.Kind == "",
	})
	seen := false
	for _, k := range tally.Kinds {
		if k.Count == 0 {
			continue
		}
		on := k.Kind == opts.Kind
		seen = seen || on
		fv.Kinds = append(fv.Kinds, kindTab{
			Kind: k.Kind, Label: kindLabel(k.Kind), Count: k.Count,
			URL: libraryLink(q, "kind", k.Kind), On: on,
		})
	}
	// A kind with nothing in this folder still gets its tab while it is the filter, or the
	// page would show "nothing here" with no visible reason.
	if opts.Kind != "" && !seen {
		fv.Kinds = append(fv.Kinds, kindTab{
			Kind: opts.Kind, Label: kindLabel(opts.Kind), URL: libraryLink(q, "kind", opts.Kind), On: true,
		})
	}

	if dir != "" {
		if local, ok := localPathFor(s.cfg.LocalLibraryPath, dir); ok {
			fv.LocalPath = local.Path
		}
	} else if lp := strings.TrimSpace(s.cfg.LocalLibraryPath); lp != "" {
		fv.LocalPath = lp
	}

	if opts.DirOnly {
		fv.SubURL = libraryLink(q, "sub", "")
	} else {
		fv.SubURL = libraryLink(q, "sub", "0")
	}
	fv.GridURL = libraryLink(q, "view", "")
	fv.ListURL = libraryLink(q, "view", "list")
	fv.ClearDirURL = libraryLink(q, "dir", "", "sub", "")
	fv.ClearKindURL = libraryLink(q, "kind", "")
	keep := url.Values{}
	for _, k := range []string{"view", "sort", "per"} {
		if v := q.Get(k); v != "" {
			keep.Set(k, v)
		}
	}
	fv.ClearAllURL = libraryLink(keep)
}

// libraryRelative turns what someone pasted into a library-relative path: a path on their
// own machine (AMBAR_LOCAL_LIBRARY_PATH), a path inside the container, a file:// link, or
// something already relative. Backslashes become slashes so a Windows path works too.
//
// The result is only ever looked up in the folder tree, never opened, so it needs no
// safepath resolution — a path that is not in the tree goes nowhere.
func (s *Server) libraryRelative(raw string) string {
	p := strings.Trim(strings.TrimSpace(raw), `"'`)
	if strings.HasPrefix(strings.ToLower(p), "file://") {
		p = p[len("file://"):]
		if unescaped, err := url.PathUnescape(p); err == nil {
			p = unescaped
		}
	}
	p = strings.ReplaceAll(p, `\`, "/")

	for _, prefix := range []string{s.cfg.LocalLibraryPath, s.cfg.LibraryRoot} {
		pre := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(prefix), `\`, "/"), "/")
		if pre == "" {
			continue
		}
		// Windows paths compare without case, as the filesystem they name does.
		windows := isWindowsStyle(strings.TrimSpace(prefix))
		if len(p) >= len(pre) {
			head := p[:len(pre)]
			same := head == pre || (windows && strings.EqualFold(head, pre))
			if same && (len(p) == len(pre) || p[len(pre)] == '/') {
				p = p[len(pre):]
				break
			}
		}
	}
	return strings.Trim(p, "/")
}

// handleGo is GET /go?path=…: the "go to path" box, and what the folder search falls back
// to when you press Enter on a name. It accepts a local path, a container path, a
// file:// link, a library-relative path, a file inside a folder (the folder opens), or just
// a folder's name, and redirects to that folder with the current kind and view kept.
func (s *Server) handleGo(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw := q.Get("path")
	carried := url.Values{}
	for _, k := range []string{"kind", "view", "sort", "per", "sub"} {
		if v := q.Get(k); v != "" {
			carried.Set(k, v)
		}
	}

	target, msg := s.resolveFolder(r.Context(), raw)
	dest := libraryLink(carried, "dir", target)
	// libraryLink never carries a message — it builds links that live on a page — so the
	// one this redirect has to deliver is added here.
	if msg != "" {
		sep := "?"
		if strings.Contains(dest, "?") {
			sep = "&"
		}
		dest += sep + "msg=" + url.QueryEscape(msg)
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// resolveFolder finds the folder a typed or pasted string names, with a message when it
// had to settle for something nearby.
func (s *Server) resolveFolder(ctx context.Context, raw string) (dir, msg string) {
	rel := s.libraryRelative(raw)
	if rel == "" {
		return "", ""
	}
	root := s.folderTree(ctx)
	if root == nil {
		return rel, ""
	}
	if root.Find(rel) != nil {
		return rel, ""
	}
	// A file: open the folder it is in.
	if parent := parentDir(rel); parent != "" && root.Find(parent) != nil {
		return parent, ""
	}
	// A name rather than a path.
	if !strings.Contains(rel, "/") {
		if hits := index.FindFolders(root, raw, 1); len(hits) > 0 {
			return hits[0].Path, ""
		}
		return "", "No folder is called “" + strings.TrimSpace(raw) + "”."
	}
	// The nearest folder that does exist, said out loud.
	for p := parentDir(rel); p != ""; p = parentDir(p) {
		if root.Find(p) != nil {
			return p, "There is no " + rel + " in the library; this is the nearest folder that exists."
		}
	}
	return "", "There is no " + rel + " in the library."
}

func parentDir(p string) string {
	if i := strings.LastIndex(p, "/"); i > 0 {
		return p[:i]
	}
	return ""
}

// folderHit is one row of the folder search.
type folderHit struct {
	Name   string
	Parent string
	Path   string
	URL    string
	Count  int
}

// findFolders runs the folder search for a typed string. A path is resolved first, so
// pasting one offers exactly that folder; then folders whose names match.
func (s *Server) findFolders(ctx context.Context, raw string, current url.Values) []folderHit {
	root := s.folderTree(ctx)
	if root == nil || strings.TrimSpace(raw) == "" {
		return nil
	}
	kind := current.Get("kind")
	var hits []folderHit
	seen := map[string]bool{}
	add := func(n *index.TreeNode) {
		if n == nil || n.Path == "" || seen[n.Path] {
			return
		}
		seen[n.Path] = true
		hits = append(hits, folderHit{
			Name: n.Name, Parent: parentDir(n.Path), Path: n.Path,
			URL: libraryLink(current, "dir", n.Path, "q", ""), Count: n.Count(kind),
		})
	}
	if strings.ContainsAny(raw, `/\`) {
		if rel := s.libraryRelative(raw); rel != "" {
			if n := root.Find(rel); n != nil {
				add(n)
			} else {
				add(root.Find(parentDir(rel)))
			}
		}
	}
	for _, n := range index.FindFolders(root, raw, index.FolderSearchLimit) {
		add(n)
	}
	return hits
}

// handleFolderFind is GET /folders/find?q=: the sidebar's "jump to folder" box, as a list
// of links. The current kind and view travel in the query so a jump keeps them.
func (s *Server) handleFolderFind(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	current := url.Values{}
	for _, k := range []string{"kind", "view", "sort", "per"} {
		if v := q.Get(k); v != "" {
			current.Set(k, v)
		}
	}
	data := s.newPageData(r)
	data.FolderHits = s.findFolders(r.Context(), q.Get("q"), current)
	data.Kind = current.Get("kind")
	s.renderPartial(w, r, "assets.html", "folder-hits", http.StatusOK, data)
}

// handleFacets is GET /facets?<the page's filters>: the tags present in a result set, for
// the sidebar's Tags section when it is opened. Computing them with every page cost about a
// second on a vendor folder of a large library, for a folded list.
func (s *Server) handleFacets(w http.ResponseWriter, r *http.Request) {
	opts := browseOptions(r)
	data := s.newPageData(r)
	data.Search, data.Kind, data.Dir, data.IncludeMissing = opts.Query, opts.Kind, opts.Dir, opts.IncludeMissing
	if facets, err := s.index.Facets(r.Context(), opts, index.DefaultFacetLimit); err != nil {
		s.log.ErrorContext(r.Context(), "facets failed", "error", err)
	} else {
		data.Facets = facets
	}
	s.renderPartial(w, r, "assets.html", "facets", http.StatusOK, data)
}

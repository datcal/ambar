package server

import (
	"errors"
	"fmt"
	"github.com/datcal/ambar/internal/derive"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/safepath"
)

// handleAssets renders the library: the three-pane workspace with navigation on the
// left and the thumbnail grid in the middle (§8). It serves both "/" and "/assets",
// because the library is the front door.
func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	// The grid pages by number since M16, so a `cursor` in the URL is either a bookmark
	// from before that or someone editing the query string. Either way the honest answer is
	// the first page of the same view rather than silently ignoring the parameter.
	if q.Get("cursor") != "" {
		http.Redirect(w, r, pageURL(r, 1), http.StatusSeeOther)
		return
	}

	opts := index.ListOptions{
		Query:          strings.TrimSpace(q.Get("q")),
		Kind:           q.Get("kind"),
		Dir:            q.Get("dir"),
		IncludeMissing: q.Get("missing") == "1",
		Sort:           index.ParseSort(q.Get("sort")),
		Limit:          pageSizeParam(q.Get("per")),
		// Always at least 1: the zero value means "cursor mode" to ListGroups, which is
		// the API's paging, not the grid's.
		Page: pageParam(q.Get("page")),
	}
	if raw := q.Get("pack"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			opts.PackID = id
		}
	}

	// Groups, not individual files: §5.1's collapsing is what makes the grid usable.
	page, err := s.index.ListGroups(r.Context(), opts)
	if err != nil {
		// A malformed cursor is the user's URL being wrong, not a server fault. (The grid
		// no longer sends cursors, but a bookmark from before M16 might.)
		if strings.Contains(err.Error(), "cursor") {
			http.Redirect(w, r, "/assets", http.StatusSeeOther)
			return
		}
		s.log.ErrorContext(r.Context(), "listing assets failed", "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	data := s.newPageData(r)
	data.Workspace = true
	data.Nav = "library"
	data.Page = page
	data.Search = opts.Query
	data.Kind = opts.Kind
	data.IncludeMissing = opts.IncludeMissing
	data.Sort = string(opts.Sort)
	data.SortOptions = index.SortOrders()
	data.PageSizes = pageSizes
	data.PageURL = func(n int) string { return pageURL(r, n) }
	// Does this page hold a model with no thumbnail? If so the browser is asked to
	// render one (M15), and only then is three.js worth loading here.
	s.markModelThumbs(page.Groups)
	for _, g := range page.Groups {
		if g.Primary.NeedsBrowserThumb() {
			data.NeedsModelThumbs = true
			break
		}
	}
	data.Flash = q.Get("msg")

	data.Dir = opts.Dir

	// Per-tile launch links and local paths (M16), when the operator has told us how the
	// library is mounted on their machine. String work only — no filesystem, no queries — so
	// a hundred tiles cost nothing measurable.
	if s.cfg.LocalLibraryPath != "" {
		data.TileApps = make(map[int64][]openApp, len(page.Groups))
		data.TilePaths = make(map[int64]string, len(page.Groups))
		for _, g := range page.Groups {
			primary := g.Primary
			local, ok := localPathFor(s.cfg.LocalLibraryPath, primary.LibraryPath())
			if !ok {
				continue
			}
			data.TilePaths[primary.ID] = local.Path
			data.TileApps[primary.ID] = openAppsFor(primary.Ext, local.Path)
		}
	}

	// The shared sidebar: counts, colours, the folder tree, saved searches, the last
	// scan. Cached, because the asset page renders the same thing now — see sidebar.go.
	s.applySidebar(r.Context(), &data, opts.Dir)

	// §7's facets describe *this* result set, so a filtered view has to compute its own — but
	// an unfiltered browse is the same for everybody and came with the cached snapshot
	// (applySidebar). That matters because Facets was measured at 55 ms, the most expensive
	// thing left on the request path, and "browsing with no filter" is most page views.
	if isFiltered(opts) {
		if facets, err := s.index.Facets(r.Context(), opts, index.DefaultFacetLimit); err != nil {
			s.log.ErrorContext(r.Context(), "facets failed", "error", err)
		} else {
			data.Facets = facets
		}
	}

	s.render(w, r, "assets.html", http.StatusOK, data)
}

// handleAsset renders one asset, plus its format variants.
//
// §5.1: "The grid shows one entry per group; the detail panel lists variants with
// download links per format."
func (s *Server) handleAsset(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	data := s.newPageData(r)
	data.Workspace = true
	data.Nav = "library"
	data.Asset = &asset
	data.Flash = r.URL.Query().Get("msg")

	// M16: the same sidebar as the grid. Opening an asset used to replace the library
	// navigation with a handful of prose links, so the kinds, colours, tags and folders
	// vanished exactly when you wanted to jump sideways. The current folder follows the
	// asset, so the tree opens where the file lives.
	s.applySidebar(r.Context(), &data, libraryDir(asset.LibraryPath()))

	// Which file the 3D viewer loads: the original, through the companion route, for
	// every format three.js can read directly.
	//
	// M17 stopped preferring the derived preview.glb, which is what made 442 glTF
	// assets open to an empty stage. A .gltf is a JSON file that names its geometry
	// (`.bin`) and its textures as separate files, and the "normalised" glb inherited
	// those names — 1,396 bytes of JSON pointing at a 202 KB buffer that no route
	// serves, because the companion route lives under /assets/{id}/file/ and a relative
	// URI beside preview.glb does not land there.
	//
	// The companion route is the better answer regardless: it resolves the `.bin`, the
	// `.mtl` and the textures, including the pack-wide texture lookup that finds the
	// shared Textures/ directory these packs actually use. Embedding could fix the
	// buffer but not a texture four directories up. preview.glb remains what §6 asks
	// for — a normalised artifact for API consumers — and is now written self-contained
	// (see internal/model), but the viewer no longer depends on it.
	if format := asset.ViewerFormat(); format != "" {
		data.ViewerSrc = asset.ViewerFile()
	}

	// M14 "open in…": the path as the operator's own machine sees it, when they have
	// told us how the library is mounted there.
	if local, ok := localPathFor(s.cfg.LocalLibraryPath, asset.LibraryPath()); ok {
		data.Local = &local
		// The launch links only exist once there is a local path to launch with: the
		// helper takes the path from the URL, so a link without one would do nothing.
		data.OpenApps = openAppsFor(asset.Ext, local.Path)
	}
	data.OpenWith = openWithApps(asset.Ext)
	// The projects already using this asset, which is the Godot integration's real
	// answer to "open in Godot" (§10: the plugin pushes, the page reports).
	if uses, err := s.projects.UsesOfAsset(r.Context(), asset.ID); err != nil {
		s.log.ErrorContext(r.Context(), "loading project uses failed", "error", err)
	} else {
		data.ProjectUses = uses
	}

	// A missing group is not fatal — an asset indexed but not yet grouped still has a
	// usable detail page.
	if group, err := s.index.GroupOf(r.Context(), asset.ID); err == nil {
		data.Group = &group
		if variants, err := s.index.Variants(r.Context(), group.ID); err == nil {
			data.Variants = variants
		}

		// Previous/next in the browse order (M16). The filters travel in the query
		// string, so J and K walk the search you came from; with none, the whole
		// library. Positioned by the group's primary, because that is what the grid
		// ordered — opening a .psd variant should not put you somewhere else in the
		// sequence than opening its .png.
		browse := browseOptions(r)
		if prev, next, err := s.index.Neighbours(r.Context(), browse, group.Primary.Filename, group.ID); err != nil {
			s.log.ErrorContext(r.Context(), "finding neighbours failed", "asset_id", asset.ID, "error", err)
		} else {
			data.PrevAsset, data.NextAsset = prev, next
		}
		data.BrowseQuery = browseQueryString(browse)
	}

	// The licence and source link, editable here (M16). Both are pack-level, which the
	// panel says out loud; a failure loading them only costs the panel.
	if licenses, err := s.prov.Licenses(r.Context()); err != nil {
		s.log.ErrorContext(r.Context(), "listing licences failed", "error", err)
	} else {
		data.Licenses = licenses
	}
	if prov, err := s.prov.Get(r.Context(), asset.PackID); err != nil {
		s.log.ErrorContext(r.Context(), "loading provenance failed", "pack_id", asset.PackID, "error", err)
	} else {
		data.Prov = &prov
	}

	// Tags (§7): direct and inherited. A failure here should not blank the page.
	if ats, err := s.tags.AssetTags(r.Context(), asset.ID); err != nil {
		s.log.ErrorContext(r.Context(), "loading asset tags failed", "asset_id", asset.ID, "error", err)
	} else {
		data.AssetTags = ats
	}

	s.render(w, r, "asset.html", http.StatusOK, data)
}

// handleAssetDownload serves the original bytes.
//
// This is the first handler that reads from the library, so it is where §11's
// serving rules apply:
//
//   - The path is built only from the stored rel_path, never from request input,
//     and still goes through safepath — a rel_path that somehow escaped the root
//     (a bad migration, a hand-edited database) must not be served.
//   - Content-Disposition: attachment, because an .html or .svg served inline from
//     the app origin is stored XSS. The nosniff header from the middleware is the
//     other half of that defence.
//
// ServeContent supplies ETag and Range handling, which §10 needs for the API in
// M8 — "a 200 MB model download that drops should resume, not restart".
func (s *Server) handleAssetDownload(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}

	if asset.Missing() {
		http.Error(w, "This file was not present at the last scan. "+
			"Its index entry is kept, but there is nothing to download.", http.StatusGone)
		return
	}

	absPath, err := safepath.ResolveExisting(s.cfg.LibraryRoot, asset.LibraryPath())
	if err != nil {
		// Either the file vanished since the last scan, or the stored path is not
		// under the root. The first is ordinary; the second is a bug or tampering,
		// so both are logged and neither is described to the client.
		s.log.ErrorContext(r.Context(), "refusing to serve asset",
			"asset_id", asset.ID, "rel_path", asset.LibraryPath(), "error", err)
		if errors.Is(err, safepath.ErrEscapes) || errors.Is(err, safepath.ErrUnsafeInput) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, "This file is no longer readable. Run `ambar scan` to update the index.",
			http.StatusGone)
		return
	}

	file, err := os.Open(absPath)
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not open asset", "asset_id", asset.ID, "error", err)
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		s.log.ErrorContext(r.Context(), "could not stat asset", "asset_id", asset.ID, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// Attachment, always. The filename is quoted and escaped so a name containing
	// a quote or a newline cannot break out of the header.
	w.Header().Set("Content-Disposition",
		mime.FormatMediaType("attachment", map[string]string{"filename": asset.Filename}))
	// An explicit generic type: letting Go sniff one would undermine nosniff.
	w.Header().Set("Content-Type", "application/octet-stream")
	// The content hash is a perfect validator, and the index already has it.
	w.Header().Set("ETag", `"`+asset.SHA256+`"`)
	// Library content is immutable in practice (invariant 1), but a private cache
	// is as far as this should go: the response is behind auth.
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")

	http.ServeContent(w, r, asset.Filename, info.ModTime(), file)
}

// lookupAsset parses the id and fetches the asset, writing the error response
// itself if either fails.
func (s *Server) lookupAsset(w http.ResponseWriter, r *http.Request) (index.Asset, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "not found", http.StatusNotFound)
		return index.Asset{}, false
	}

	asset, err := s.index.Get(r.Context(), id)
	if errors.Is(err, index.ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return index.Asset{}, false
	}
	if err != nil {
		s.log.ErrorContext(r.Context(), "fetching asset failed", "asset_id", id, "error", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return index.Asset{}, false
	}
	return asset, true
}

// isFiltered reports whether these options narrow the library at all.
func isFiltered(opts index.ListOptions) bool {
	return opts.Query != "" || opts.Kind != "" || opts.Dir != "" || opts.PackID != 0 || opts.IncludeMissing
}

// browseOptions reads the filters an asset page was reached with.
//
// The detail page has always been reachable by a bare /assets/{id}, and it still is — this
// only picks up context when the grid passes it along, so previous/next follows the result
// set you were looking at rather than jumping into the whole library.
func browseOptions(r *http.Request) index.ListOptions {
	q := r.URL.Query()
	opts := index.ListOptions{
		Query:          strings.TrimSpace(q.Get("q")),
		Kind:           q.Get("kind"),
		Dir:            q.Get("dir"),
		IncludeMissing: q.Get("missing") == "1",
	}
	if raw := q.Get("pack"); raw != "" {
		if id, err := strconv.ParseInt(raw, 10, 64); err == nil {
			opts.PackID = id
		}
	}
	return opts
}

// browseQueryString renders those filters back, so the previous/next links and the "back to
// results" link carry them too.
func browseQueryString(opts index.ListOptions) string {
	v := url.Values{}
	if opts.Query != "" {
		v.Set("q", opts.Query)
	}
	if opts.Kind != "" {
		v.Set("kind", opts.Kind)
	}
	if opts.Dir != "" {
		v.Set("dir", opts.Dir)
	}
	if opts.PackID != 0 {
		v.Set("pack", strconv.FormatInt(opts.PackID, 10))
	}
	if opts.IncludeMissing {
		v.Set("missing", "1")
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// pageSizes are the choices the grid offers. §8 wants the grid responsive at 20,000
// assets, and MaxPageSize caps anything larger than the top choice here.
var pageSizes = []int{100, 200, 500}

// pageSizeParam reads `per`, falling back to the default rather than erroring.
func pageSizeParam(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return index.DefaultPageSize
	}
	for _, allowed := range pageSizes {
		if n == allowed {
			return n
		}
	}
	return index.DefaultPageSize
}

// pageParam reads `page`. Anything unparseable or below 1 is page 1: a bad page number in
// a URL should show you the library, not an error.
func pageParam(raw string) int {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// pageURL rebuilds the current URL at another page, keeping every filter, the sort and the
// page size. Page 1 drops the parameter so the canonical URL of a search stays clean.
func pageURL(r *http.Request, n int) string {
	q := r.URL.Query()
	q.Del("cursor")
	if n <= 1 {
		q.Del("page")
	} else {
		q.Set("page", strconv.Itoa(n))
	}
	if len(q) == 0 {
		return r.URL.Path
	}
	return r.URL.Path + "?" + q.Encode()
}

// FormatBytes renders a size the way a human reads it. Used by the templates.
func FormatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	value := float64(b)
	units := []string{"KB", "MB", "GB", "TB"}
	var suffix string
	for _, u := range units {
		value /= unit
		suffix = u
		if value < unit {
			break
		}
	}
	if value < 10 {
		return fmt.Sprintf("%.1f %s", value, suffix)
	}
	return fmt.Sprintf("%.0f %s", value, suffix)
}

// libraryDir is the directory part of an asset's path, for display.
func libraryDir(libPath string) string {
	dir := filepath.Dir(libPath)
	if dir == "." {
		return ""
	}
	return dir
}

// derivativeExists reports whether one derived file was produced for this content.
//
// The derivative cache is keyed by content hash (§6), so this is a stat rather than a
// query — and it is the only honest answer to "was a glb actually written", which the
// derive_state column cannot give.
// markModelThumbs asks the filesystem which model tiles actually have a picture.
//
// A model is the one kind whose derive can succeed without producing an image: glTF and
// OBJ normalise to a preview.glb and nothing else, while a browser-rendered thumbnail
// writes the image first and only then records success. `derive_state` cannot tell the
// two apart, so the grid believed 254 model tiles had a thumbnail, rendered an <img> at
// a URL that 404s, and — because the same field also gates the browser renderer — never
// asked anyone to fix it. Blank forever, which is what "listeleniyor ve görüntü yok"
// describes.
//
// One stat per model row on the page, and only for models. §8's rule about aggregates
// belonging in a cache is about queries over the whole library; this is a handful of
// stats against the local data volume, and unlike a cached column it cannot go stale —
// invariant 2, in the small.
func (s *Server) markModelThumbs(groups []index.Group) {
	for i := range groups {
		if !groups[i].Primary.IsModel() {
			continue
		}
		groups[i].Primary.ThumbOnDisk = s.derivativeExists(groups[i].Primary.SHA256, derive.FileThumb)
	}
}

func (s *Server) derivativeExists(sha256hex, name string) bool {
	relDir, err := derive.Dir(sha256hex)
	if err != nil {
		return false
	}
	info, err := os.Stat(filepath.Join(s.cfg.DataRoot, relDir, name))
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

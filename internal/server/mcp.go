package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/datcal/ambar/internal/auth"
	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/handoff"
	"github.com/datcal/ambar/internal/index"
	"github.com/datcal/ambar/internal/mcp"
)

// AI assistants, over MCP.
//
// An assistant working in a game project (Claude Code in the Unreal project, say) asks Ambar
// what the library has and brings assets in, the way a person does with the plugin's
// browser. The tools are the browser's verbs: search, open a folder, look at an asset (its
// facts and its picture), import it. Importing goes through the user's open editor; see
// internal/handoff.
//
// The endpoint sits behind the API token check like /api/v1. The transport's Origin check
// guards servers a web page could reach without credentials; every call here needs a bearer
// token that a page cannot attach, so it is not repeated.

const mcpInstructions = `Ambar is the team's game asset library: models, textures, sprites, audio, fonts, Unreal packages and code from asset packs, on a NAS. Search it before making an asset from scratch.

search_assets finds assets (one row per logical asset; its formats are listed). browse_folder walks the library's folders (vendor/pack/...). get_asset gives one asset's facts: every format with its id, which file Unreal and Godot should take, tags, licence, where it is already used. view_asset shows its picture.

import_into_unreal brings assets into the Unreal project open in the editor, through the editor's Ambar plugin: it picks the format Unreal imports (FBX before glTF), puts it under /Game/Ambar/<library folders> unless told otherwise, records it for the project's credits, and answers with the package path of each — use those paths to place or reference the assets. Unreal packages (.uasset/.umap) are copied into Content where their references resolve.`

// mcpKinds are the kinds an assistant can filter by (library.Kind).
var mcpKinds = []string{"image", "spritesheet", "texture", "model", "audio", "video", "font",
	"script", "material", "hdri", "tilemap", "rig", "other"}

func (s *Server) mcpServer() http.Handler {
	sorts := make([]string, 0, len(index.SortOrders()))
	for _, o := range index.SortOrders() {
		sorts = append(sorts, string(o))
	}
	return &mcp.Server{
		Name: "ambar", Version: s.build.Version, Instructions: mcpInstructions,
		Tools: []mcp.Tool{
			{
				Name: "search_assets", Title: "Search the asset library", ReadOnly: true,
				Description: "Search Ambar's asset library. Matches words in file and pack names, " +
					"plus filters in the query: type:model, ext:fbx,uasset, tris:<5000, dim:32x32, " +
					"width:>=64, has:alpha, has:animation, style:pixel-art, a tag such as theme:pirate, " +
					"and -word to exclude. Returns one line per asset with its id.",
				InputSchema: mcpObject(nil, map[string]any{
					"query": mcpString("What to look for, with any filters. Empty lists everything (in the folder, if one is given)."),
					"kind":  mcpEnum("Only this kind of asset.", mcpKinds),
					"folder": mcpString("Only inside this library folder and below: a library path such as " +
						"'3d/Synty/POLYGON - Spy Kit', a pasted local path, or just a folder's name."),
					"sort":  mcpEnum("Order of the results.", sorts),
					"page":  mcpInteger("1-based page.", 1, 0),
					"limit": mcpInteger("Results per page (default 20).", 1, 50),
				}),
				Call: s.mcpSearch,
			},
			{
				Name: "browse_folder", Title: "List a library folder", ReadOnly: true,
				Description: "List the subfolders of a library folder with how many assets each holds, " +
					"and what kinds of asset the folder has. Without a path, the top of the library.",
				InputSchema: mcpObject(nil, map[string]any{
					"path": mcpString("A library path such as '3d/Synty', a pasted local path, or a folder's name. Empty for the top."),
				}),
				Call: s.mcpBrowse,
			},
			{
				Name: "get_asset", Title: "Describe an asset", ReadOnly: true,
				Description: "Everything about one asset: path, size, dimensions or triangle count, every " +
					"format it comes in (each with its own id), which of them Unreal and Godot should " +
					"import, tags, licence, and the projects already using it.",
				InputSchema: mcpObject([]string{"id"}, map[string]any{
					"id": mcpInteger("The asset's id, from search_assets.", 1, 0),
				}),
				Call: s.mcpGetAsset,
			},
			{
				Name: "view_asset", Title: "Look at an asset", ReadOnly: true,
				Description: "The asset's thumbnail as an image, to check it is the right thing before importing it.",
				InputSchema: mcpObject([]string{"id"}, map[string]any{
					"id":    mcpInteger("The asset's id.", 1, 0),
					"large": map[string]any{"type": "boolean", "description": "The larger preview, where there is one."},
				}),
				Call: s.mcpViewAsset,
			},
			{
				Name: "import_into_unreal", Title: "Import into the open Unreal project",
				Description: "Import assets into the Unreal project open in the editor, through its Ambar " +
					"plugin, and wait for the result: the package path (/Game/...) each one became. " +
					"Picks the format Unreal imports for each asset. Needs the editor open with the " +
					"Ambar plugin connected to this server, and a token with the write scope.",
				InputSchema: mcpObject([]string{"asset_ids"}, map[string]any{
					"asset_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"},
						"minItems": 1, "maxItems": 50, "description": "Ids from search_assets or get_asset."},
					"folder":  mcpString("A content folder to import into instead of /Game/Ambar/<library folders>, e.g. /Game/Props/Pirate."),
					"project": mcpString("The Unreal project to import into, when more than one is open."),
				}),
				Call: s.mcpImport,
			},
			{
				Name: "import_status", Title: "Check on an import", ReadOnly: true,
				Description: "The result of an import_into_unreal request that was still running when it answered.",
				InputSchema: mcpObject([]string{"request_id"}, map[string]any{
					"request_id": mcpInteger("The request id import_into_unreal gave.", 1, 0),
				}),
				Call: s.mcpImportStatus,
			},
		},
	}
}

func mcpObject(required []string, props map[string]any) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

func mcpString(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func mcpEnum(desc string, values []string) map[string]any {
	return map[string]any{"type": "string", "description": desc, "enum": values}
}

// mcpInteger is an integer property; a zero bound is no bound.
func mcpInteger(desc string, lo, hi int) map[string]any {
	m := map[string]any{"type": "integer", "description": desc}
	if lo != 0 {
		m["minimum"] = lo
	}
	if hi != 0 {
		m["maximum"] = hi
	}
	return m
}

func mcpArgs(raw json.RawMessage, v any) error {
	if err := json.Unmarshal(raw, v); err != nil {
		return fmt.Errorf("the arguments do not fit this tool: %v", err)
	}
	return nil
}

// mcpFolder resolves what an assistant called a folder. Empty is the library root.
func (s *Server) mcpFolder(ctx context.Context, raw string) (dir, note string, err error) {
	if strings.TrimSpace(raw) == "" {
		return "", "", nil
	}
	dir, note = s.resolveFolder(ctx, raw)
	if dir == "" {
		if note == "" {
			note = "No folder matches " + raw + "."
		}
		return "", "", errors.New(note + " browse_folder lists the folders.")
	}
	return dir, note, nil
}

func (s *Server) mcpSearch(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct {
		Query, Kind, Folder, Sort string
		Page, Limit               int
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.Kind != "" && !slices.Contains(mcpKinds, a.Kind) {
		return nil, fmt.Errorf("kind must be one of %s", strings.Join(mcpKinds, ", "))
	}
	dir, note, err := s.mcpFolder(ctx, a.Folder)
	if err != nil {
		return nil, err
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 20
	}
	opts := index.ListOptions{
		Query: strings.TrimSpace(a.Query), Kind: a.Kind, Dir: dir,
		Sort: index.ParseSort(a.Sort), Page: max(1, a.Page), Limit: min(limit, 50),
	}
	page, err := s.index.ListGroups(ctx, opts)
	if err != nil {
		s.log.ErrorContext(ctx, "mcp search failed", "error", err)
		return nil, errors.New("the search failed on the server")
	}

	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n")
	}
	where := "the library"
	if dir != "" {
		where = dir
	}
	if page.Total == 0 {
		fmt.Fprintf(&b, "Nothing in %s matches.", where)
		return mcp.Text("%s", b.String()), nil
	}
	fmt.Fprintf(&b, "%d assets in %s match; page %d of %d.\n\n", page.Total, where, page.Page, page.Pages())
	for _, g := range page.Groups {
		b.WriteString(mcpAssetLine(g.Primary, g.Formats) + "\n")
	}
	if page.Page < page.Pages() {
		fmt.Fprintf(&b, "\nNext: page %d.", page.Page+1)
	}
	return mcp.Text("%s", b.String()), nil
}

// mcpAssetLine is one asset in a list: id, name, what it is, where it lives.
func mcpAssetLine(a index.Asset, formats []string) string {
	facts := []string{a.Kind}
	if len(formats) > 1 {
		facts = append(facts, strings.ToUpper(strings.Join(formats, ", ")))
	}
	facts = append(facts, mcpMeasures(a)...)
	return fmt.Sprintf("#%d %s (%s) %s", a.ID, a.Filename, strings.Join(facts, " · "), a.LibraryPath())
}

// mcpMeasures are the numbers that tell one asset from another of its kind.
func mcpMeasures(a index.Asset) []string {
	var out []string
	if a.Width > 0 && a.Height > 0 {
		out = append(out, fmt.Sprintf("%dx%d", a.Width, a.Height))
	}
	if a.TriCount > 0 {
		out = append(out, fmt.Sprintf("%d tris", a.TriCount))
	}
	// As the asset page has it: an animation's frames, or a sprite sheet's grid. Any other
	// image may carry a grid the detector guessed, which is not worth a model's attention.
	switch {
	case a.Animated():
		out = append(out, fmt.Sprintf("%d frames", a.FrameCount))
	case a.Kind == "spritesheet" && a.FrameCount > 1:
		out = append(out, fmt.Sprintf("sheet of %d×%d = %d frames", a.FrameCols, a.FrameRows, a.FrameCount))
	}
	if a.DurationMS > 0 {
		out = append(out, fmt.Sprintf("%.1f s", float64(a.DurationMS)/1000))
	}
	return out
}

func (s *Server) mcpBrowse(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct{ Path string }
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	root := s.folderTree(ctx)
	if root == nil {
		return nil, errors.New("the folder tree is not ready yet; try again in a moment")
	}
	dir, note, err := s.mcpFolder(ctx, a.Path)
	if err != nil {
		return nil, err
	}
	node := root
	if dir != "" {
		if node = root.Find(dir); node == nil {
			return nil, fmt.Errorf("there is no folder %s", dir)
		}
	}

	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n")
	}
	name := node.Path
	if name == "" {
		name = "The library"
	}
	fmt.Fprintf(&b, "%s: %d assets", name, node.Groups)
	if node.DirectGroups > 0 && len(node.Children) > 0 {
		fmt.Fprintf(&b, ", %d of them directly in it", node.DirectGroups)
	}
	b.WriteString(".\n")
	if len(node.Kinds) > 0 {
		kinds := make([]string, 0, len(node.Kinds))
		for k := range node.Kinds {
			kinds = append(kinds, k)
		}
		slices.SortFunc(kinds, func(x, y string) int { return node.Kinds[y] - node.Kinds[x] })
		parts := make([]string, 0, len(kinds))
		for _, k := range kinds {
			parts = append(parts, fmt.Sprintf("%s %d", k, node.Kinds[k]))
		}
		b.WriteString("Kinds: " + strings.Join(parts, ", ") + ".\n")
	}
	if len(node.Children) > 0 {
		b.WriteString("\nFolders:\n")
		const shown = 200
		for i, c := range node.Children {
			if i == shown {
				fmt.Fprintf(&b, "…and %d more.\n", len(node.Children)-shown)
				break
			}
			fmt.Fprintf(&b, "- %s/ (%d)\n", c.Name, c.Groups)
		}
	}
	return mcp.Text("%s", b.String()), nil
}

// mcpAsset loads an asset by id, as a sentence when there is none.
func (s *Server) mcpAsset(ctx context.Context, id int64) (index.Asset, error) {
	asset, err := s.index.Get(ctx, id)
	if errors.Is(err, index.ErrNotFound) {
		return index.Asset{}, fmt.Errorf("there is no asset #%d", id)
	}
	if err != nil {
		s.log.ErrorContext(ctx, "mcp asset lookup failed", "asset_id", id, "error", err)
		return index.Asset{}, errors.New("the lookup failed on the server")
	}
	return asset, nil
}

// mcpVariants is the asset's group: every format it comes in, itself included.
func (s *Server) mcpVariants(ctx context.Context, asset index.Asset) []index.Asset {
	if group, err := s.index.GroupOf(ctx, asset.ID); err == nil {
		if variants, err := s.index.Variants(ctx, group.ID); err == nil && len(variants) > 0 {
			return variants
		}
	}
	return []index.Asset{asset}
}

func (s *Server) mcpGetAsset(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct{ ID int64 }
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	asset, err := s.mcpAsset(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	variants := s.mcpVariants(ctx, asset)

	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", asset.ID, asset.Filename)
	fmt.Fprintf(&b, "Kind: %s. Pack: %s.\n", asset.Kind, asset.PackName)
	fmt.Fprintf(&b, "Library path: %s\n", asset.LibraryPath())
	if local, ok := localPathFor(s.cfg.LocalLibraryPath, asset.LibraryPath()); ok {
		fmt.Fprintf(&b, "On the user's machine: %s\n", local.Path)
	}
	fmt.Fprintf(&b, "Size: %s", FormatBytes(asset.Size))
	if m := mcpMeasures(asset); len(m) > 0 {
		b.WriteString(" · " + strings.Join(m, " · "))
	}
	b.WriteString(".\n")
	if asset.Missing() {
		b.WriteString("The file was missing at the last scan.\n")
	}

	if len(variants) > 1 {
		parts := make([]string, 0, len(variants))
		for _, v := range variants {
			parts = append(parts, fmt.Sprintf("#%d .%s", v.ID, v.Ext))
		}
		b.WriteString("Formats: " + strings.Join(parts, ", ") + ".\n")
	}
	for _, c := range engineChoices(asset.Kind, variants) {
		if c.Asset != nil {
			fmt.Fprintf(&b, "For %s: #%d (.%s).\n", c.Engine, c.Asset.ID, c.Asset.Ext)
		} else {
			fmt.Fprintf(&b, "For %s: %s\n", c.Engine, c.Note)
		}
	}

	if tagsList, err := s.tags.AssetTags(ctx, asset.ID); err == nil && len(tagsList) > 0 {
		names := make([]string, 0, len(tagsList))
		for _, t := range tagsList {
			names = append(names, t.Tag.Canonical())
		}
		b.WriteString("Tags: " + strings.Join(names, ", ") + ".\n")
	}
	if prov, err := s.prov.Get(ctx, asset.PackID); err == nil {
		var parts []string
		if prov.LicenseID != nil {
			if l, ok, _ := s.prov.LicenseByID(ctx, *prov.LicenseID); ok {
				parts = append(parts, "licence "+l.SPDXID)
			}
		}
		if prov.SourceAuthor != "" {
			parts = append(parts, "by "+prov.SourceAuthor)
		}
		if prov.SourceURL != "" {
			parts = append(parts, "from "+prov.SourceURL)
		}
		if len(parts) > 0 {
			b.WriteString("Pack " + strings.Join(parts, ", ") + ".\n")
		}
	}
	if uses, err := s.projects.UsesOfAsset(ctx, asset.ID); err == nil && len(uses) > 0 {
		parts := make([]string, 0, len(uses))
		for _, u := range uses {
			parts = append(parts, u.ProjectName+" ("+u.ResPath+")")
		}
		b.WriteString("Already used in: " + strings.Join(parts, "; ") + ".\n")
	}
	if s.cfg.BaseURL != nil {
		fmt.Fprintf(&b, "Web: %s/assets/%d\n", strings.TrimRight(s.cfg.BaseURL.String(), "/"), asset.ID)
	}
	return mcp.Text("%s", b.String()), nil
}

func (s *Server) mcpViewAsset(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct {
		ID    int64
		Large bool
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	asset, err := s.mcpAsset(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	names := []string{derive.FileThumb2x, derive.FileThumb}
	if a.Large {
		names = append([]string{derive.FilePreview}, names...)
	}
	relDir, err := derive.Dir(asset.SHA256)
	if err != nil {
		return nil, fmt.Errorf("#%d has no picture", asset.ID)
	}
	for _, name := range names {
		f, err := os.Open(filepath.Join(s.cfg.DataRoot, relDir, name))
		if err != nil {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(f, 8<<20))
		f.Close() //nolint:errcheck
		if err != nil || len(data) == 0 {
			continue
		}
		return &mcp.Result{Content: []mcp.Content{
			{Type: "image", Data: base64.StdEncoding.EncodeToString(data), MimeType: "image/webp"},
			{Type: "text", Text: mcpAssetLine(asset, nil)},
		}}, nil
	}
	why := "it has not been rendered yet"
	switch {
	case asset.IsModel():
		why = "a model's picture is drawn by a browser the first time someone sees it in Ambar's web UI"
	case asset.DeriveState == derive.StateFailed || asset.DeriveState == derive.StateUnsupported:
		why = "Ambar cannot make a picture of this file"
	}
	return nil, fmt.Errorf("there is no picture of #%d yet: %s. get_asset describes it", asset.ID, why)
}

func (s *Server) mcpImport(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct {
		AssetIDs []int64 `json:"asset_ids"`
		Folder   string
		Project  string
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return nil, errors.New("not signed in")
	}
	if !auth.TokenHasScope(ctx, auth.ScopeWrite) {
		return nil, errors.New("importing needs an API token with the write scope, and this one can only read. " +
			"The user creates one in Ambar under Settings → API tokens")
	}
	switch {
	case len(a.AssetIDs) == 0:
		return nil, errors.New("asset_ids is empty")
	case len(a.AssetIDs) > 50:
		return nil, errors.New("at most 50 assets per import")
	}
	folder := strings.TrimRight(strings.TrimSpace(a.Folder), "/")
	if folder != "" && folder != "/Game" && !strings.HasPrefix(folder, "/Game/") {
		return nil, errors.New("folder is a content folder under /Game, such as /Game/Props/Pirate")
	}

	project := strings.TrimSpace(a.Project)
	editors := s.handoff.Editors(u.ID, "unreal")
	switch {
	case len(editors) == 0:
		return nil, fmt.Errorf("no Unreal editor is connected for %s. The user opens the project in Unreal "+
			"with the Ambar plugin, and checks that Editor Preferences → Plugins → Ambar has this server and "+
			"an API token, and that \"Accept imports from AI assistants\" is on", u.Username)
	case project != "" && !slices.ContainsFunc(editors, func(e string) bool { return strings.EqualFold(e, project) }):
		return nil, fmt.Errorf("no open Unreal editor has the project %q; open: %s", project, strings.Join(editors, ", "))
	case project == "" && len(editors) > 1:
		return nil, fmt.Errorf("several Unreal projects are open (%s); say which with project", strings.Join(editors, ", "))
	}

	// The format Unreal imports, for each: an asset that ships as OBJ, FBX and BLEND is
	// asked for by whichever id the search showed, and imported as the FBX.
	var send []int64
	var notes []string
	for _, id := range a.AssetIDs {
		asset, err := s.mcpAsset(ctx, id)
		if err != nil {
			notes = append(notes, err.Error()+"; skipped")
			continue
		}
		pick := asset
		skip := false
		for _, c := range engineChoices(asset.Kind, s.mcpVariants(ctx, asset)) {
			if c.Engine != "Unreal" {
				continue
			}
			if c.Asset == nil {
				notes = append(notes, fmt.Sprintf("#%d %s: %s Skipped.", asset.ID, asset.Filename, c.Note))
				skip = true
			} else {
				pick = *c.Asset
			}
		}
		if skip {
			continue
		}
		if pick.ID != asset.ID {
			notes = append(notes, fmt.Sprintf("#%d %s: importing #%d %s, the format Unreal takes.",
				asset.ID, asset.Filename, pick.ID, pick.Filename))
		}
		if !slices.Contains(send, pick.ID) {
			send = append(send, pick.ID)
		}
	}
	if len(send) == 0 {
		return nil, errors.New("nothing to import:\n" + strings.Join(notes, "\n"))
	}

	id := s.handoff.Submit(u.ID, handoff.Request{
		Engine: "unreal", Project: project, Folder: folder, AssetIDs: send,
		Via: "an AI assistant (" + u.Username + ")",
	})
	wait, cancel := context.WithTimeout(ctx, s.importWait)
	defer cancel()
	state, res := s.handoff.Await(wait, u.ID, id)
	return mcpImportReport(id, state, res, notes)
}

func (s *Server) mcpImportStatus(ctx context.Context, raw json.RawMessage) (*mcp.Result, error) {
	var a struct {
		RequestID int64 `json:"request_id"`
	}
	if err := mcpArgs(raw, &a); err != nil {
		return nil, err
	}
	u, ok := auth.UserFromContext(ctx)
	if !ok {
		return nil, errors.New("not signed in")
	}
	state, res := s.handoff.Status(u.ID, a.RequestID)
	return mcpImportReport(a.RequestID, state, res, nil)
}

// mcpImportReport says what became of an import request.
func mcpImportReport(id int64, state handoff.State, res *handoff.Result, notes []string) (*mcp.Result, error) {
	var b strings.Builder
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	switch state {
	case handoff.Done:
		failed := 0
		fmt.Fprintf(&b, "Import request %d, done in %s:\n", id, res.Project)
		for _, it := range res.Items {
			if it.OK {
				fmt.Fprintf(&b, "#%d → %s (%s)\n", it.AssetID, it.Path, it.Message)
			} else {
				failed++
				fmt.Fprintf(&b, "#%d failed: %s\n", it.AssetID, it.Message)
			}
		}
		out := mcp.Text("%s", b.String())
		out.IsError = failed > 0 && failed == len(res.Items)
		return out, nil
	case handoff.Taken:
		fmt.Fprintf(&b, "The editor took import request %d and is still importing (a large model or a slow "+
			"download takes a while). Ask import_status with request_id %d in a minute.", id, id)
		return mcp.Text("%s", b.String()), nil
	case handoff.Waiting:
		return nil, fmt.Errorf("%sno editor took import request %d in time; it stays queued for %d minutes in "+
			"case the editor is busy. Ask import_status with request_id %d", b.String(), id,
			int(handoff.ClaimWithin/time.Minute), id)
	default:
		return nil, fmt.Errorf("%sthere is no import request %d: it finished more than %d minutes ago or was "+
			"never made", b.String(), id, int(handoff.KeepFor/time.Minute))
	}
}

// handleEditorRequests is GET /api/v1/editor/requests?engine=&project=&wait=: an editor's
// long poll for the imports assistants asked of it.
func (s *Server) handleEditorRequests(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	q := r.URL.Query()
	engine := strings.TrimSpace(q.Get("engine"))
	if engine == "" {
		s.apiError(w, http.StatusBadRequest, "engine is required")
		return
	}
	wait := 25 * time.Second
	if n, err := strconv.Atoi(q.Get("wait")); err == nil {
		wait = time.Duration(n) * time.Second
	}
	reqs := s.handoff.Take(r.Context(), u.ID, engine, strings.TrimSpace(q.Get("project")), wait)
	if reqs == nil {
		reqs = []handoff.Request{}
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"requests": reqs})
}

// handleEditorResult is POST /api/v1/editor/requests/{id}/result: the editor's report.
func (s *Server) handleEditorResult(w http.ResponseWriter, r *http.Request) {
	u, ok := auth.UserFromContext(r.Context())
	if !ok {
		s.apiError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		s.apiError(w, http.StatusNotFound, "no such request")
		return
	}
	var res handoff.Result
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&res); err != nil {
		s.apiError(w, http.StatusBadRequest, "invalid result")
		return
	}
	if err := s.handoff.Finish(u.ID, id, res); err != nil {
		s.apiError(w, http.StatusNotFound, "no such request")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

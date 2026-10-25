package server

import (
	"net/http"
	"strings"

	"github.com/datcal/ambar/internal/derive"
	"github.com/datcal/ambar/internal/index"
)

// The library page's right-hand panel: one asset, looked at without leaving the grid.
//
// Opening an asset used to mean a new tab with the full page — right for tagging or
// checking a licence, heavy for "is this the briefcase I want". The panel shows what that
// decision needs: the picture, the formats with a download each, which file each engine
// should take, the path, and where it is already used. The full page is one link away.

// engineChoice is "use it in a game" for one editor: the variant to take, or why there is
// none.
type engineChoice struct {
	Engine string
	// Plugin is where that editor's Ambar plugin lives, because importing through it is
	// what records the use for CREDITS.md and protects the file from removal — a download
	// does neither.
	Plugin string
	Asset  *index.Asset
	Note   string
}

// engineFormats is what each editor imports natively, best first, per kind.
//
// Godot takes glTF before anything else — it is the format its importer is built around —
// and imports FBX (via ufbx, 4.3+) and OBJ. Unreal is the other way round: FBX is its
// native path, glTF goes through Interchange. Unreal has no importer for .ogg or .mp3, which
// is the difference that matters most for audio; the plugin's README says the same.
var engineFormats = map[string]map[string][]string{
	"Godot": {
		"model": {"glb", "gltf", "fbx", "obj", "blend", "dae"},
		"image": {"png", "webp", "svg", "jpg", "jpeg", "tga", "bmp"},
		"audio": {"ogg", "wav", "mp3"},
		"font":  {"ttf", "otf", "woff2", "woff"},
	},
	"Unreal": {
		"model": {"fbx", "glb", "gltf", "obj"},
		"image": {"png", "tga", "psd", "jpg", "jpeg", "bmp", "exr"},
		"audio": {"wav", "flac", "aiff", "aif"},
		"font":  {"ttf", "otf"},
	},
}

var enginePlugins = map[string]string{
	"Godot":  "the Ambar dock",
	"Unreal": "Window › Ambar Asset Library",
}

// engineChoices picks, for Godot and for Unreal, the variant of an asset to use. Nil for
// kinds neither engine imports as such (a Tiled map, a Spriter rig).
func engineChoices(kind string, variants []index.Asset) []engineChoice {
	if _, ok := engineFormats["Godot"][kind]; !ok {
		return nil
	}
	var out []engineChoice
	for _, engine := range []string{"Godot", "Unreal"} {
		choice := engineChoice{Engine: engine, Plugin: enginePlugins[engine]}
	pick:
		for _, ext := range engineFormats[engine][kind] {
			for i := range variants {
				if variants[i].Ext == ext && !variants[i].Missing() {
					choice.Asset = &variants[i]
					break pick
				}
			}
		}
		if choice.Asset == nil {
			var have []string
			for _, v := range variants {
				have = append(have, "."+v.Ext)
			}
			choice.Note = engine + " cannot import " + strings.Join(have, ", ") +
				" — convert it first (for example to ." + engineFormats[engine][kind][0] + ")."
		}
		out = append(out, choice)
	}
	return out
}

// engineOf names the editor a project's resource path belongs to.
func engineOf(resPath string) string {
	switch {
	case strings.HasPrefix(resPath, "res://"):
		return "Godot"
	case strings.HasPrefix(resPath, "/Game/") || strings.HasPrefix(resPath, "/Script/"):
		return "Unreal"
	default:
		return ""
	}
}

// handleAssetPeek is GET /assets/{id}/peek: the panel's contents as a fragment.
func (s *Server) handleAssetPeek(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.lookupAsset(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// For a model, "derived" is not "has a picture" — the filesystem says (see
	// markModelThumbs), exactly as it does for the tile this panel was opened from.
	if asset.IsModel() {
		asset.ThumbOnDisk = s.derivativeExists(asset.SHA256, derive.FileThumb)
	}

	data := s.newPageData(r)
	data.Asset = &asset
	data.BrowseQuery = browseQueryString(browseOptions(r))

	variants := []index.Asset{asset}
	if group, err := s.index.GroupOf(ctx, asset.ID); err == nil {
		data.Group = &group
		if vs, err := s.index.Variants(ctx, group.ID); err == nil && len(vs) > 0 {
			variants = vs
			data.Variants = vs
		}
	}
	data.Engines = engineChoices(asset.Kind, variants)

	if local, ok := localPathFor(s.cfg.LocalLibraryPath, asset.LibraryPath()); ok {
		data.Local = &local
		data.OpenApps = openAppsFor(asset.Ext, local.Path)
	}
	if uses, err := s.projects.UsesOfAsset(ctx, asset.ID); err != nil {
		s.log.ErrorContext(ctx, "loading project uses failed", "asset_id", asset.ID, "error", err)
	} else {
		data.ProjectUses = uses
	}
	data.DirURL = func(path string) string { return libraryLink(nil, "dir", path, "sub", "0") }

	s.renderPartial(w, r, "assets.html", "asset-peek", http.StatusOK, data)
}

package library

import "strings"

// formatFolderTokens is the §5.1 list of path segments that indicate a format or
// variant folder rather than a meaningful part of an asset's identity.
//
// These exist because vendors like CraftPix ship the same artwork several times
// over, split by format into sibling folders:
//
//	craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/
//	    PNG/Plant1/idle.png
//	    PSD/Plant1/idle.psd
//	    ASEPRITE/Plant1/idle.aseprite
//
// Three consumers, which is why this is its own file from the start:
//
//   - M1 pack detection: a directory whose children are all format folders is a
//     pack, not an organisational parent (§5.1).
//   - M2 asset grouping: the group key is the relative path with the format-folder
//     segment removed, so the grid shows one entry instead of three (§5.1).
//   - M3 auto-tagging: path segments become tags, and these must be skipped or
//     the library fills with a `png` tag on everything (§7).
var formatFolderTokens = []string{
	// Raster and editable 2D
	"png", "psd", "aseprite", "ase", "kra", "xcf",
	// Vector
	"svg", "eps", "ai", "vector",
	// Tiled
	"tiled_files", "tmx",
	// 3D
	"fbx", "obj", "gltf", "glb", "blend",
	// Generic source buckets
	"source", "sources", "sourcefiles",
}

// IsFormatFolder reports whether a single path segment names a format or variant
// folder.
//
// Matching is case-insensitive on the whole segment, plus the prefix form §5.1
// calls out: a known token followed by `_` or `-`, which covers the real
// `PNG_Animations` and `PNG_Parts&Spriter_Animation` directories.
//
// The separator requirement is what keeps this from over-matching. Without it,
// "Objects" would match the "obj" token and a perfectly ordinary folder would be
// treated as a format variant — which in M2 would collapse unrelated assets into
// one group.
func IsFormatFolder(segment string) bool {
	s := strings.ToLower(strings.TrimSpace(segment))
	if s == "" {
		return false
	}
	for _, token := range formatFolderTokens {
		if s == token {
			return true
		}
		if len(s) > len(token)+1 && strings.HasPrefix(s, token) {
			switch s[len(token)] {
			case '_', '-':
				return true
			}
		}
	}
	return false
}

// StripFormatFolders removes format-folder segments from a slash-separated
// relative path.
//
// This is the §5.1 asset-group key, minus the extension handling. It is not used
// by M1 — grouping is M2 — but it belongs beside IsFormatFolder, and having it
// tested now means M2 inherits a verified building block rather than writing the
// same logic again.
func StripFormatFolders(relPath string) string {
	parts := strings.Split(relPath, "/")
	kept := make([]string, 0, len(parts))
	for i, p := range parts {
		// Never strip the final segment: it is the filename, and a file legitimately
		// named "PNG" or "source.png" must survive.
		if i < len(parts)-1 && IsFormatFolder(p) {
			continue
		}
		kept = append(kept, p)
	}
	return strings.Join(kept, "/")
}

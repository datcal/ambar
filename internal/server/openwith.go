package server

import (
	"net/url"
	"strings"
)

// "Open in Aseprite / Blender / your file manager" (M14), and the honest version of
// it.
//
// A browser cannot launch an application on the machine you are sitting at, and this
// server normally runs on the NAS — so a server-side "open in Aseprite" would launch
// Aseprite *on the NAS*, headless, which is worse than useless. Rather than pretend,
// Ambar hands over the one thing that does work everywhere: the path to the file as
// your own machine sees it, ready to paste into an application's Open dialog or a
// file manager.
//
// AMBAR_LOCAL_LIBRARY_PATH is that mapping. Unset, the whole section disappears
// instead of showing a path that would not resolve.

// LocalPath is a library file as the operator's own machine sees it.
type LocalPath struct {
	// Path is what to paste into an Open dialog: an SMB URL, a UNC path, or a mount
	// point, matching however AMBAR_LOCAL_LIBRARY_PATH is written.
	Path string
	// URL is a clickable link when the form allows one (smb:// or file://), or empty.
	// Browsers deliberately restrict these, so it is offered as a convenience next to
	// the copyable text rather than as the primary control.
	URL string
	// Windows records which separator style was used, so the UI can say what it is
	// showing rather than leaving the user to guess.
	Windows bool
}

// localPathFor maps a library-relative path onto the operator's machine.
//
// The template's own shape decides the separator: a UNC path or a drive letter gets
// backslashes, everything else gets forward slashes. That avoids asking the browser
// what operating system it is on — which it lies about — and it means the operator
// sees exactly the form they configured.
func localPathFor(template, libraryRelPath string) (LocalPath, bool) {
	template = strings.TrimSpace(template)
	if template == "" || libraryRelPath == "" {
		return LocalPath{}, false
	}

	rel := strings.Trim(strings.ReplaceAll(libraryRelPath, "\\", "/"), "/")
	if rel == "" {
		return LocalPath{}, false
	}

	windows := isWindowsStyle(template)
	base := strings.TrimRight(template, "/\\")

	var out LocalPath
	out.Windows = windows
	if windows {
		out.Path = base + "\\" + strings.ReplaceAll(rel, "/", "\\")
	} else {
		out.Path = base + "/" + rel
	}

	// A clickable link where the form supports one. Each segment is escaped so a name
	// with a space or a hash still resolves; the scheme prefix is left alone.
	switch {
	case strings.HasPrefix(strings.ToLower(base), "smb://"),
		strings.HasPrefix(strings.ToLower(base), "file://"),
		strings.HasPrefix(strings.ToLower(base), "afp://"):
		out.URL = base + "/" + escapePathSegments(rel)
	case strings.HasPrefix(base, "/"):
		// A local mount point: file:// is the only form a browser might follow.
		out.URL = "file://" + base + "/" + escapePathSegments(rel)
	}
	return out, true
}

// isWindowsStyle reports whether the configured template is a Windows path: a UNC
// share (\\nas\share) or a drive letter (Z:\...).
func isWindowsStyle(template string) bool {
	if strings.HasPrefix(template, `\\`) {
		return true
	}
	if len(template) >= 2 && template[1] == ':' {
		c := template[0]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') {
			return true
		}
	}
	return false
}

// escapePathSegments percent-escapes each segment of a slash path, leaving the
// separators intact.
func escapePathSegments(rel string) string {
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// openWithApps names the applications worth suggesting for an extension, so the
// panel can say "Aseprite" rather than a generic "your editor". Suggestions only:
// nothing here launches anything.
func openWithApps(ext string) []string {
	switch strings.ToLower(ext) {
	case "aseprite", "ase":
		return []string{"Aseprite"}
	case "psd":
		return []string{"Photoshop", "Krita", "GIMP"}
	case "kra":
		return []string{"Krita"}
	case "xcf":
		return []string{"GIMP"}
	case "png", "jpg", "jpeg", "webp", "bmp", "tga", "gif":
		return []string{"Aseprite", "Krita", "your image editor"}
	case "blend":
		return []string{"Blender"}
	case "obj", "fbx", "gltf", "glb":
		return []string{"Blender", "Godot"}
	case "wav", "ogg", "mp3", "flac":
		return []string{"Audacity", "your audio editor"}
	case "tmx", "tsx":
		return []string{"Tiled"}
	case "scml":
		return []string{"Spriter"}
	default:
		return nil
	}
}

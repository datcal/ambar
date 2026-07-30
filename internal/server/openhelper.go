package server

import (
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"strings"
)

// "Open in Aseprite" as a link that actually opens Aseprite (M15).
//
// The previous version of this feature handed over a path to copy, which is honest but
// is not what a button should do. The mechanism that does work is the one behind
// `tel:` and `vscode://`: a URL scheme registered with the operating system. Neither
// Aseprite, Blender nor Godot ships one — so Ambar registers its own, `ambar://`, and
// a small helper on the operator's machine turns it into a launch.
//
//	ambar://open?app=aseprite&path=<the local path>
//
// The helper is generated per platform and downloaded from the settings page. Until it
// is installed the link is inert, which is why the copyable path stays: a feature that
// needs a one-time install must degrade to something that works immediately.
//
// Nothing about this runs on the server. Ambar never executes an application — it
// could only ever launch one on the NAS, which is the whole reason this indirection
// exists.

// openApp is an application the UI offers to launch.
type openApp struct {
	// Key is what the helper matches on.
	Key string
	// Label is what the button says.
	Label string
	// URL is the ambar:// link.
	//
	// template.URL rather than string because html/template only trusts a known set of
	// schemes in an href and rewrites anything else to "#ZgotmplZ" — sensible default,
	// wrong answer for a scheme we define ourselves. The value is safe to trust: it is
	// assembled here from url.QueryEscape'd parts, never from raw input.
	URL template.URL
}

// appsForExt maps an extension to the applications worth offering, in order.
// Suggestions, not a registry: the helper decides what "aseprite" means on that
// machine, and an app the operator does not have simply never gets clicked.
var appsForExt = map[string][]struct{ key, label string }{
	"aseprite": {{"aseprite", "Aseprite"}},
	"ase":      {{"aseprite", "Aseprite"}},
	"png":      {{"aseprite", "Aseprite"}, {"editor", "Image editor"}},
	"jpg":      {{"editor", "Image editor"}},
	"jpeg":     {{"editor", "Image editor"}},
	"webp":     {{"editor", "Image editor"}},
	"gif":      {{"aseprite", "Aseprite"}, {"editor", "Image editor"}},
	"psd":      {{"editor", "Image editor"}},
	"kra":      {{"krita", "Krita"}},
	"xcf":      {{"gimp", "GIMP"}},
	"blend":    {{"blender", "Blender"}},
	"obj":      {{"blender", "Blender"}, {"godot", "Godot"}},
	"fbx":      {{"blender", "Blender"}, {"godot", "Godot"}},
	"gltf":     {{"blender", "Blender"}, {"godot", "Godot"}},
	"glb":      {{"blender", "Blender"}, {"godot", "Godot"}},
	"wav":      {{"audio", "Audio editor"}},
	"ogg":      {{"audio", "Audio editor"}},
	"mp3":      {{"audio", "Audio editor"}},
	"flac":     {{"audio", "Audio editor"}},
	"tmx":      {{"tiled", "Tiled"}},
	"tsx":      {{"tiled", "Tiled"}},
	"ttf":      {{"fonts", "Font viewer"}},
	"otf":      {{"fonts", "Font viewer"}},
}

// openAppsFor builds the launch links for one asset's local path.
//
// localPath is the path as the operator's machine sees it — the same string the copy
// button offers. It travels in the URL rather than the asset id so the helper needs no
// network access, no credentials and no knowledge of Ambar at all: everything it needs
// is in the link.
func openAppsFor(ext, localPath string) []openApp {
	if localPath == "" {
		return nil
	}
	entries := appsForExt[strings.ToLower(ext)]
	if len(entries) == 0 {
		// Still worth offering: the helper's "reveal" action opens the containing
		// folder, which is useful for anything.
		entries = []struct{ key, label string }{{"reveal", "File manager"}}
	} else {
		entries = append(entries, struct{ key, label string }{"reveal", "File manager"})
	}

	out := make([]openApp, 0, len(entries))
	for _, e := range entries {
		out = append(out, openApp{
			Key:   e.key,
			Label: e.label,
			URL: template.URL("ambar://open?app=" + url.QueryEscape(e.key) +
				"&path=" + url.QueryEscape(localPath)),
		})
	}
	return out
}

// handleOpenHelper serves the helper script for a platform (M15).
//
// It is generated rather than vendored so it can carry the current scheme name and a
// comment explaining what it does — a script a person is about to register as a
// protocol handler should be readable in full before they run it, which is the same
// reasoning as §9.1's exported removal script.
func (s *Server) handleOpenHelper(w http.ResponseWriter, r *http.Request) {
	platform := r.URL.Query().Get("platform")

	var body, filename string
	switch platform {
	case "linux":
		body, filename = linuxHelper(), "ambar-open-linux.sh"
	case "macos":
		body, filename = macHelper(), "ambar-open-macos.sh"
	case "windows":
		body, filename = windowsHelper(), "ambar-open-windows.ps1"
	default:
		http.Error(w, "unknown platform; use linux, macos or windows", http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(body))
}

// The helpers below share one shape: parse `ambar://open?app=…&path=…`, map the app
// key to a command, and launch it with the path. They are deliberately small and
// dependency-free, because the operator has to read them before trusting them.

func linuxHelper() string {
	return `#!/bin/sh
# ambar-open — turn an ambar:// link into a launched application.
#
# Ambar runs on a server, so it cannot open Aseprite on your machine. This script
# can: it registers itself as the handler for the ambar:// scheme, and Ambar's
# "Open in…" buttons then work like any other application link.
#
# Install:
#   1. Put this file somewhere permanent and make it executable:
#        install -Dm755 ambar-open-linux.sh ~/.local/bin/ambar-open
#   2. Register it:
#        ~/.local/bin/ambar-open --install
#
# Uninstall: remove ~/.local/share/applications/ambar-open.desktop and re-run
#   update-desktop-database ~/.local/share/applications
#
# Edit the case block below to point each app key at the command you actually use.

set -eu

self=$(readlink -f "$0")

if [ "${1:-}" = "--install" ]; then
    dir="$HOME/.local/share/applications"
    mkdir -p "$dir"
    cat > "$dir/ambar-open.desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Ambar open handler
Exec=$self %u
NoDisplay=true
MimeType=x-scheme-handler/ambar;
DESKTOP
    update-desktop-database "$dir" 2>/dev/null || true
    xdg-mime default ambar-open.desktop x-scheme-handler/ambar 2>/dev/null || true
    echo "registered: ambar:// links now run $self"
    exit 0
fi

url=${1:-}
[ -n "$url" ] || { echo "usage: ambar-open ambar://open?app=<key>&path=<path>" >&2; exit 2; }

# Pull the query apart without depending on python or jq.
query=${url#*\?}
app=""
path=""
IFS='&'
for pair in $query; do
    key=${pair%%=*}
    value=${pair#*=}
    # Percent-decoding, the portable way.
    value=$(printf '%b' "$(printf '%s' "$value" | sed 's/+/ /g; s/%\(..\)/\\x\1/g')")
    case $key in
        app) app=$value ;;
        path) path=$value ;;
    esac
done
unset IFS

[ -n "$path" ] || { echo "no path in the link" >&2; exit 2; }

# An smb:// URL is not a path any application can open. Mount it once (in your file
# manager or with gio mount) and point AMBAR_LOCAL_LIBRARY_PATH at the mount point
# instead — then these links carry a real path.
case $path in
    smb://*|afp://*)
        echo "this link carries a network URL rather than a mounted path: $path" >&2
        echo "set AMBAR_LOCAL_LIBRARY_PATH to the mount point to make it launchable" >&2
        exit 3
        ;;
esac

case $app in
    aseprite) exec aseprite "$path" ;;
    blender)  exec blender "$path" ;;
    godot)    exec godot "$path" ;;
    krita)    exec krita "$path" ;;
    gimp)     exec gimp "$path" ;;
    audio)    exec audacity "$path" ;;
    tiled)    exec tiled "$path" ;;
    editor)   exec xdg-open "$path" ;;
    fonts)    exec xdg-open "$path" ;;
    reveal)   exec xdg-open "$(dirname "$path")" ;;
    *)        exec xdg-open "$path" ;;
esac
`
}

func macHelper() string {
	return `#!/bin/sh
# ambar-open — turn an ambar:// link into a launched application (macOS).
#
# macOS registers URL schemes from an application bundle's Info.plist, so this script
# is the launcher and the bundle is a three-line wrapper around it.
#
# Install:
#   1. mkdir -p ~/Applications/AmbarOpen.app/Contents/MacOS
#   2. cp ambar-open-macos.sh ~/Applications/AmbarOpen.app/Contents/MacOS/AmbarOpen
#      chmod +x ~/Applications/AmbarOpen.app/Contents/MacOS/AmbarOpen
#   3. Save this as ~/Applications/AmbarOpen.app/Contents/Info.plist:
#
#      <?xml version="1.0" encoding="UTF-8"?>
#      <!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
#        "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
#      <plist version="1.0"><dict>
#        <key>CFBundleIdentifier</key><string>net.ambar.open</string>
#        <key>CFBundleName</key><string>AmbarOpen</string>
#        <key>CFBundleExecutable</key><string>AmbarOpen</string>
#        <key>CFBundleURLTypes</key><array><dict>
#          <key>CFBundleURLName</key><string>Ambar</string>
#          <key>CFBundleURLSchemes</key><array><string>ambar</string></array>
#        </dict></array>
#      </dict></plist>
#
#   4. open ~/Applications/AmbarOpen.app   (once, so Launch Services notices it)

set -eu

url=${1:-}
[ -n "$url" ] || exit 2

query=${url#*\?}
app=""
path=""
IFS='&'
for pair in $query; do
    key=${pair%%=*}
    value=${pair#*=}
    value=$(printf '%b' "$(printf '%s' "$value" | sed 's/+/ /g; s/%\(..\)/\\x\1/g')")
    case $key in
        app) app=$value ;;
        path) path=$value ;;
    esac
done
unset IFS

[ -n "$path" ] || exit 2

# An smb:// URL is not a path an application can open. Mount the share in Finder and
# point AMBAR_LOCAL_LIBRARY_PATH at the /Volumes mount point instead.
case $path in
    smb://*|afp://*)
        echo "this link carries a network URL rather than a mounted path: $path" >&2
        echo "set AMBAR_LOCAL_LIBRARY_PATH to the /Volumes mount point instead" >&2
        exit 3
        ;;
esac

case $app in
    aseprite) exec open -a Aseprite "$path" ;;
    blender)  exec open -a Blender "$path" ;;
    godot)    exec open -a Godot "$path" ;;
    krita)    exec open -a Krita "$path" ;;
    gimp)     exec open -a GIMP "$path" ;;
    audio)    exec open -a Audacity "$path" ;;
    tiled)    exec open -a Tiled "$path" ;;
    reveal)   exec open -R "$path" ;;
    *)        exec open "$path" ;;
esac
`
}

func windowsHelper() string {
	return `# ambar-open.ps1 — turn an ambar:// link into a launched application (Windows).
#
# Install (per user, no administrator rights needed):
#   1. Put this file somewhere permanent, e.g. %LOCALAPPDATA%\Ambar\ambar-open.ps1
#   2. From PowerShell, in that directory:
#        .\ambar-open.ps1 -Install
#
# Uninstall: remove HKCU:\Software\Classes\ambar
#
# Edit $Apps below to point each key at the executable you actually use.

param(
    [switch]$Install,
    [Parameter(ValueFromRemainingArguments = $true)]
    [string[]]$Url
)

$Apps = @{
    aseprite = 'aseprite.exe'
    blender  = 'blender.exe'
    godot    = 'godot.exe'
    krita    = 'krita.exe'
    gimp     = 'gimp-2.10.exe'
    audio    = 'audacity.exe'
    tiled    = 'tiled.exe'
}

if ($Install) {
    $self = $MyInvocation.MyCommand.Path
    $key = 'HKCU:\Software\Classes\ambar'
    New-Item -Path $key -Force | Out-Null
    Set-ItemProperty -Path $key -Name '(Default)' -Value 'URL:Ambar open handler'
    Set-ItemProperty -Path $key -Name 'URL Protocol' -Value ''
    New-Item -Path "$key\shell\open\command" -Force | Out-Null
    $command = 'powershell.exe -NoProfile -ExecutionPolicy Bypass -File "' + $self + '" "%1"'
    Set-ItemProperty -Path "$key\shell\open\command" -Name '(Default)' -Value $command
    Write-Host "registered: ambar:// links now run $self"
    exit 0
}

if (-not $Url) { exit 2 }

$parsed = [System.Uri]::new($Url[0])
$query = [System.Web.HttpUtility]::ParseQueryString($parsed.Query)
$app = $query['app']
$path = $query['path']
if (-not $path) { exit 2 }

# A UNC path works as-is; an smb:// URL does not. Map the share to a drive letter and
# point AMBAR_LOCAL_LIBRARY_PATH at that instead.
if ($path -like 'smb://*') {
    Write-Error "this link carries an smb:// URL; set AMBAR_LOCAL_LIBRARY_PATH to a UNC path or a mapped drive"
    exit 3
}

if ($app -eq 'reveal') {
    # Built by concatenation rather than an escaped quote, so this file stays readable
    # in every editor and quoting style.
    $arg = '/select,"' + $path + '"'
    Start-Process explorer.exe $arg
    exit 0
}

$exe = $Apps[$app]
if ($exe) {
    Start-Process $exe -ArgumentList $path
} else {
    Start-Process $path
}
`
}

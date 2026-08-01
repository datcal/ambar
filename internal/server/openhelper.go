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
	// Key is what the helper matches on, and what the button's styling is keyed on.
	Key string
	// Label is what the button says.
	Label string
	// Short is the two-or-three character mark shown on a grid tile, where there is no room
	// for a word. Not the vendor's logo: shipping those would mean either an external request
	// (which §11's CSP forbids) or redistributing a trademark. The app's own colour plus its
	// initials is recognisable at 20px and is honestly ours.
	Short string
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

// appShort is the mark each app gets on a grid tile. Two or three characters, because the tile
// is a picture and the buttons are not the point of it.
var appShort = map[string]string{
	"aseprite": "Ase",
	"blender":  "Bl",
	"godot":    "Gd",
	"krita":    "Kr",
	"gimp":     "Gi",
	"audio":    "Au",
	"tiled":    "Ti",
	"editor":   "Img",
	"fonts":    "Aa",
	"reveal":   "⤢",
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
			Short: appShort[e.key],
			URL: template.URL("ambar://open?app=" + queryEscapeStrict(e.key) +
				"&path=" + queryEscapeStrict(localPath)),
		})
	}
	return out
}

// queryEscapeStrict percent-escapes for a query string with spaces as %20 rather than "+".
//
// url.QueryEscape produces "+", which is correct for form encoding and wrong here in a way that
// only shows up on real filenames: the library holds directories like "2 Objects", so the link
// carried "2+Objects", and the helper's decoder turned every "+" back into a space — which means
// a file genuinely named "sprite+outline.png" opened as "sprite outline.png" and failed. %20 is
// unambiguous in both directions.
//
// html/template also escapes "+" to "&#43;" inside an href, which is harmless but made the link
// unreadable when debugging exactly this.
func queryEscapeStrict(s string) string {
	return strings.ReplaceAll(url.QueryEscape(s), "+", "%20")
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
# Ambar runs on a server, so it cannot open Aseprite on your machine. This script can: it
# registers itself as the handler for the ambar:// scheme, and Ambar's "Open in…" buttons then
# work like any other application link.
#
#   Install:   sh ambar-open-linux.sh --install
#   Check:     ambar-open --check
#   Try one:   ambar-open --test 'ambar://open?app=aseprite&path=/mnt/game-assets/x.png'
#
# --install copies this script to ~/.local/bin/ambar-open and registers *that* copy, so you can
# delete the download afterwards. Then it verifies the registration and prints what it found.
#
# If a launch opens your application with no file in it, the scheme is not registered and your
# browser asked you to pick an application instead — so it handed the raw ambar:// URL to that
# application, which cannot open it. --check is how you tell the difference.
#
# Apps are resolved in this order: a command on PATH, then a Flatpak, then a Steam install,
# then xdg-open. Steam is in that list because an application bought there has no command on
# PATH and its .desktop entry is "steam steam://rungameid/431730", which carries no file — so
# the fallback opened the editor with nothing in it. Override any of them in
# ~/.config/ambar-open.conf, which survives re-downloading this script:
#
#   ASEPRITE_CMD="flatpak run org.aseprite.Aseprite"
#   BLENDER_CMD="/opt/blender/blender"

set -eu

self=$(readlink -f "$0")
target="$HOME/.local/bin/ambar-open"
desktop="$HOME/.local/share/applications/ambar-open.desktop"

# --- installation ------------------------------------------------------------------

if [ "${1:-}" = "--install" ]; then
    mkdir -p "$(dirname "$target")" "$(dirname "$desktop")"
    if [ "$self" != "$target" ]; then
        cp "$self" "$target"
        chmod 755 "$target"
        echo "installed: $target"
    fi

    cat > "$desktop" <<DESKTOP
[Desktop Entry]
Type=Application
Name=Ambar open handler
Exec=$target %u
NoDisplay=true
Terminal=false
MimeType=x-scheme-handler/ambar;
DESKTOP

    update-desktop-database "$(dirname "$desktop")" 2>/dev/null || true
    xdg-mime default ambar-open.desktop x-scheme-handler/ambar 2>/dev/null || true
    exec "$target" --check
fi

# --- resolving applications ---------------------------------------------------------

# shellcheck source=/dev/null
[ -f "$HOME/.config/ambar-open.conf" ] && . "$HOME/.config/ambar-open.conf"

# steam_libraries lists every Steam library root on this machine, one per line.
#
# Steam installs to more than one place: the default under $HOME, and any extra library the
# user added on another drive. The extras are listed in libraryfolders.vdf as "path" entries,
# which is a Valve text format but a regular enough one to read with sed for this single key.
steam_libraries() {
    for base in "$HOME/.local/share/Steam" "$HOME/.steam/steam" \
        "$HOME/.var/app/com.valvesoftware.Steam/.local/share/Steam"; do
        [ -d "$base/steamapps/common" ] && echo "$base"
        vdf="$base/steamapps/libraryfolders.vdf"
        [ -f "$vdf" ] && sed -n 's/^[[:space:]]*"path"[[:space:]]*"\(.*\)".*/\1/p' "$vdf"
    done
}

# steam_binary echoes the first executable matching an install directory and a binary glob.
#
# Why this exists: an application bought on Steam has no command on PATH and no Flatpak, and
# its .desktop entry is Steam's own —
#
#     Exec=steam steam://rungameid/431730
#
# — which takes no file argument at all. So the xdg-open fallback launched Aseprite with
# nothing in it, every time, and looked exactly like a broken link. Aseprite itself has a
# perfectly good CLI -- "aseprite [OPTIONS] [FILES]..." -- it was never handed the file.
steam_binary() {
    dir=$1
    glob=$2
    steam_libraries | while IFS= read -r lib; do
        for candidate in "$lib/steamapps/common/$dir/"$glob; do
            [ -f "$candidate" ] && [ -x "$candidate" ] && printf '%s\n' "$candidate"
        done
    done | head -1
}

# resolve echoes the command for an app key: an override, a binary on PATH, a Flatpak, a
# Steam install, or empty. In that order, because an explicit override beats a guess and a
# packaged binary beats a game-store copy.
resolve() {
    override=$1
    binary=$2
    flatpak_id=$3
    steam_dir=$4
    steam_glob=$5

    if [ -n "$override" ]; then
        echo "$override"
        return
    fi
    if command -v "$binary" >/dev/null 2>&1; then
        echo "$binary"
        return
    fi
    if command -v flatpak >/dev/null 2>&1 && [ -n "$flatpak_id" ] &&
        flatpak info "$flatpak_id" >/dev/null 2>&1; then
        echo "flatpak run $flatpak_id"
        return
    fi
    if [ -n "$steam_dir" ]; then
        found=$(steam_binary "$steam_dir" "$steam_glob")
        if [ -n "$found" ]; then
            echo "$found"
            return
        fi
    fi
    echo ""
}

command_for() {
    case $1 in
        aseprite) resolve "${ASEPRITE_CMD:-}" aseprite org.aseprite.Aseprite "Aseprite" "aseprite" ;;
        blender)  resolve "${BLENDER_CMD:-}"  blender  org.blender.Blender  "Blender" "blender" ;;
        # Steam ships Godot as the raw export-template-style binary, whose name carries the
        # platform and build ("godot.x11.opt.tools.64"), so this matches on the prefix.
        godot)    resolve "${GODOT_CMD:-}"    godot    org.godotengine.Godot "Godot Engine" "godot*" ;;
        krita)    resolve "${KRITA_CMD:-}"    krita    org.kde.krita       "Krita" "bin/krita" ;;
        gimp)     resolve "${GIMP_CMD:-}"     gimp     org.gimp.GIMP       "" "" ;;
        audio)    resolve "${AUDIO_CMD:-}"    audacity org.audacityteam.Audacity "" "" ;;
        tiled)    resolve "${TILED_CMD:-}"    tiled    org.mapeditor.Tiled "Tiled" "tiled" ;;
        *)        echo "" ;;
    esac
}

# --check reports whether the registration actually took, and what each application resolves to.
#
# Two separate failures look identical from a browser: nothing is registered, or something else
# is. A third looks identical from the *desktop*: the scheme works, the script runs, and the
# application opens with nothing in it — which is what a Steam-installed editor did before
# resolve() learned to look there, because xdg-open honours a .desktop whose Exec is
# "steam steam://rungameid/431730" and that carries no file. So the report names the command it
# would actually run for each app; a wrong or missing one is then a line you can read rather
# than a mystery.
if [ "${1:-}" = "--check" ]; then
    status=0
    [ -x "$target" ] || { echo "missing: $target is not installed or not executable" >&2; status=1; }
    [ -f "$desktop" ] || { echo "missing: $desktop" >&2; status=1; }

    handler=$(xdg-mime query default x-scheme-handler/ambar 2>/dev/null || true)
    case $handler in
        ambar-open.desktop) echo "ok: ambar:// is handled by $target" ;;
        "")  echo "not registered: nothing handles ambar:// yet" >&2; status=1 ;;
        *)   echo "registered to something else: $handler" >&2; status=1 ;;
    esac

    echo
    echo "applications:"
    for app_key in aseprite blender godot krita gimp audio tiled; do
        found=$(command_for "$app_key")
        if [ -n "$found" ]; then
            printf '  %-9s %s\n' "$app_key" "$found"
        else
            upper_key=$(printf '%s' "$app_key" | tr '[:lower:]' '[:upper:]')
            printf '  %-9s not found — xdg-open will guess; set %s_CMD to choose\n' \
                "$app_key" "$upper_key"
        fi
    done

    # Firefox keeps its own handler list and only consults it after the first prompt; if it has
    # already remembered a wrong choice, this is where that gets fixed.
    echo
    echo "if your browser still asks which application to use, clear ambar in its"
    echo "settings (Firefox: Settings → General → Applications) and click a link again"
    exit $status
fi

# --- the link ----------------------------------------------------------------------

dry_run=""
if [ "${1:-}" = "--test" ]; then
    dry_run=1
    shift
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
    # %XX only: Ambar encodes spaces as %20, so a literal "+" in a filename stays a "+".
    value=$(printf '%b' "$(printf '%s' "$value" | sed 's/%\(..\)/\\x\1/g')")
    case $key in
        app) app=$value ;;
        path) path=$value ;;
    esac
done
unset IFS

[ -n "$path" ] || {
    echo "no path in the link: $url" >&2
    echo "if your application opened with nothing in it, it was handed this URL directly" >&2
    echo "instead of the file — run: ambar-open --check" >&2
    exit 2
}

# An smb:// URL is not a path any application can open. Mount it once (in your file manager or
# with gio mount) and point AMBAR_LOCAL_LIBRARY_PATH at the mount point instead — then these
# links carry a real path.
case $path in
    smb://*|afp://*)
        echo "this link carries a network URL rather than a mounted path: $path" >&2
        echo "set AMBAR_LOCAL_LIBRARY_PATH to the mount point to make it launchable" >&2
        exit 3
        ;;
esac

if [ "$app" = "reveal" ]; then
    target_path=$(dirname "$path")
    [ -n "$dry_run" ] && { echo "would run: xdg-open $target_path"; exit 0; }
    exec xdg-open "$target_path"
fi

# The file has to be there. Without this the launch "works" and the application opens empty,
# which is indistinguishable from the scheme not being registered — the one confusion this
# script exists to remove.
[ -e "$path" ] || {
    echo "not found on this machine: $path" >&2
    echo "AMBAR_LOCAL_LIBRARY_PATH describes how the library is mounted here; check it" >&2
    exit 4
}

cmd=$(command_for "$app")
if [ -z "$cmd" ]; then
    # The override variables are upper case, so name the right one — telling somebody to set
    # "aseprite_CMD" when the script reads ASEPRITE_CMD is worse than saying nothing.
    upper=$(printf '%s' "$app" | tr '[:lower:]' '[:upper:]')
    echo "no command found for '$app'; falling back to xdg-open" >&2
    echo "set ${upper}_CMD in ~/.config/ambar-open.conf to choose one" >&2
    [ -n "$dry_run" ] && { echo "would run: xdg-open $path"; exit 0; }
    exec xdg-open "$path"
fi

[ -n "$dry_run" ] && { echo "would run: $cmd $path"; exit 0; }

# A resolved absolute path is executed quoted, because Steam's install directories contain
# spaces — "Godot Engine" would otherwise word-split into two arguments and launch nothing.
if [ -x "$cmd" ]; then
    exec "$cmd" "$path"
fi

# Unquoted on purpose: an override may be a command *with* arguments ("flatpak run org.x.Y").
# shellcheck disable=SC2086
exec $cmd "$path"
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
    # %XX only: Ambar encodes spaces as %20, so a literal "+" in a filename stays a "+".
    value=$(printf '%b' "$(printf '%s' "$value" | sed 's/%\(..\)/\\x\1/g')")
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

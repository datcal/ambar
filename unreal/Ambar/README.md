# Ambar for Unreal Engine

Browse your [Ambar](../../README.md) asset library from inside the Unreal Editor: search it, open
its folders, look at an asset properly, and import it into the project — with the licence and the
credits file taken care of.

Written for **Unreal Engine 5.8**, on **Windows and Linux** (nothing in it is platform specific).
It is an editor-only plugin: it adds nothing to your packaged game.

## Install

1. Unzip `ambar-unreal-plugin-<version>.zip` into your project's `Plugins/` folder, so that
   `YourProject/Plugins/Ambar/Ambar.uplugin` exists. (From a checkout: copy `unreal/Ambar`.)
2. Open the project. Unreal asks to build the plugin; say yes. A C++ toolchain is needed once —
   Visual Studio 2022 on Windows, the Unreal-supported clang on Linux. A Blueprint-only project
   becomes a C++ project the first time; nothing else about it changes.
3. **Window → Ambar Asset Library** (or the **Ambar** button in the toolbar, or `Ambar.Open` in the
   console).

## Connect

The tab opens on a setup panel the first time:

| Field | What |
| --- | --- |
| Server address | `http://nas:8973`, or the `https://` address of your tunnel |
| API token | Ambar web UI → **Settings → API tokens**. Tick **write**: it is what lets imports be recorded |

Press **Save and connect**. The settings live in *your* user settings (`EditorSettings.ini`), not
in the project — so the token is never committed, and the same connection works in every project.
They can also be edited under **Editor Preferences → Plugins → Ambar**.

### Several people, one server

Each person gets their own account and token, on the server:

```sh
docker exec -it ambar /ambar user add alice
docker exec -it ambar /ambar token add alice --name unreal --scope write
```

Everyone pastes their own token. The project itself is identified by a UUID in
`.ambar/project.json`, which *is* committed, so the whole team's imports end up in one place on
the server and in one `CREDITS.md`.

### Through a Cloudflare tunnel

Ambar binds plain HTTP; a tunnel is the usual way to let a friend reach a NAS.

```sh
cloudflared tunnel --url http://localhost:8973        # a quick, temporary address
# or a named tunnel with your own hostname — see Cloudflare's documentation
```

Set `AMBAR_BASE_URL` on the server to the public `https://` address, and use that address in the
plugin. The API needs a token on every request, so the tunnel is not an open door. If you also put
**Cloudflare Access** in front of it, create a *service token* in Cloudflare and paste its client ID
and secret into the plugin's connection panel; they are sent as `CF-Access-Client-Id` and
`CF-Access-Client-Secret` on every request.

## Using it

- **Search** is Ambar's own language: `sword type:model`, `32x32`, `color:#8b3a3a~20`,
  `-style:realistic`. Kind chips and the sort menu sit beside it.
- **Folders** open one level at a time, with asset counts. Click one to browse it; the breadcrumb
  above the grid goes back up. Format variants of one artwork (PNG, PSD, ASEPRITE) are one tile.
- **Select** one tile to inspect it — large preview, size, triangle count, licence, tags (click a
  tag to search for it), other formats — or several (Ctrl/Shift-click) to import them together.
- **Import** with the button, by double-clicking a tile, or from the right-click menu. The original is
  downloaded, **checked against the hash the server listed**, and imported with Unreal's own
  importers — exactly as if you had dragged the file into the Content Browser — into
  `/Game/Ambar/<kind>/<pack>` (change the root in the settings). The Content Browser then shows it.
  Pixel-art textures (as detected by the server) get nearest filtering, no mipmaps and UI
  compression, unless you turn that off.
- **In project** lists everything the project holds from the library: what the library has since
  replaced (**Update** re-imports it over the same asset, so references survive), what was imported
  while the server was unreachable (**Tell server**), and **Write CREDITS.md**, built by the server
  from the licences of what the project actually uses.

What Unreal can import is up to Unreal: `.glb`, `.fbx`, `.obj`, `.png`, `.jpg`, `.tga`, `.wav` and
the like work; Unreal has no importer for `.ogg`/`.mp3` or Aseprite files, and says so rather than
guessing. A `.gltf` that points at separate `.bin` and texture files needs those files beside it,
so prefer `.glb`.

## The cache

Everything the plugin remembers is in one folder and none of it is precious:

```
<user settings>/AmbarUnreal/Cache/<server>/
  folders/    the folder tree, one small file per folder
  thumbs/     thumbnails, named by asset id and content hash — an entry cannot go stale
  previews/   the large inspector previews
```

- Folder names are shown **immediately from disk** and refreshed from the server a moment later, so
  big libraries open instantly and still show new folders.
- Pictures are kept until the cache passes its limit (512 MB by default; least recently used go
  first), and are what makes a second visit to a folder instant — and lets you browse what you have
  already seen while the server is off.
- The **Cache** menu in the header shows the size, switches either kind of caching off, clears
  pictures / folder names / everything, and opens the folder. Each server has its own sub-folder.

## Files it writes

| Where | What | Commit it? |
| --- | --- | --- |
| `<Project>/.ambar/project.json` | the project's UUID and name | yes |
| `<Project>/.ambar/manifest.json` | asset id → what was imported | yes |
| `<Project>/CREDITS.md` | only when you press the button | yes |
| `<Project>/Saved/Ambar/Staging` | the download, until the importer has taken it | no (temporary) |
| user settings | server, token, cache options | never |

The manifest and project files use the same format as the Godot plugin's, so a team using both
engines shares one record on the server.

## Server requirements

Ambar 1.x with the folder API (`GET /api/v1/folders`, `search?dir=`, `thumb?format=png`), which is
in the same release as this plugin. Older servers connect but show no folders.

## Layout

```
Ambar.uplugin
Source/AmbarEditor/
  Public/AmbarSettings.h         per-user settings
  Private/AmbarClient.*          the HTTP client (every failure comes back as a sentence)
  Private/AmbarCache.*           the on-disk cache
  Private/AmbarImages.*          picture loading: memory → disk → server, 6 requests at a time
  Private/AmbarImporter.*        download, verify, import, record
  Private/AmbarProject.*         .ambar/project.json and manifest.json
  Private/SAmbarBrowser.*        the tab
  Private/SAmbarTile.* SAmbarInspector.* SAmbarProjectPanel.*
```

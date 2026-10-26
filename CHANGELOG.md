# Changelog

Notable changes per release. This project follows [semantic versioning](https://semver.org):
while it is on `1.x`, the HTTP API, the configuration variables and the on-disk layout
will not change incompatibly without a major bump.

## Unreleased

- **Blender stays open between files.** A conversion used to start Blender, which on the
  NAS's N100 was 2.9 s of a 3 s FBX; one Blender now takes file after file over stdin, each
  from an empty factory scene: 0.06 s a file, at a steady 350 MB. An idle Blender exits after a
  minute, one is replaced after 500 files, and one that crashes or overruns is killed and
  replaced.
- **A worker with Blender can run on the NAS itself:** `COMPOSE_PROFILES=worker` starts one
  beside the app, one file at a time on one core and weighted below it, with an unpacked
  Blender in `blender/` and a write token in `worker.token` in the compose directory.
  `scripts/deploy.sh` builds and ships its image when it changed.
- An ASCII FBX is "no preview" without waiting for Blender, whose importer refuses it.
- **The jobs page says when models wait for Blender and nothing can convert them.** FBX and
  `.blend` waiting for Blender are counted under Previews, and while this server has no Blender
  and no worker with Blender is connected, a warning at the top names the worker and when it was
  last seen — remembered across restarts (`blender-worker.json` in the data root). A worker that
  stopped at night left forty thousand of them waiting with nothing on the page to show it.
- **Retry leaves failed FBX and `.blend` to wait for a worker with Blender** when nothing can
  convert them, instead of running them through this server and straight back to "needs
  Blender", which looked like a retry that did nothing. Their failed jobs are closed with
  "waiting for a worker with Blender"; the worker is offered them when it connects. When Blender
  is available, Retry queues everything waiting for it as well. The page says what Retry did.
- **TIFFs with extra channels get previews.** x/image/tiff refuses an RGB TIFF whose fourth
  channel is marked "unspecified" and any greyscale TIFF with an extra channel; Synty ships its
  metallic and specular maps that way, smoothness in the fourth channel. They are read by a
  fallback now (8-bit strips, plain, LZW or Deflate), the extra channel left out unless the
  file marks it alpha. A TIFF neither reads is "no preview", not failed.
- An `.obj` with no geometry — a locator exported beside the meshes — is "no preview", not
  failed, so a retry no longer fails it again.

- **AI assistants can use the library (MCP).** `/mcp` serves the Model Context Protocol over
  Streamable HTTP, behind the same API tokens as `/api/v1`: `search_assets`, `browse_folder`,
  `get_asset`, `view_asset` (the thumbnail, as an image) and, with a write token,
  `import_into_unreal` and `import_status`. An import goes through the user's open Unreal editor:
  the server keeps the request, the plugin collects it (`GET /api/v1/editor/requests`, a long poll),
  imports each asset as its Import button does, picking the format Unreal takes, and reports the
  package paths back to the assistant. The plugin setting "Accept imports from AI assistants"
  turns it off. See the README for Claude Code's configuration.
- **The first account can set another user's password** under Settings → Users, for one
  that was forgotten: choose the user, type the new password twice and your own once. They are
  signed out everywhere; their API tokens keep working. Nobody else can, so the second user
  cannot lock the first out. `ambar user passwd` still works on the server.
- **The library is a folder view now.** The folder tree is the sidebar — always open at the
  folder you are in, as deep as the library goes, counting what the grid would show for the
  kind you picked — with a box above it to jump to any folder by name or by a pasted path
  (`/mnt/…`, a Windows path, a `file://` link; `GET /go?path=`). Above the grid: a breadcrumb,
  "Go to path…", "Copy path", a kind switch counted *within the current folder*, an "Include
  subfolders" switch (`sub=0`), shortcuts to the subfolders, and a list view (`view=list`)
  beside the thumbnails. Clicking a tile opens a panel instead of a tab: the picture, its
  formats with a download each, its local path, where it is used, and **which file Godot and
  which Unreal should take** (Godot prefers glTF, Unreal FBX; Unreal has no importer for
  `.ogg`/`.mp3`, and the panel says so). Double-click or Ctrl+click still opens the full page.
  Tiles name their formats ("FBX · OBJ") instead of "2 variants". Tags, colours and the fixed
  filters fold away under the tree; kinds moved onto the toolbar. The accent is amber.
- **The search box has a scope.** The folder and kind you are browsing sit in it as chips,
  so a search runs inside them; × or Backspace widens it again. Typing a folder's name
  offers it first — "spy", Enter, and you are searching inside POLYGON - Spy Kit — and inside
  a folder, "mod" offers *Models (47 in POLYGON - Spy Kit)*.
- The asset page's "Use in a game" box gives the same Godot/Unreal answer, and "In Godot
  projects" is "In projects", marked Godot or Unreal by the project path.
- Fixed: the tag list beside a folder failed on every folder (`no such column:
  p.library_rel_path`), and "Tag all N" inside a folder tagged every match in the library.
  The first page view after a restart no longer caches a sidebar with no folder tree when
  the browser moves on before it is built.
- `GET /api/v1/folders` serves the sidebar's cached tree instead of building its own.
- **Browsing a large folder is fast.** Measured on a 111,000-asset library: a page of
  `3d/` took about a second and a 100-file pack 440 ms; both are now a few milliseconds.
  The folder filter reads the `(pack_id, rel_path)` index instead of assembling every
  asset's path; a large result walks its sort order's index (the 0016 indexes were never
  used, because the tiebreaker came from another table); the grid's total comes from the
  cached tree; and the Tags list loads when its section is opened (`GET /facets`) instead
  of with every page. The tree itself builds in a fifth of the time. Background events — a
  browser-rendered thumbnail, a remote worker's result — mark the sidebar stale instead of
  throwing it away, so a page view no longer waits for it to be rebuilt. A folder with more
  than twelve subfolders folds them behind one line.
- **The folder panel can be resized**: drag the line beside it, or focus it and use the arrow
  keys (Shift for bigger steps); double-click resets it. The width is remembered per browser.
  It starts a little wider (18rem), and deep folders indent less.

- **Unreal plugin: a status bar drawer, folder imports, and everything in a folder.** Ambar sits on
  the status bar beside the Content Drawer and slides up the same way; **Dock in layout** keeps it as
  a tab beside the Content Browser. Right-click a folder to import it with its subfolders (counted and
  confirmed first, cancellable, one report at the end). Imports go by kind: models, images and sound
  through Unreal's importers into `/Game/Ambar/<the library's folders>` (two Synty packs' files of the
  same name no longer overwrite each other); `.uasset`/`.umap` copied into `Content` where their
  references resolve; code and other files copied to `<Project>/Ambar/`. **Update** re-imports into
  the folder an asset already lives in. New **Unreal** and **Code** chips; tiles with no picture show
  their extension. Fixed: model tiles showed a checkerboard (the plugin asked for a size a
  browser-drawn thumbnail never has — and the server now falls back to the size it does), and
  **Update all** in "In project" stopped after the first asset and stayed busy.
- **`ext:` in the query language** (`ext:uasset,umap`, `-ext:psd`), and C/C++ and Unreal shader
  source are `script` (shown as Code) rather than `other`; migration 0022 moves rows indexed before.
- Fixed: the Unreal plugin did not compile against UE 5.8.4 ("could not be compiled. Try
  rebuilding from source manually"). 5.8 keys `FJsonObject::Values` by `FSharedString`, and
  `SImage::Image` binds only const getters. Verified by building a project's
  `<Project>Editor` target with the plugin in `Plugins/`.
- Fixed: **a remote worker that lost its library failed everything it was given.** When the
  desktop's automounted share reconnected, the worker's container kept the old, empty mount,
  and in a quarter of an hour 47,000 assets were marked failed three attempts at a time. A
  worker whose library is empty or unreadable now takes no work, hands back a job it could not
  open with the attempt refunded, and logs once until the library is back. The Quadlet in
  `deploy/` mounts the library `rslave`, so a share mounted again on the host is seen in the
  container. Recover such a run with **Retry failed** on the jobs page.
- The worker's Quadlet runs it at idle CPU priority (`CPUWeight=idle`, `IOWeight=10`): eight
  Blenders working through a backlog made the Unreal editor on the same desktop crawl.
- **Unreal Engine plugin** (`unreal/Ambar`, UE 5.8, Windows and Linux): an Ambar tab in the
  editor with live search, a folder tree opened one level at a time, kind and sort filters,
  multi-select, an inspector (large preview, licence, tags, other formats), import through the
  editor's own importers with the hash checked against the server's, a project manifest and
  `CREDITS.md` like the Godot plugin's, and an "In project" screen for updates. Folder names,
  thumbnails and previews are cached on disk (size-capped, with a Cache menu to clear them).
  Each person uses their own API token; Cloudflare Access service tokens are supported for
  tunnels.
- **API: `GET /api/v1/folders?path=`** lists one level of the library tree with asset counts,
  **`search?dir=`** limits a search to a folder and everything beneath it, and
  **`thumb`/`preview.webp?format=png`** serves the picture as PNG for clients with no WebP
  decoder.

- **`.ambarignore`**: a per-folder hide list with the everyday `.gitignore` syntax
  (names, `dir/`, anchored paths, `**`). Hidden files leave the index on the next scan
  instead of being marked missing; nothing on disk is touched, and removing the rule
  brings them back. Bad lines are reported in the scan summary.
- **`.unitypackage` files are no longer indexed**, like the archives before them. Rows an
  earlier scan created are dropped on the next one.
- A scan never drops the row of an asset a Godot project has used, whatever rule excludes
  it — that would have cascaded the project's record and its `CREDITS.md` entry away.
- **The model palette backfill no longer queues models without a thumbnail.** It assumed
  a library of under a thousand models; after a Synty import it queued 78,570 jobs at every
  restart, each of which found nothing to read.
- The server starts listening immediately; the startup backlog enqueue runs beside it
  rather than in front of it.
- **`ambar worker`: derive work on another machine.** With `AMBAR_REMOTE_DERIVE=auto` or
  `only`, a worker claims `asset.derive` jobs over the API, decodes against its own mount
  of the library and uploads the results; the server checks each file against the names
  and formats derive writes, moves it into place and records it through the same code as a
  local derive. Leases expire after 15 minutes, a stopping worker hands its jobs back, a
  worker built from a different derive version is refused, and a worker with Blender is
  offered everything waiting in `needs_blender`. The server needs no worker and behaves as
  before with the setting off. Migration 0020 adds a partial index for claiming queued jobs
  by type.
- **`ambar token add`** creates an API token from the command line.
- **Choose where background work runs from the Background work page** — this server,
  automatic, or remote workers only — with the connected workers listed beside it: what
  each is working on, how many it has done, how fast. "Remote workers only" stops the
  server's own workers entirely. The choice is remembered in the data root and outranks
  `AMBAR_REMOTE_DERIVE`. A worker told to wait keeps waiting instead of exiting, and a job
  it was halfway through is still accepted after the switch.
- **Clear the queue** from the same page: waiting jobs are deleted, running ones finish.
- **`Dockerfile.worker`**: the worker as a container, using the desktop's own Blender mounted
  read-only (an official Linux build is self-contained; the Flathub package's `files/blender`
  is one), and a Podman Quadlet unit in `deploy/`. Neither image carries a Blender.
- Blender's conversion script is written beside its output, not in the system temp
  directory, which a sandboxed Blender cannot see.
- **Model textures are found by name.** The 3D viewer and the grid's thumbnail renderer send
  every texture a model names to `/assets/{id}/texture`, which resolves it nearest first:
  the exact path, the same name up to the pack root, the same name as a browser-readable
  format (`.psd` → `.png`), then the closest-named colour texture in the nearest Textures
  folder. Normal, emissive and other non-colour maps are never guessed. On a sample of 640
  Synty FBX files, textured renders went from 29 to 459. The 3D scripts now carry the
  version, so a changed loader reaches browsers that cached the old one.
- **Redraw model thumbnails from the jobs page.** Each thumbnail records which renderer drew
  it and how many textures were missing (`thumb.json`); one drawn by an older renderer —
  the white FBX tiles — is replaceable by the current one and nothing else. The "Model
  thumbnails" card walks the library in the open tab and draws every missing or stale one.
- **Claiming a job no longer sorts the whole queue.** The claim's index could not deliver
  priority order past its range on `run_after`, so every claim read and sorted every queued
  row: invisible at a few hundred jobs, most of the NAS's CPU at 170,000. Migration 0021
  adds a partial index in claim order, both claim queries name their index, and a test pins
  the plans.
- **Change your password on the settings page.** It asks for the current one, and wrong
  guesses count against the login limits. Every other browser signed in as you is signed
  out, this one carries on under a new session, and the change is audited. API tokens
  keep working. A forgotten password is reset on the server with `ambar user passwd
  <username>`, which also ends that user's sessions.

## v1.0.0

First tagged release. Everything below already existed and had been running against a
real 6,500-file library; the tag is the point at which it became something to hand to
somebody else.

### The library

- **`ambar scan`** — depth-agnostic pack detection, junk exclusion, kind classification,
  hashing and reconciliation. A moved file keeps its row and id; a missing file is marked,
  never deleted; a library that looks unmounted aborts the scan rather than flagging
  everything. Unchanged files are not re-read.
- **Format variants collapse into one asset.** The PNG, PSD and ASEPRITE of one sprite —
  and the FBX, glTF and OBJ of one model — are one grid tile with the rest listed beside
  it, including packs that split them across `PNG/`, `Source/` or `fbx(unity)/` folders.
- **Search**: FTS5 over filenames and packs, plus a query language —
  `sword type:model 32x32 -style:realistic`, `color:#8b3a3a~20`, `palette-near:<id>`.
- **Tags** with namespaces, hierarchy, aliases, autocomplete, bulk tagging and automatic
  tags derived from paths.
- **Nine browse orders** and numbered paging that stays flat at any depth.

### Previews

- Pixel-art-safe thumbnails: nearest-neighbour resizing decided by colour count and edge
  hardness, not by image size, so a large pixel-art atlas is handled correctly.
- An `.aseprite` decoder reading the binary format directly — frame tags, durations,
  layer compositing — checked pixel-by-pixel against vendor PNG exports.
- Audio waveforms, font specimens, spritesheet frame detection with a player, a 2D viewer
  with zoom and pan, and a browser 3D viewer for `.obj`, `.fbx` and `.gltf`.
- `.tga` and `.xcf` have no pure-Go decoder and are recorded as `unsupported` with a
  reason rather than failing quietly.

### Provenance, licensing and safety

- Provenance and licence per pack, a licence-risk view, and `CREDITS.md` generated per
  Godot project from what that project actually uses.
- Duplicate detection — exact hash, pack containment, near-identical images — that
  reports and never acts.
- A removal path built around refusing: nothing pre-selected, every selection previewed,
  an asset a project uses is a hard block, and the last copy of any content can never be
  removed. Removals move to a trash folder with a record of where they came from.
- Reflink or hardlink deduplication where the filesystem supports it, so space is
  reclaimed while every path keeps working.

### Operations

- Job queue with retries, crash recovery and a status page.
- `verify` (re-hash to detect bit rot), `rebuild-index` (reconstruct from the filesystem),
  `backup` (VACUUM INTO), `junk`, `dupes`, `trash`.
- argon2id passwords, server-side sessions, login rate limiting, CSRF, API tokens with
  scopes. No self-registration.
- One container, ~25 MB, no CGO.

### The Godot plugin

- A main-screen tab: a grid that reflows to the window, thumbnail sizes from 64 to 256,
  the nine sort orders, numbered pages, and per-person preferences that persist.
- An inspector panel that shows an asset — preview, dimensions, frame count, geometry,
  tags, licence, other formats — **before** importing it.
- Model previews rendered by the editor itself and posted back to the server, so models
  the server has no picture of fill in for everyone, including the web UI.
- Import verifies the downloaded bytes against the hash the server advertised and refuses
  on a mismatch.
- An **In this project** screen: what has been imported, what is stale, what the server
  was never told about, what is missing from the checkout, and `CREDITS.md`.

### Known gaps

- `.fbx` and `.blend` need Blender for a server-side thumbnail; without it they are
  reported as `needs_blender`. The Godot plugin covers glTF and OBJ by rendering them.
- Aseprite blend modes other than Normal are approximated as Normal, and say so in the
  job log.

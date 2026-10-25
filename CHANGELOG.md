# Changelog

Notable changes per release. This project follows [semantic versioning](https://semver.org):
while it is on `1.x`, the HTTP API, the configuration variables and the on-disk layout
will not change incompatibly without a major bump.

## Unreleased

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

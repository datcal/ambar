# Changelog

Notable changes per release. This project follows [semantic versioning](https://semver.org):
while it is on `1.x`, the HTTP API, the configuration variables and the on-disk layout
will not change incompatibly without a major bump.

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

# Ambar — Self-Hosted Game Asset Library

> **The design specification.** This is the long-form reference: what each part is supposed to
> do, and *why* — including the decisions that were reversed, kept with their reasons, because a
> specification that still argues for an abandoned idea costs more than one that admits the
> change.
>
> It is not a getting-started guide. [../README.md](../README.md) is that, and
> [../ARCHITECTURE.md](../ARCHITECTURE.md) is the map of how the pieces fit. Read a section here
> before changing behaviour it describes, or before revisiting a choice that looks arbitrary —
> most of them are load-bearing and the reason is written down.
>
> Section numbers are stable and are referenced from code comments. `ambar` is used throughout as
> the binary name, config prefix and Godot plugin folder name.

## 0. Settled decisions

These were argued through and are not open for reinterpretation. If the implementation wants
to deviate, say so explicitly rather than quietly doing something else.

- **One container, one mode.** It runs on the NAS. No agent/server split, no sync protocol,
  no separate worker container.
- **Everything lives on the NAS**: the asset library, the derivatives, and the SQLite
  database. Backup is `rsync` plus one database file.
- **The filesystem is the source of truth. SQLite is a rebuildable index.**
- **Single static Go binary.** No CGO. Server-rendered HTML with htmx. No SPA, no bundler
  for the main UI.
- **The app knows nothing about how it is reached.** It binds `0.0.0.0:8080` and speaks plain
  HTTP. Remote access is Tailscale's problem, not the application's.
- **Originals are never modified, moved, or renamed.** Human-readable paths are a hard
  requirement.
- **The interface is part of the product, not a skin on it.** Added after M16, because the
  first version was correct and unpleasant: prose where a control belonged, a viewer that ate
  the scroll wheel, sorting with no stated order, a "Load more" button where a page number was
  wanted. None of that showed up as a bug. It showed up as people not using the thing.

### Non-goals

- Not version control. The game repo lives in git+LFS; this manages the *upstream library*.
- Not a marketplace, not multi-tenant, not a public gallery.
- No asset editing. Read, preview, tag, export.
- Not a file sync tool.

## 1. What this is

A self-hosted digital asset manager for a two-person indie game team building a Godot game
for Steam release. It indexes thousands of downloaded game assets living on a home NAS —
3D models, textures, sprites, spritesheets, audio, HDRIs — arriving mostly as zip/rar
archives from itch.io bundles, Kenney packs, Poly Haven, freesound, and Humble bundles.

It makes them searchable by tag, previews them properly in the browser, tracks where each
asset came from and under what license, and lets a Godot editor plugin pull assets straight
into a project.

## 2. Stack and deployment

| Concern | Decision |
| --- | --- |
| Language | Go. Prefer stdlib `net/http` with Go 1.22+ pattern routing; `chi` only if it earns its place. |
| Database | SQLite, single file. `modernc.org/sqlite` (pure Go, **no CGO**). FTS5 required — verify in M0 before building on it. |
| Frontend | Server-rendered HTML + htmx. `html/template` or `templ`. JS only as isolated islands: 3D viewer, canvas zoom, audio scrubber, waveform. |
| Container | **One** container, multi-stage build, `alpine` or `scratch` final stage. Target under 250 MB without Blender. |
| Host | Docker on a Synology/Unraid-class NAS. Library and data bind-mounted. |
| Bind | `0.0.0.0:8080`, plain HTTP. |
| Auth | Required. Small fixed set of users, plus machine tokens for the Godot plugin. |

### Access topology

The application implements none of this. It is documented here so the auth and
rate-limiting requirements in §11 make sense.

- **Tailscale tailnet — the primary path.** Both users install the client once. MagicDNS
  gives one stable hostname that works identically at home and away, so the Godot plugin
  needs exactly one configured URL and no LAN/remote fallback logic. No inbound ports on
  the router, no public attack surface, no request body limits, direct peer-to-peer
  transfer where possible.
- **Cloudflare Tunnel — optional.** Already running for other things. Its one advantage
  over Tailscale here is a custom domain, so keep it for the rare case of showing the
  library to someone on a real hostname. Note that Cloudflare's free plan caps request
  bodies at 100 MB, so uploads through it will fail for large archives. Do not engineer
  around this — the `_inbox` path in §5 makes it irrelevant. Just document it.
- **Tailscale Funnel — ad-hoc.** Makes a service publicly reachable with no client needed
  on the visitor's side, at `node.tailnet.ts.net`. Custom domains are not supported and
  ports are limited to 443/8443/10000. Useful for one-off sharing.

**Consequence that matters:** if Funnel or Cloudflare is ever enabled, the app is on the
public internet with no edge rate limiting. Bots will find it. This is why §11 is not
optional.

If `AMBAR_TRUSTED_PROXIES` is empty, ignore all forwarded-IP headers and use the socket
peer address. Only honour `X-Forwarded-For` / `CF-Connecting-IP` from explicitly configured
CIDRs.

## 3. Storage layout and source of truth

The filesystem being the source of truth means: the database can be deleted and
reconstructed by rescanning; files can be added or reorganised by hand over SMB; backup is
`rsync` plus one file; and there is no proprietary blob store to be trapped inside.

```
$LIBRARY_ROOT/                       # bind mount; treat originals as read-only
  itch-io/kenney/sci-fi-rts-pack/
      .ambar.json                    # sidecar: pack provenance + per-file metadata and tags
      Models/turret_a.glb
      Sprites/ui_atlas.png
  poly-haven/hdri/studio_small_09/
  freesound/impacts/
  _inbox/                            # drop archives here; polled
  _archives/                         # optional: retained original archives
  _quarantine/                       # failed ingests, with an error log beside each
  _trash/                            # staged deletions, original path preserved

$DATA_ROOT/
  ambar.db                           # WAL mode
  derivatives/<sha[0:2]>/<sha>/
      thumb.webp                     # 512px
      thumb@2x.webp
      turntable.webp                 # animated, 3D only
      preview.glb                    # normalised model for the web viewer
      peaks.json                     # audio waveform peaks
      sheet.gif                      # spritesheet animation preview
  tools/                             # runtime-downloaded Blender, if used
  logs/
```

### Sidecars

`.ambar.json` per pack carries everything needed to reconstruct that pack's metadata:
provenance, license, per-file tags, spritesheet frame geometry, notes. Write it on every
metadata change, debounced.

On scan: if a sidecar exists and the DB row does not, import from the sidecar. On conflict,
newest `updated_at` wins and the divergence is logged. This is what makes the database
genuinely disposable and makes a copied folder carry its own metadata with it.

Support `AMBAR_LIBRARY_READONLY=true`, where sidecars are written to
`$DATA_ROOT/sidecars/` mirroring the tree instead, and ingest is disabled.

## 4. Data model

Sketch, not gospel. Adjust names, keep the shape. Every table gets `created_at`,
`updated_at`, and soft-delete `deleted_at` where deletion is user-facing.

```
packs
  id, name, slug, kind (archive | folder | standalone)
  library_rel_path
  source_url, source_site, source_author, source_author_url
  license_id, license_note, attribution_required, attribution_text
  acquired_at, price_paid_cents, currency, order_ref
  original_archive_name, original_archive_sha256, original_archive_size
  provenance_state (complete | needs_provenance)
  notes

assets                               -- one row per file
  id, pack_id, rel_path, filename, ext
  kind (image | spritesheet | texture | model | audio | video | font | script | material | hdri | other)
  size, sha256, phash
  -- image
  width, height, has_alpha, is_pixel_art, color_count, has_semitransparent
  palette_json, palette_kind (exact | quantized)   -- [{hex, r, g, b, count, ratio}, ...]
  -- spritesheet
  frame_w, frame_h, frame_count, frame_cols, frame_rows, fps, frame_source (sidecar|detected|manual)
  -- audio
  duration_ms, sample_rate, channels, bit_depth, peak_dbfs, is_loopable
  -- model
  tri_count, vert_count, bbox_x, bbox_y, bbox_z, material_count, animation_names
  -- derivatives
  derive_state (pending | ok | failed | unsupported | needs_blender), derive_error, derive_version
  first_seen_at, last_verified_at, missing_since

licenses          id, spdx_id, name, commercial_ok, attribution_required, share_alike, url
tags              id, namespace, name, description, parent_id
tag_aliases       id, tag_id, alias
asset_tags        asset_id, tag_id, source (manual | auto_path | auto_type | inherited), created_by
pack_tags         pack_id, tag_id, source

projects          id, uuid, name, note          -- uuid is authoritative, never a filesystem path
project_uses      id, project_id, asset_id, res_path, asset_sha256, added_at, removed_at

jobs              id, type, payload_json, state (queued|running|done|failed),
                  attempts, last_error, priority, run_after, started_at, finished_at
saved_searches    id, name, query_json
users             id, username, password_hash, role, created_at, last_login_at
api_tokens        id, user_id, name, token_hash, scopes, last_used_at, expires_at, revoked_at
sessions          id, user_id, token_hash, expires_at, user_agent, ip
audit_log         id, user_id, action, entity, entity_id, detail_json, at

assets_fts        FTS5 external-content over: filename, pack_name, tag_text, notes
```

`derive_version` earns its place immediately: when the thumbnail algorithm improves, bump
the version and only stale derivatives regenerate. Without it, every improvement means
manually re-triggering twenty thousand files.

### SQLite discipline

`journal_mode=WAL`, `busy_timeout=5000`, `foreign_keys=ON`, `synchronous=NORMAL`.
Serialize writes through a single writer connection (`SetMaxOpenConns(1)`) with a separate
read pool.

**The database file must sit on a real local volume of the NAS, not on a remote SMB/NFS
share bind-mounted into the container.** WAL corrupts on network filesystems. A Synology
`/volume2/...` path is a local volume and is fine — SMB is only how a desktop reaches it
from outside. The failure mode to avoid is mounting some *other* machine's share into the
container and putting the database there.

Keep the database out of the library tree itself, so `scan` never walks it and so a
file-level backup of the library never captures a live WAL database mid-write. See §17.

## 5. Ingest

Two paths in, converging on one pipeline.

1. **Inbox polling — the primary path.** Poll `$LIBRARY_ROOT/_inbox/` on an interval.
   Polling, not inotify: it must work across SMB and Docker bind mounts. A file is ready
   when its size and mtime are unchanged across two consecutive polls. Large bundles arrive
   this way — dropped over SMB from the desktop, which is where they were downloaded anyway.
2. **Web upload.** Plain multipart, for small files, with a configurable size cap. No
   chunking, no resumable upload machinery. If someone tries a 2 GB archive through
   Cloudflare it fails; the error message should say to use `_inbox`.

Optionally also: paste a direct download URL and let the server fetch it. Useful for Poly
Haven. Respect a size cap. Low priority.

### Pipeline

Each step is a job row, so any step retries independently and progress is visible:

1. `archive.inspect` — identify type, list entries. Pure-Go decoders: `archive/zip`,
   `github.com/nwaples/rardecode` (RAR5), `github.com/bodgit/sevenzip`. No external
   `unrar` binary — keeps the image small and the licensing clean.
2. `archive.extract` — extract to the target library path. **Sanitize every entry path**
   against traversal: `../`, absolute paths, symlinks, Windows drive prefixes. Enforce a
   total uncompressed size cap and an entry count cap against zip bombs. Flatten a single
   redundant top-level folder if present.
3. `pack.create` — create the pack row, attaching provenance if supplied.
4. `asset.index` — walk the tree, classify each file, hash it, extract cheap metadata.
5. `asset.derive` — one derivative job per asset.
6. `pack.autotag` — derive tags from path and file type (§7).

### Provenance capture

**This must be frictionless or it will not happen, and then the tool has failed at its most
valuable job.**

New packs land in `provenance_state=needs_provenance` and appear prominently in the UI as a
short form: source URL, author, license dropdown, price, date. Pre-fill by sniffing the URL
— an `itch.io` URL implies the site and usually the author from the subdomain.

Additionally: if a `<archive>.url` or `<archive>.txt` file is dropped into `_inbox`
alongside the archive, read the source URL from it. This lets provenance be captured at
download time, in the browser, instead of reconstructed later from memory.

### Deduplication

On `asset.index`, if `sha256` already exists, record a second location of the same content
rather than a new asset, and surface it in a duplicates view. Use `phash` Hamming distance
for near-duplicate images — the same sprite pack downloaded twice at different resolutions
is the common case.

## 5.1 Pack detection, junk, and format variants

These rules are derived from the actual target library and are not hypothetical. Getting
them wrong makes the grid view unusable on day one.

### Pack detection must be depth-agnostic

Packs are **not** all at depth 1. The library contains both
`./craftpix-net-695666-free-undead-tileset-top-down-pixel-art/` and
`./raw/craftpix-net-385863-free-top-down-trees-pixel-art/`. Detect a pack as the shallowest
directory that either contains a `.ambar.json`, or contains asset files directly, or whose
children are recognisable format/variant folders. Do not assume a fixed depth, and do not
treat organisational parents (`2d/`, `3d/`, `mix/`, `raw/`) as packs.

### Junk to ignore, always

`__MACOSX/` (present in the library, at any depth), `.DS_Store`, `Thumbs.db`,
`desktop.ini`, `._*` AppleDouble files, `.git/`. Configurable ignore-glob list, with these
as defaults. `__MACOSX` in particular duplicates the entire tree of a pack and will double
the apparent asset count if not excluded.

### Format variants are one asset, not many

This is the most consequential rule here. Packs from CraftPix and similar vendors ship the
same artwork several times over, split by format into sibling folders:

```
craftpix-net-284465-free-predator-plant-mobs-pixel-art-pack/
    PNG/Plant1/...
    PSD/Plant1/...
    ASEPRITE/Plant1/...
    Tiled_files/...
```

`PNG/Plant1/idle.png`, `PSD/Plant1/idle.psd`, and `ASEPRITE/Plant1/idle.aseprite` are one
logical asset in three formats — not three assets. Indexing them independently means every
sprite appears three or four times and the grid becomes noise.

Introduce an `asset_group` concept: group by (pack, relative path with the format-folder
segment removed, filename without extension). Nominate a **primary** variant — the
engine-ready one, normally PNG — and treat the rest as source variants attached to it.
The grid shows one entry per group; the detail panel lists variants with download links per
format.

Format-folder segments to recognise, case-insensitively, matched as a whole path segment:
`PNG`, `PSD`, `ASEPRITE`, `ASE`, `SVG`, `EPS`, `AI`, `KRA`, `XCF`, `Tiled_files`, `TMX`,
`FBX`, `OBJ`, `GLTF`, `GLB`, `BLEND`, `Source`, `Sources`, `SourceFiles`, `Vector`,
and variants such as `PNG_Animations` and `PNG_Parts&Spriter_Animation` where the segment
*starts with* a known format token followed by `_` or `-`.

Primary-variant precedence, first match wins: `png` > `webp` > `glb` > `gltf` > `fbx` >
`obj` > `svg` > `aseprite` > `psd` > `kra` > `xcf`.

Tag groups with `has:source-file`, `format:psd`, `format:aseprite` and so on, so "give me
everything I can still edit" is a query.

### Additional kinds present in this library

- `.tmx` / `.tsx` — Tiled maps and tilesets. Kind `tilemap`. Parse the XML for grid size,
  tile count, and referenced image paths; render a thumbnail from the referenced tileset if
  cheap, otherwise mark `unsupported` and still index the metadata.
- `.scml` / `.scon` — Spriter rigged animation projects, seen in
  `PNG_Parts&Spriter_Animation/`. Kind `rig`. Index and tag; no preview.
- Loose PNG sequences in folders like `PNG_Animations/Explosions/` — a directory of
  numbered frames is effectively an animation. Detect a numeric-suffix run of images with
  identical dimensions in one folder and offer to treat it as an animation group with a
  generated GIF preview, the same way a spritesheet is handled.

### Provenance can be auto-derived here

Pack folder names already encode the source. Two observed shapes:

```
craftpix-net-695666-free-undead-tileset-top-down-pixel-art
craftpix-991101-free-pixel-art-enemy-spaceship-2d-sprites
```

Parse `^craftpix(-net)?-(?P<id>\d+)-(?P<slug>.+)$` to populate `source_site=craftpix.net`,
`source_ref=<id>`, a candidate `source_url` built from the slug, and seed tags from the slug
tokens. Present it as a **suggestion the user confirms**, never as silently-trusted truth —
the URL shape may be wrong and a wrong licence recorded confidently is worse than a blank
one.

Make this a small pluggable set of filename-pattern recognisers, so patterns for itch.io,
Kenney, and Poly Haven downloads can be added as their naming becomes known.

## 6. Derivatives and previews

One `asset.derive` job type dispatching on `kind`. Idempotent, keyed on
`sha256` + `derive_version`, so rescans do no work. Failures recorded in `derive_state` with
a "retry failed derivatives" action in the UI.

### Images — `png jpg jpeg webp gif bmp tga`

Pure Go: `image`, `golang.org/x/image/draw`, a webp encoder.

- **Nearest-neighbour resize when the image is pixel art.** Detect by low dimensions (either
  axis under 256), low unique-colour count, and hard edges. Store `is_pixel_art`. Bilinear-
  downscaling pixel art into mush is the single most annoying failure of every existing
  tool; get this right.
- Composite thumbnails over mid-grey so alpha-heavy sprites are visible in a dark UI, with
  the transparent version also available.
- Keep animation in GIF/WebP thumbnails, capped at a sane frame count.

### Spritesheets

Candidate if dimensions suggest a grid, or a sibling `.json`/`.tres`/`.atlas`/`.plist`/`.xml`
describes it, or the filename matches `_sheet`, `_atlas`, `-anim`, trailing frame counts.

- Parse sidecar metadata when present: TexturePacker JSON, Godot `.tres` AtlasTexture,
  Aseprite JSON export, Kenney XML.
- Otherwise **guess** the grid: derive candidate cell sizes from common divisors of both
  dimensions, score each by how well cell boundaries align with fully-transparent gutters
  and by resulting aspect ratio. Show the top guess with a **grid overlay** and let the user
  confirm in one click or correct manually. Never silently guess wrong. Record
  `frame_source` so detected values are distinguishable from confirmed ones.
- Generate `sheet.gif` playing frames at the stored fps, so the grid view shows animation
  instead of forty tiny squares.

### Source art — `psd kra svg aseprite xcf`

- `.psd`: `github.com/oov/psd` for the flattened composite. Pure Go.
- `.kra`: it is a zip with `mergedimage.png` already inside. Trivial, no dependency.
- `.svg`: `github.com/srwiley/oksvg` + `rasterx`. Pure Go.
- `.aseprite` / `.ase`: **first-class, not a nice-to-have.** The target library already ships
  `ASEPRITE/` folders and their share will grow, so this is a primary editable source format
  rather than an exotic case. Parse the documented binary format in Go
  (`ase-file-specs`): flattened composite per frame, frame durations, layer list, and
  Aseprite's own **frame tags**. Frame tags map directly onto `animation_names`, and frame
  durations give a real fps — so an animated preview can be generated from the `.aseprite`
  itself without needing the exported PNG sequence. Offer Aseprite's tags as suggested
  library tags on ingest.
  An **optional** CLI at `AMBAR_ASEPRITE_BIN` remains as a fallback for format versions the
  parser does not yet handle. Do **not** bake the Aseprite binary into the image — its
  licence does not permit redistribution. Document that it must be bind-mounted if wanted.
- `.xcf`: mark `unsupported` gracefully. Low priority.

### Audio — `wav ogg mp3 flac`

- Decode with pure Go where possible: `go-audio/wav`, `hajimehoshi/go-mp3`,
  `jfreymuth/oggvorbis`. Optional `ffmpeg` for exotic formats.
- Compute and store **peaks JSON** — min/max pairs at fixed resolution, roughly 2000 buckets
  — rather than rendering a waveform PNG. Cheaper to store, and it scales and scrubs
  responsively when drawn client-side on canvas.
- Extract duration, sample rate, channels, bit depth, peak dBFS. Detect probable loop points
  via near-zero crossings and matching head/tail.
- Transcode to `preview.ogg` only when browsers cannot play the original natively. 24-bit
  and 32-bit float WAV are the usual offenders.

### 3D models — `glb gltf obj fbx blend`

- **Normalise everything to `preview.glb`** so the browser viewer loads exactly one format.
  `gltf`/`glb` pass through with validation. `obj` converts in pure Go. `fbx` and `blend`
  need Blender.
- Extract triangle count, vertex count, bounding box, material count, animation names,
  embedded texture list.
- **Blender is optional and downloaded at runtime**, not baked into the image — the
  difference between a 250 MB image and a 2 GB one. A settings-page action downloads the
  Blender CLI into `$DATA_ROOT/tools/` with checksum verification. Until then, affected
  assets sit in `derive_state=needs_blender` with a clear reason shown in the UI.
- Turntable thumbnails: Blender `--background` with the **Workbench** engine — fast,
  CPU-only, no GPU needed in a container. Secondary path: when the browser viewer renders a
  model with no turntable, capture N canvas frames and POST them back for caching. Build
  the server path first; the client path is a good backfill for `.glb` when Blender is absent.
- **The client path is what actually carries this library.** Blender was never installed, so
  every model's picture comes from somebody's renderer: the browser's three.js viewer (M15) or
  the Godot plugin (M18, §10), both posting to `/assets/{id}/thumb`. Measured on the real
  library after the M18 grouping fix: 526 model groups, of which 178 had a picture — every one
  of them a model somebody had happened to open. All 526 have a `preview.glb`, which is what
  makes the client path able to finish the job. The endpoint refuses to overwrite an existing
  thumbnail, so "whoever looks first draws it" needs no coordination between clients.

### HDRI — `hdr exr`

Tone-map to an LDR thumbnail, retain the panorama for equirectangular preview. `exr` may
need `ffmpeg`; degrade gracefully.

## 7. Tagging and search

This is why the tool exists. It has to beat folders or there is no point.

- **Namespaced tags** stored as `namespace:name`: `type:sfx`, `theme:sci-fi`, `biome:desert`,
  `license:cc0`, `style:pixel-art`, `author:kenney`, `pipeline:needs-rework`. Namespaces are
  what make a 20k-asset library navigable where a flat tag soup collapses.
- **Hierarchy**: `type:sfx:impact` implies `type:sfx`; searching a parent returns children.
  Closure table or recursive CTE.
- **Aliases**: `sfx` → `type:sfx`, `cc0` → `license:cc0`. Typing the short form works.
- **Auto-tagging on ingest**, marked `source=auto_path` so it is distinguishable from and
  overridable by manual tags:
  - folder path segments: `Models/Environment/Rocks/` → `models`, `environment`, `rocks`.
    **Normalise segments before tagging**, because real vendor folders are messy: strip
    leading ordering prefixes (`2 Objects` → `objects`, `4 Stone` → `stone`), lowercase,
    replace spaces and `&` with `-`, collapse repeats, and skip segments that are recognised
    format-folder names (§5.1) or pure numbers. Without the prefix-stripping step the
    library fills up with tags like `1-tiles` and `4-stone`.
  - file type and detected properties: `type:model`, `style:pixel-art`, `has:alpha`
  - pack metadata: `author:kenney`, `license:cc0`, `source:itch.io`
- **Inherited tags**: pack tags apply to all member assets, shown greyed and overridable.
- **Bulk tagging**: multi-select in the grid; also "tag everything matching this search".
- **Search syntax** — a real parser, not a naive LIKE:
  ```
  type:model theme:sci-fi -style:realistic tris:<5000 license:cc0
  "laser turret" author:kenney has:animation added:>2026-01
  ```
  Colour is searchable too, and this turns out to matter more than it sounds: the game is
  being assembled from many different free packs by different artists, and palette mismatch
  is the main thing that makes such a game look incoherent. Support `color:#8b3a3a`
  (assets containing that colour, with a tolerance), `palette-near:<asset_id>` (assets whose
  palette is close to a given asset's, by earth-mover or nearest-neighbour distance over
  swatches), and a pack-level **palette consistency view** answering "does this tileset sit
  next to that character set". Finding the assets that match the art direction already chosen
  is the real daily problem, and no folder structure can answer it.

  Implicit AND, explicit OR, `-` negation, quoted phrases, numeric comparators on `tris`,
  `verts`, `width`, `height`, `duration`, `size`, and date comparators on `added` and
  `acquired`. FTS5 for the text part, indexed columns for the structured part.
- **Fuzzy filename matching** as fallback, so `swrd` finds `wooden_sword_01.glb`. Exact and
  prefix matches rank above fuzzy.
- **Faceted sidebar**: tags present in the current result set with live counts, zero-match
  tags hidden, selected tags pinned. Drill-down, not a static tag cloud.
- **Saved searches** as first-class pinnable objects.

## 8. Viewers

Each viewer must be good enough that you never open the file externally to decide whether
to use the asset.

**M16 rewrote this section against the real library rather than against an idea of one.**
What follows is what exists. Where something was reversed or dropped it says so, because a
specification that still describes an abandoned idea is worse than one that admits it. The
measurements behind each change are in `docs/spec.md`.

### 2D

- **Zoom and pan.** Fit / 100% / 200% / 400% / 800% presets, `0` fits, drag pans, and zoom
  keeps the point under the cursor fixed.
- **The wheel scrolls the page. It does not zoom.** This reverses the original line, and the
  reason is what the page is: the palette, the tags, the provenance and the format variants
  all live *below* the image, and a viewer that swallows the wheel makes them reachable only
  by grabbing the scrollbar. **Ctrl/⌘+wheel zooms**, and a **Wheel zoom** toggle makes
  plain-wheel zooming sticky for anyone who wants it back. Remembered per browser.
- **The image is centred on the stage, exactly.** Stated as a requirement because it was
  wrong for months and the bug is easy to reintroduce: CSS centring by `translate: -50% -50%`
  composed with CSS zoom by `transform: scale()` produces an offset of `(scale−1)·size/2`, so
  every 2D asset drifted right. Centring is computed in one place in JS from the measured
  stage rectangle, and panning is an offset added to it.
- **Pixels / Smooth is a switch, not a guess.** `is_pixel_art` (§6) is a heuristic and,
  measured against this library, it rejected shaded sprites — so it no longer decides what
  you see. Pixels is the default, because in a library of itch and CraftPix packs smoothing
  is the exception, and it means nearest-neighbour **snapped to whole-number zoom factors**.
  That snapping is what "looks like Aseprite" actually requires: a 3.7× nearest upscale of a
  32×32 sprite has rows one pixel taller than their neighbours, which reads as broken art.
- **Background toggle**: checkerboard, mid-grey, black, white. "You cannot evaluate a
  sprite's edges without this" is still true. A custom colour was not built and has not been
  missed.
- **Spritesheet mode**: grid overlay, frame stepping, play/pause at the stored fps, editable
  frame geometry.
- **Deferred rather than dropped**: channel isolation (R/G/B/A separately) and the 3×3 tiling
  preview. Both are texture-workflow tools and this library is overwhelmingly sprites and
  packs; build them when a PBR set arrives that needs them. The pixel colour-picker readout
  lost its argument to the palette panel below, which answers the same question exactly and
  for the whole image at once.

#### Palette panel

Extracted at derive time, stored in `palette_json`, and shown on the detail page for every
2D asset. This is a working tool, not decoration — for pixel art the palette is the thing you
actually need when authoring anything that has to sit next to the asset.

**Extraction must be exact, not approximate.** Two paths, chosen by content:

- **Exact enumeration** when the image is indexed-colour (read the PNG `PLTE` chunk directly)
  or when `color_count` is below a threshold, around 256. A 32×32 sprite has exactly fourteen
  colours, and the user needs those fourteen hex values — not a k-means approximation of them.
  Set `palette_kind=exact`. Every existing tool gets this wrong by clustering unconditionally,
  and an approximate hex is useless when the point is to match a colour precisely.
- **Quantized** (median-cut or k-means, top N with N configurable, default 16) only for
  photographic textures and large images where exact enumeration is meaningless. Label it
  clearly in the UI as approximate, and set `palette_kind=quantized`.

Alpha handling: exclude fully transparent pixels entirely, or the palette is dominated by
transparent black. Count semi-transparent pixels separately and store
`has_semitransparent` — in pixel art these are usually an authoring mistake and worth
surfacing.

**The panel is a row of circles.** One filled circle per colour, largest share first, click to
copy, hover for the hex and the percentage. That is the whole default state: no table, no
prose, no controls beyond a **Details** button. The colours are the interface, and a
twelve-colour sprite should read as twelve dots in one glance rather than as a paragraph of
hex values you have to parse.

**Details** opens the working surface underneath, and everything the original specification
asked for lives there:

- Sorting, user-switchable — **by frequency**, most-used first, and **perceptually**, hue then
  lightness, which reveals the ramp structure that is how pixel artists actually think about a
  palette. Plus a **greyscale toggle** for checking whether the value structure reads.
- Per swatch: hex, RGB, pixel count and percentage of visible pixels.

#### Copy and export

- **Click a swatch to copy.** The primary interaction, one click, with visible confirmation.
  Copy format is a preference in the Details panel: `#RRGGBB`, `RRGGBB`, `rgb(r, g, b)`,
  `r, g, b`, and **`Color(0.545, 0.227, 0.227)`** for pasting straight into GDScript.
- **Export the full palette** in the three formats this workflow uses:
  - GIMP `.gpl` — the widest-supported interchange format; Aseprite and Krita import it directly.
  - A GDScript `const` array of `Color` values.
  - A Godot `.tres` `Gradient`.

  `.txt`, `.json`, `.css` and a PNG strip were specified and built, then removed in M16. They
  existed because they were easy to write rather than because a game palette gets exported to
  CSS, and seven links in a row made the useful three harder to find. Old URLs 404 rather than
  serving a format nobody maintains.
- **Pack-level and library-level colour data stays** and drives the sidebar colour filter and
  `color:` search. The dedicated `/palettes` comparison page that displayed it was removed:
  "does this tileset sit next to that character set" turns out to be answered by clicking a
  colour in the sidebar, which is the version people reach for.

**Deployment gotcha, handled:** `navigator.clipboard` is unavailable in non-secure contexts, so
plain-HTTP LAN access (`http://meshnas.local:8973`) breaks clipboard copy in most browsers while
Tailscale HTTPS works. There is a `document.execCommand('copy')` hidden-textarea fallback, because
the single most-used interaction in this panel must not silently fail on the LAN — which is where
it is used from every day.

### 3D

- three.js as a JS island on the detail page. `OrbitControls`, grid and axis helpers,
  adjustable lighting with a few HDRI presets.
- Toggles: wireframe, flat/smooth shading, normals, bounding box, backface culling.
- Overlays: triangle and vertex counts, bounding box in metres, material list, texture list
  with resolutions.
- Animation panel: clip list, play/pause/scrub, speed.
- **Scale reference**: a human-height marker, so authored-at-wrong-scale assets are obvious
  immediately. It is labelled "1.8 m human" rather than "1.8 m", because a bare number in a
  toolbar answers no question anyone was asking.

### Audio

- Canvas waveform from stored peaks, click to seek, space to play.
- Loop toggle, visual markers at detected loop points.
- Display sample rate, bit depth, channels, duration, peak level.
- **Keyboard audition mode was removed in M16.** Not a judgement on the feature — stepping
  through 400 impact sounds with the arrow keys is genuinely better than clicking — but its
  only entry point was a sidebar "Tools" block that was deleted as clutter, and a mode nobody
  can reach is not a feature. The per-tile audio preview in the grid stayed. If it comes back
  it belongs on the grid itself, keyed to the selection, with no block to enable first.

### Grid

- **Numbered pagination**, not "Load more". `1 2 3 … next`, a page-size choice (100 by
  default), and a "showing 1–100 of 6,490" line. The original spec said "virtualised or
  paginated" and the first implementation chose an infinite cursor; the person using it asked
  for the other one, and the reason is good: *"deterministik olsun."* Page 3 of a sort is a
  place you can return to, link to, and reason about. The cursor path stays underneath for the
  API, where a stable cursor is the right answer.
- **Sort is explicit and visible**: newest first, oldest first, name, size, and file date, with
  the current order named in the control rather than implied. A grid with no stated order is a
  pile.
- Must stay responsive at 20k+ rows. Configurable thumbnail size. Hover plays animated
  previews. Full keyboard navigation. Tiles carry their own actions — open in Aseprite, Blender
  or Godot, copy the path — because the thing you want to do with an asset you have just
  recognised should not require a page load first.

## 9. Provenance and licensing

The feature that pays for itself at Steam release.

- Every pack carries source URL, site, author, license, attribution requirement, acquisition
  date, price. Assets inherit unless overridden.
- **Unlicensed packs are tagged, not blocked.** A pack ingested without confirmed licence
  information gets `license_id=NULL`, `provenance_state=needs_provenance`, and the tag
  `license:unverified`. It is fully usable and fully browsable — nothing is gated on
  provenance being complete. The backlog is resolved later, in bulk, from the licence risk
  view, and every provenance field must be editable at any time, individually or across a
  multi-selection.
- **License risk view**: everything with no license, or `commercial_ok=false`, or
  `attribution_required=true` with empty `attribution_text`. Filterable by "used in a
  project" so the urgent cases surface first.
- **`CREDITS.md` generation** driven by `project_uses`, populated by the Godot plugin (§10).
  Only assets actually used appear. Group by license then author, emit required attribution
  text and source URLs. Offer CSV export too.
- Retain the original archive's name, size, and sha256 after extraction so a purchase traces
  back to a receipt.
- Optionally keep original archives in `_archives/` rather than deleting them —
  re-downloading a delisted itch.io bundle is impossible.

## 9.1 Duplicates and cleanup

The library has accumulated over years and contains real waste: packs downloaded twice,
`__MACOSX` shadow trees, free sample packs later superseded by full versions. Reclaiming that
space is a first-class feature, not a maintenance script.

### The application never deletes anything on its own

This is the governing rule for the whole section. Ambar is a **detector and a reporter**, not
a cleaner. It finds candidates, explains its reasoning, shows what each choice would reclaim,
and then waits. Every removal is selected by a human, item by item, and confirmed explicitly.

Concretely, this rules out:

- Any scheduled or background cleanup job.
- Any "clean up my library" button that acts on a computed set.
- Any pre-checked checkbox, pre-selected row, or default-selected recommendation.
- Any automatic purging of trash, including under low-disk conditions.
- Any deletion as a side effect of ingest, scan, rescan, or re-derive.

The UI presents findings with **everything unselected**. A "select all in this finding" control
is fine — choosing two hundred things one at a time is its own kind of hazard — but it must be
a deliberate act, and the confirmation step must still list every affected path and the total
bytes before anything moves.

This is also the only part of the application that touches user data destructively, while the
rest of the spec is built on "originals are never modified". Treat it as the highest-risk
surface in the codebase and design accordingly.

### What counts as a duplicate — and what does not

Four distinct relationships, resolved differently. Conflating them makes the feature useless.

1. **Exact duplicates** — identical `sha256`. Unambiguous. Safe to act on.
2. **Moved files** — identical `sha256`, path changed since last scan, previous path now gone.
   This is a *move*, not a duplicate. Never report it as one; update the row.
3. **Near-duplicate images** — close `phash`, different bytes. Usually the same art at a
   different resolution or re-export. **Frequently intentional** (`@2x` variants, different
   tile sizes). Report as "review these", never as "delete these", and never bulk-act by
   default.
4. **Format variants** — the PNG, PSD, and ASEPRITE of the same artwork (§5.1). **These are
   not duplicates at all.** They are the point of the pack. The duplicate finder must consume
   the asset-group model and never surface variants of one group as redundant. Getting this
   wrong would suggest deleting every source file in the library.

### Resolve at pack level first

File-level lists are the wrong granularity. One craftpix pack downloaded twice under slightly
different folder names produces four hundred duplicate rows and no actionable insight.

Compute **pack similarity** from the set of member `sha256` values:

- **Identical packs** — same content hash set. Report the pair and let the user pick which one
  goes, if either.
- **Subset packs** — pack A's hashes are a strict subset of pack B's. Extremely likely in this
  library: CraftPix publishes a free sample pack and a larger paid pack of the same art, so
  `...-free-...` folders will often turn out to be fully contained in a later purchase. Report
  the containment relationship. If the user chooses to remove the subset, **transfer its tags
  and provenance onto the superset first** so nothing curated is lost, and say so before acting.
- **Overlapping packs** — high Jaccard similarity but neither contains the other. Report only,
  because the non-overlapping remainder matters and no clean answer exists.

Show reclaimable bytes per finding and sort by largest win. That number is what makes the
view worth opening.

### Junk cleanup, kept separate

A distinct view, because the risk profile is different and the volume is high:
`__MACOSX/` trees, `.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*`, zero-byte files, and empty
directories. These are safe to bulk-remove and there will be a lot of them. Also list
**orphaned derivatives** — directories under `$DATA_ROOT/derivatives/` whose `sha256` no
longer matches any asset.

### Deletion safety — non-negotiable rules

- **Nothing is ever deleted in place.** Move to `$AMBAR_TRASH_DIR`, preserving the original
  relative path so restoration is unambiguous, with a JSON record of where it came from and
  why. Purge after `AMBAR_TRASH_RETENTION`, and never purge anything younger than that even
  under space pressure.
- **Hard block on anything referenced by `project_uses`.** If a file has been imported into a
  Godot project, it is not a candidate for removal regardless of how duplicated it looks.
  Surface *why* it is blocked, naming the project.
- **Refuse to remove the last remaining copy** of any content hash. The duplicate finder may
  reduce copies to one; it may never reduce them to zero. Assert this as an invariant in code,
  not merely in the UI flow.
- **Always preview.** Show the complete list of affected paths and the total bytes before
  anything moves. No one-click "clean up everything".
- **Keep-policy heuristics are annotations, not decisions.** Alongside each copy, show the
  facts that would inform a choice — outside `raw/` or not, path depth, provenance
  completeness, `first_seen_at`, which pack it belongs to, whether a sibling variant depends on
  it. Label the copy the heuristics would favour, as a hint. Never let that hint select
  anything.
- **Offer a reviewable script as an alternative to acting in-app.** Export the user's selected
  operations as a shell script they can read, edit, and run themselves. Given the intended
  operator prefers verifying over trusting, expect this to be the primary path rather than a
  fallback.
- Every removal goes in the audit log with the reason and the finding that motivated it.

### Prefer linking over deleting

For exact duplicates, deletion is not the only way to reclaim space, and it is the only
irreversible one. Replace the redundant copy with a link to the kept copy:

- `reflink` (btrfs copy-on-write, `cp --reflink`) is the best option: space is shared, but
  editing one copy transparently diverges it from the other. Synology DSM 7 volumes are
  commonly btrfs. Probe support at startup and report it in the health endpoint.
- `hardlink` works on ext4 and reclaims the same space, but the copies are genuinely the same
  inode — acceptable here only because originals are never edited. Note the caveat in the UI.
- `off` disables linking and falls back to trash-based removal.

Both paths keep every path in the library valid and working while reclaiming the bytes.
`AMBAR_DEDUPE_LINK_MODE` selects *how* a user-initiated dedupe acts, not whether one happens:
default to `reflink` where supported, and present linking as the recommended choice ahead of
removal. Linking is still only ever triggered by an explicit selection.

## 10. API and editor plugins

### HTTP API

`/api/v1`, JSON, bearer token in `Authorization`. Versioned from day one.

```
GET    /api/v1/search?q=&tags=&kind=&limit=&cursor=
GET    /api/v1/search?…&group=1&sort=&page=  -> grouped, ordered, numbered (M18)
GET    /api/v1/sorts                       -> the orders `sort=` accepts, with labels
GET    /api/v1/assets/{id}                 -> asset + tags + variants + pack licence
GET    /api/v1/assets/{id}/thumb?size=256
GET    /api/v1/assets/{id}/preview.webp    -> full-size preview
GET    /api/v1/assets/{id}/anim.gif
GET    /api/v1/assets/{id}/sheet.gif
GET    /api/v1/assets/{id}/file            -> original bytes, ETag + Range support
GET    /api/v1/assets/{id}/preview.glb
GET    /api/v1/assets/{id}/peaks.json
GET    /api/v1/packs/{id}
GET    /api/v1/packs/{id}/download         -> zip of the pack
GET    /api/v1/tags?prefix=&namespace=     -> autocomplete
POST   /api/v1/projects/{project}/uses     -> {asset_id, res_path, sha256}
DELETE /api/v1/projects/{project}/uses/{id}
GET    /api/v1/projects/{project}/uses      -> what a project holds, with the current hashes
GET    /api/v1/projects/{project}/credits.md
GET    /api/v1/ping                        -> healthz for a token rather than a cookie
GET    /api/v1/healthz
```

`Range` support on `/file` matters: a 200 MB model download that drops should resume, not
restart.

**Search has two modes, and the request picks one.** `q`, `kind`, `tags`, `limit` and `cursor`
alone behave as they always have: one row per *file*, filename order, keyset cursor. Any of
`group=1`, `sort=` or `page=` switches to the grid's own query — one row per logical asset
(§5.1), any of the nine browse orders, numbered pages — and the response gains `grouped`, `sort`,
`page`, `pages`, `page_size`, `page_numbers` (with `0` for a gap), `first_shown` and `last_shown`,
plus `variant_count` and `group_id` per row. `next_cursor` is empty there: a client that pages by
number must not also follow a cursor, or it skips rows.

The default was deliberately *not* changed to grouped. There is one client today and it would
have been safe, but "the same request now returns different rows" is the kind of break an API
version exists to prevent, and any of three parameters opts in explicitly.

`/assets/{id}` answers with the asset, its tags, its other formats and the pack's provenance in
one response, because it backs a detail panel that opens on every selection change and three
round trips per click to a NAS is a panel that feels broken.

### Godot 4 editor plugin — `addons/ambar/`

Rewritten in M16 and **verified in Godot 4.7.1**, after the first version was installed and the
report was "hiçbir şey olmadı" — nothing happened. Four of the five bullets below changed as a
result, and the reasons are worth keeping.

- **A main-screen tab, not a dock.** `_has_main_screen()` puts "Ambar" beside 2D, 3D and Script,
  which is where a library belongs and what was actually asked for. A dock tab in
  `DOCK_SLOT_LEFT_UR` is easy to never notice.
- **The grid is a browser, not a preview strip** (M18). The first version showed five or six tiles
  across a window that fits fifteen, at one fixed thumbnail size, in one order, with "Load more"
  at the bottom and no way to look at anything without importing it first. Each of those is now a
  control:
  - `HFlowContainer`, so the number of columns is however many fit.
  - A thumbnail size picker — 64 to 256 — remembered per person in `user://ambar_prefs.cfg`,
    along with the sort, the page size, the kind filter and the inspector's width.
  - The nine browse orders, fetched from `/api/v1/sorts` rather than hardcoded, so adding one
    server-side does not need a plugin release.
  - Numbered pages — `‹ 1 2 … 57 ›` with "101–200 of 5610" — and a page size of 30 to 240. The
    page links come from the server, which already computes them for the web grid.
  - At most six thumbnail requests in flight. A page is up to 240 tiles and the server is a NAS.
- **An inspector panel beside the grid, so an asset can be judged before it is imported.** Full-size
  preview, pixel dimensions, frame count, duration or triangle count, file size, pack, licence,
  tags, and the other formats of the same artwork with the one to import selectable. Everything in
  one request. Small pictures are upscaled by a whole-number factor before display: nearest
  filtering alone still gives some source pixels eleven screen pixels and others ten, and unevenly
  scaled pixel art is the same class of failure as §6's bilinear one.
- **The plugin renders the models nobody has looked at yet, and posts them back.** §6 keeps Blender
  optional and the server has no renderer, so a model's derive writes a normalised `preview.glb`
  and never a picture; the web viewer fills thumbnails in as people open assets (M15), which left
  most of the library's models as blank tiles everywhere. The plugin runs *inside* a renderer:
  `GLTFDocument` reads that glb, a `SubViewport` draws it, and the result goes to
  `POST /api/v1/assets/{id}/thumb` — the same endpoint the browser uses, which refuses to
  overwrite an existing thumbnail. So each model is drawn once, by whoever browses past it first,
  and every viewer afterwards is served the stored image. FBX has no `preview.glb` and Godot has
  no runtime FBX importer either; those say "needs Blender on the server" rather than showing an
  empty box.
- **Settings live in the plugin's own UI, in two files.** `res://ambar.cfg` holds the base URL and
  is committed, because the server address is a fact about the studio and everyone should get it
  by checking the project out; `user://ambar_token.cfg` holds the API token and is not, for the
  same reason §11 gives. They were in Editor Settings under `ambar/base_url`, which nobody finds
  among four hundred preferences, so the default hostname stayed and every request went nowhere.
  There is a **Save and test** button that says what came back.
- **"Import"**: download into `res://assets/<kind>/<pack-slug>/<filename>`, preserving relative
  structure for multi-file assets — a model and its textures must land together with intact
  relative paths or materials break. Then rescan the editor filesystem.
- **Set import defaults, do not write `.import` files.** The original bullet said to write them by
  hand before triggering a reimport; Godot owns those files, keys them to its own version, and
  overwrites or errors on a partial one. The supported mechanism is `importer_defaults/texture`
  plus `rendering/textures/canvas_textures/default_texture_filter`, set once for the project by a
  **Set pixel-art import defaults** button: lossless, no mipmaps, nearest filtering, no automatic
  switch to VRAM compression.
- **Project identity is a UUID, never a filesystem path.** On first use the plugin generates one
  into `res://.ambar/project.json` and commits it. Two people have the project checked out at
  different paths; keying `project_uses` on a path registers the same project twice and splits
  the credits list in half.
- **`res://.ambar/manifest.json`** maps `asset_id → {res_path, sha256, filename, pack}` and is
  committed. It is the input to credits generation, the answer to "where did this file come from"
  two years from now, and — because it travels through git — how each person's editor knows what
  the others already imported. Merge additively; never rewrite the whole file from one client's
  view of the world.
- **An "In this project" screen, beside the library one** (M18). All of the above was true and
  none of it was visible: the only trace of an import was a tile going grey somewhere in a search.
  The screen is the manifest and `GET /projects/{uuid}/uses` side by side, one row per asset, and
  the disagreements between them are the content:
  - *library has a newer version* — the recorded hash is not the library's current one. An
    **Update** re-downloads over the same `res://` path, so scenes pointing at it keep working.
  - *not recorded on the server* — in the manifest, no use row: an import made while the server
    was unreachable. **Sync** replays those, which is the reconcile the offline tolerance above
    has always promised and never had a trigger for.
  - *missing from this project* — the manifest describes a file that is no longer in the checkout.
  - *gone from the library* — the asset it came from is missing at the source (§12).
  - **Remove** deletes this project's copy, forgets the manifest entry and deletes the use row,
    behind a confirmation naming the file. The library is never touched — that is §9.1's business
    and it has its own selection and preview.
  The credits action lives here too, beside the thing it describes.
- `project_uses` is deduplicated on `(project_id, asset_id, res_path)`, so two people importing
  the same asset independently produces one row, not two.
- POST each addition to `/projects/{project}/uses`, and **tolerate being offline**: the manifest is
  committed, so a later reconcile can replay it. The import reports "the server was not told"
  rather than failing.
- **Every failure comes back as a sentence.** "HTTP 401" in a panel nobody has open is how "the
  plugin does nothing" happens; "unauthorised — the API token is missing or wrong (Settings → API
  tokens in Ambar)" is a thing somebody can act on.
- Badges in the grid: "already in project" from the manifest, "outdated" when the library sha256
  differs from the imported copy. A **Generate CREDITS.md** action writes the file into the project.

**Testing an editor plugin is not optional and does not need a display.** The actual cause of
"nothing happened" was a GDScript *parse* error — `var data := f()` where `f` has no declared
return type — which fails the compile of every script that preloads that file, leaving an addon
that is enabled and inert with the only evidence in the Output panel. `godot --headless --editor
--quit` surfaces exactly that in about twenty seconds, and `godot --headless --script` can drive
the API client against a running server. Both belong in the loop before a plugin ships.

M18 made that a suite rather than a habit: `godot-test/` is a project whose `addons/` is a symlink
to the working copy, and `make godot-test GODOT=…` runs three passes — the parse check, an API
drive, and an import driven through the panel's own button that then checks the file landed, the
manifest recorded it and the server was told. A fourth pass renders the panel to a PNG, which is
the only one that needs a display. See `godot-test/README.md`, including the two GDScript traps
that cost the most time: lambdas capture locals by value, and the root Window is not in the scene
tree until the first frame.

**Other engines.** The API is the integration surface, and nothing in it is Godot-specific: an
Unreal plugin would speak the same endpoints, keep the same UUID-in-a-committed-file identity, and
write the same manifest. Not built yet.

## 11. Auth and security

Not optional, and not deferrable to a later milestone. Tailscale is the daily path, but the
moment Funnel or Cloudflare is switched on the app is public with **no edge rate limiting**,
and scanners will find it.

- Session cookies: `HttpOnly`, `Secure`, `SameSite=Lax`, server-side session rows, rotation
  on login, absolute and idle expiry.
- Passwords: `argon2id`. **No self-registration.** Users created via `ambar user add` or a
  first-run bootstrap that disables itself afterwards.
- Login rate limiting by client IP and by username. Constant-time comparison. No
  user-enumeration difference between "no such user" and "wrong password".
- API tokens: store only a hash; show plaintext once at creation; scopes (`read`, `write`,
  `admin`), expiry, revocation, `last_used_at` so stale tokens are visible.
- CSRF tokens on all state-changing posts. Configure `hx-headers` globally for htmx.
- Path traversal defence on every filesystem operation, ingest and serving alike. Never build
  a path from user input without resolving it and confirming it is still under the configured
  root.
- Serve library content with `Content-Disposition: attachment` and
  `X-Content-Type-Options: nosniff`. An uploaded `.html` or `.svg` served inline from the app
  origin is stored XSS.
- Trusted-proxy CIDR list, empty by default. Never blindly trust `X-Forwarded-For`.
- Audit log: logins, token creation, deletions, metadata edits.

### Two equal users

Both people browse, upload, tag, and import into Godot. There is no meaningful privilege
split, so keep the `role` column but ship exactly one role plus an implicit owner. Do not
build a permission system for two people who trust each other.

What does need care is concurrency:

- **Serialize `.ambar.json` writes per pack.** Two people tagging the same pack at the same
  time must not produce a lost update or a half-written sidecar. Take a per-pack lock, write
  to a temp file, `fsync`, then rename atomically.
- Tag edits are additive operations (`add tag X`, `remove tag Y`), never "here is the new
  complete tag set for this asset". The latter loses the other person's concurrent work.
- `asset_tags.created_by` records who applied a tag. Cheap, and it answers "did you tag this
  or did I" without building a full history.
- Each person's Godot plugin uses its **own** API token, so a compromised or stale token can
  be revoked without disrupting the other.

## 12. Operations

- **`ambar scan`** — full library walk. New files indexed; missing files marked
  `missing_since` and **never hard-deleted**, because a NAS share can be temporarily
  unmounted and destroying the index over that would be catastrophic; changed hashes flagged
  for review. Runnable from the UI and on a schedule.
- **One scheduled job, and it runs at night.** `AMBAR_NIGHTLY_SCAN` defaults to 05:00 local
  and is the *only* thing the application starts on a timer. "Sürekli arkada bir şey
  çalışmasın" is a hard requirement on a box that also serves files, not a preference: a
  background walk during the working day is indistinguishable from the NAS being broken.
  Set it to `off` to disable it entirely.
- **Re-scanning from the UI happens in place.** The button starts the job and the page stays
  where it is, showing progress and phase ("checking files", "matching moved files", "writing
  the index") and, when idle, when the last scan finished. A button that navigates you
  somewhere else to watch a spinner is a button people stop pressing.
- **`ambar verify`** — re-hash all or a sample; detect bit rot and truncated files.
- **`ambar rebuild-index`** — drop and reconstruct the DB from the filesystem and sidecars.
  This must actually work. Test it.
- **`ambar backup`** — `VACUUM INTO` a timestamped copy. **Take backups at the SQLite level,
  not via btrfs/ZFS snapshots** — a filesystem snapshot of a live WAL database can be
  inconsistent.
- Health endpoint: DB reachable, library mount present and readable, derivatives dir
  writable, job queue depth, failed job count, Blender availability.
- Structured logging via `log/slog`. Job failures inspectable in the UI, not only in
  container logs — a silently failing derivative pipeline is easy to miss for weeks.
- Graceful shutdown: finish or requeue in-flight jobs, close the DB cleanly.
- Worker concurrency configurable, defaulting **low**. This is a NAS with a weak CPU running
  other services; derivative generation must not starve them or freeze the UI.
- **Idle must mean idle.** The complaint that opened M16's CPU work was "çok fazla CPU
  harcıyor, neden?" and the answer was not the workers — it was every page rebuilding the
  faceted sidebar (six aggregate queries over 6,500 assets, ~157 ms of them in one colour
  query) on every request, including navigation between two grid pages. It is cached with
  stale-while-revalidate behind a single-flight guard now. The rule this leaves behind: an
  aggregate over the whole library belongs in a cache with an explicit invalidation, never in
  a request path, and page rendering must not carry columns the page does not display.

## 13. Configuration

Environment variables with a documented `.env.example`:

```
AMBAR_LIBRARY_ROOT=/library
AMBAR_DATA_ROOT=/data
AMBAR_LIBRARY_READONLY=false
AMBAR_BIND=0.0.0.0:8080
AMBAR_BASE_URL=http://nas:8080
AMBAR_TRUSTED_PROXIES=                          # empty = ignore forwarded headers
AMBAR_REAL_IP_HEADER=
AMBAR_WORKERS=2
AMBAR_MAX_UPLOAD_SIZE=0                         # 0 = no cap (default); upload streams to disk
AMBAR_MAX_ARCHIVE_UNCOMPRESSED=21474836480
AMBAR_MAX_ARCHIVE_ENTRIES=200000
AMBAR_MAX_IMAGE_PIXELS=50000000
AMBAR_KEEP_ARCHIVES=true
AMBAR_INBOX_POLL_INTERVAL=30s
AMBAR_NIGHTLY_SCAN=05:00                        # local time; "off" disables it
AMBAR_IGNORE_GLOBS=                             # empty = the §5.1 defaults
AMBAR_LIBRARY_BUCKETS=                          # empty = the §17 defaults
AMBAR_BACKUP_INTERVAL=1h                        # empty disables the internal scheduler
AMBAR_BACKUP_DIR=/data/backups
AMBAR_BACKUP_KEEP=48                            # rotate, oldest first
AMBAR_TRASH_DIR=/library/_trash
AMBAR_TRASH_RETENTION=                          # empty (default) = never auto-purge; manual only
AMBAR_DEDUPE_LINK_MODE=reflink                  # reflink | hardlink | off
AMBAR_ASEPRITE_BIN=                             # optional, bind-mounted
AMBAR_BLENDER_BIN=                              # optional, or runtime-downloaded
AMBAR_LOCAL_LIBRARY_PATH=                       # where the library is mounted on the *client*,
                                                # for the ambar:// open-in-app helper
AMBAR_COOKIE_SECURE=                            # empty = infer from AMBAR_BASE_URL
AMBAR_SESSION_SECRET=
```

**On the upload cap.** It was 100 MB, and it was wrong the moment somebody dragged a real
itch.io pack onto the page. The number existed because the first implementation buffered the
whole request body through `TMPDIR` before it knew what it had; the upload streams straight
into `_inbox` in constant memory now, so the ceiling protected nothing and blocked the normal
case. Default is no cap. A cap is still honoured when set, which is what an instance exposed
beyond the LAN should do.

Ship a `docker-compose.yml`: library bind-mounted, data volume on a local NAS volume, worker
count and poll interval commented.

## 14. Build order

Ship something usable early. Do not build all of it before first run.

| Milestone | Deliverable |
| --- | --- |
| **M0** | Go skeleton, SQLite + migrations, config, auth, sessions, Dockerfile, healthz. **Verify FTS5 works with `modernc.org/sqlite` before building on it.** |
| **M1** | `scan` walks the library, indexes files, classifies kinds. Grid view with filenames, no thumbnails. FTS filename search. Already useful. |
| **M2** | Job queue + workers. Image thumbnails with the pixel-art path. **Asset group / format-variant collapsing (§5.1) and the `.aseprite` decoder** — both are needed before the grid is worth looking at, given what the library actually contains. 2D viewer: zoom, pan, background toggle. |
| **M3** | Tags: namespaces, hierarchy, aliases, autocomplete, bulk tagging, auto-tag from path. Faceted sidebar. Query language parser. |
| **M4** | Ingest: `_inbox` polling, zip/rar/7z extraction with path sanitisation, provenance form, pack model, sidecar read/write. |
| **M5** | Audio: peaks, canvas waveform, keyboard audition mode. |
| **M6** | 3D: glb normalisation, three.js viewer, metadata extraction, Blender-optional turntables. |
| **M7** | Spritesheets: grid detection with confirmation UI, animated previews. |
| **M8** | API + tokens. |
| **M9** | Godot plugin: search, add to project, import presets, manifest. |
| **M10** | Licensing: license risk view, `CREDITS.md` generation. |
| **M11** | Ops: verify, rebuild-index, backup, missing-file handling. |
| **M11.5** | Palette extraction, palette panel, copy and export formats. Small, self-contained, and immediately useful every day. Fold it in earlier if it is wanted sooner — it depends only on M2. |
| **M12** | Junk view (`__MACOSX`, empty dirs, orphaned derivatives) — reporting only, manual selection. Low risk, high volume, immediate payoff. |
| **M13** | Duplicates: exact-hash detection, pack similarity and subset detection, advisory keep-policy annotations, trash staging, preview, reflink/hardlink dedupe, script export. Ship the safety invariants and their tests in the same milestone as the removal path, never after it. |
| **M14** | Fonts: specimen rendering, per-family grouping, the licence questions fonts raise more sharply than anything else in the library. |
| **M15** | Workspace: saved searches, the removal list as a persistent selection rather than a one-shot form. |
| **M16** | **The review milestone.** The library was loaded with a real 6,500-asset collection and handed to the people who use it, and most of what came back was not a missing feature — it was the design. Rewritten: the visual language, the shell and its navigation, the 2D viewer, the asset page, the grid (pagination and sort), search autocomplete, upload, background-job visibility, the `ambar://` open-in-app helper, and the Godot plugin. Removed: `/palettes`, `/provenance`, four palette export formats, the sidebar Tools block. Measured and fixed: NAS CPU. Nothing here was speculative; every item traces to somebody trying to use the thing. |
| **M18** | The Godot plugin's browse: reflowing grid, thumbnail size, the nine sort orders, numbered pages, and an inspector panel that shows an asset — preview, metadata, tags, formats, licence — without importing it. `/api/v1/search` grew a grouped, sorted, numbered mode to serve it, and `godot-test/` grew a `make` target so the plugin has a suite rather than a habit. |

## 15. Decisions that were open before the code existed

All five are settled; kept as a record of what was decided and why, because each of them still
constrains the codebase. `docs/spec.md` has the reasoning at length.

1. **FTS5 works in `modernc.org/sqlite`.** Verified in M0 before anything was built on it, which
   is the only reason there was never a Dockerfile crisis. No CGO, as invariant 6 requires.
2. **`html/template`, not `templ`.** No codegen step, no second toolchain in the image, and the
   templates stay readable to anybody who knows Go. The type safety was not worth the build.
3. **Vendored ES modules, no bundler.** three.js is served directly from `/static/vendor`. The
   ergonomics held, and a CSP of `default-src 'self'` with no `unsafe-inline` is far easier to
   keep honest without a build step in the way.
4. **`phash` landed in M13 with the duplicates work, not in M2.** It is cheap to compute at derive
   time but expensive to *use*: clustering is quadratic, so it is capped by a configurable image
   count and skipped with an explicit message above it rather than quietly running for an hour.
   And near-duplicate findings are review-only — "these two sprites are 94% alike" is a question
   for a human, never grounds for the application to propose a removal (§9.1 rule 3).
5. **Spritesheet grid detection** scores candidate cell sizes against transparent gutters and
   repeated content, proposes the best, and shows a confirmation UI where the geometry is editable
   — a wrong guess is corrected in place, never silently applied.

## 16. Quality bar

- Tests where bugs are expensive and silent: the search query parser, path sanitisation,
  archive extraction against malicious inputs, spritesheet grid detection, scan
  reconciliation, and `rebuild-index` fidelity.
- Table-driven Go tests. Real fixture files for every decoder, including deliberately broken
  ones.
- The UI must stay usable at 20,000 assets. Generate a library that size and test against it
  rather than discovering the problem in production.
- No panics reaching the HTTP layer. Recover middleware exists, but a recovered panic is a
  bug to fix, not a handled case.
- **Originals are never modified.** Add a test that hashes the entire library before and
  after a full ingest-and-scan cycle and asserts nothing changed.
- **Measure before optimising, and measure the real library.** Every performance claim in
  `docs/spec.md` has a number next to it, and the numbers repeatedly contradicted the
  guess: the CPU complaint was the sidebar, not the workers; 98 ms of a 112 ms query was
  Go-side, and 55% of *that* was a JSON column the grid never displays. A 6,500-asset copy of
  the real library is the fixture; a synthetic one hides exactly the cases that matter.
- **Anything with a runtime outside Go gets exercised in that runtime before it ships.** The
  Godot plugin was written twice, and the second version was still broken in a way no Go test
  could see — a parse error that left the addon enabled and inert. `godot --headless --editor
  --quit` finds it in twenty seconds without a display. The same applies to the `ambar://`
  helper (run it against stub applications) and to the rendered pages
  (`firefox --headless --screenshot`, twice, because the first capture races image decoding).
  "Verified" means it ran, not that it reads correctly.
- Deletion invariants get dedicated tests, written before the delete path ships: that the
  last copy of a hash can never be removed, that `project_uses`-referenced files are always
  blocked, that format variants are never proposed as duplicates, that a move is never
  reported as a duplicate, that nothing is ever selected or acted on without an explicit user
  choice, and that everything in trash can be restored to its exact original path. These are
  the tests that prevent an afternoon of silent data loss.

## 17. Target deployment

The first and primary deployment is a Synology NAS reachable as `meshnas.local`, with the
asset library already living at `/volume2/game/assets`. Nothing here is specific enough to
belong in the code — it belongs in the README and the `docker-compose.yml`.

### Paths

| Host path | Container path | Contents |
| --- | --- | --- |
| `/volume2/game/assets` | `/library` | The existing library. Left exactly as it is. |
| `/volume2/game/ambar` | `/data` | `ambar.db`, `derivatives/`, `tools/`, `logs/`, `backups/` |

Both are on the same volume, so one NAS-level backup job covers both. `/data` is
deliberately a sibling of the library rather than a child of it: derivatives will grow to
several GB, and the database must not be inside the tree that `scan` walks or that a
file-level backup copies while it is being written.

The container **needs write access to `/library`**, not only read. Originals are never
modified, but `.ambar.json` sidecars are written beside them, and `_inbox` ingest extracts
into the tree. The read-only mode from §3 exists as an escape hatch, but it writes sidecars
into `/data` instead, which loses the main benefit — metadata travelling with the folder.
Default to read-write.

### Library top-level layout

The layout that emerged in use, which is not quite the one first sketched:

```
/mnt/game-assets/            (or /volume2/game/assets on the NAS itself)
    2d/          <pack>/ ...
    3d/          <pack>/ ...
    aseprite/    <pack>/ ...
    fonts/       <pack>/ ...
    sounds/      <pack>/ ...
    raw/         packs that are a bit of everything, and old downloads
    _inbox/      drop zone: anything landing here is ingested
    _archives/   the original zips, kept
    _quarantine/ archives that failed extraction
```

`sounds` rather than `audio`, and `raw` promoted from "the untidy corner" to the honest
destination for a pack that does not sort cleanly.

The bucket list (`AMBAR_LIBRARY_BUCKETS`, defaulting to `2d 3d mix raw audio`) names only the
directories whose *children* are packs. `sounds`, `aseprite` and `fonts` hold loose files
rather than pack directories, so they are correctly detected as packs in their own right and
must **not** be added to the list — doing so would scatter their contents into the synthetic
standalone pack. Add a name to the list the day it grows pack subdirectories, and not before.
This is the one place the code knows anything about the human layout, and it is configuration
precisely so §17's "must not depend on this layout" stays true.

Packs are extracted from their archive and dropped into whichever bucket fits. These
directories are for the human browsing over SMB; the application derives nothing essential
from them and **must not depend on this layout**. Pack detection is marker-based and
depth-agnostic (§5.1), so the buckets can be renamed or abandoned later without a migration.

**Upload asks rather than guesses.** Dragging a zip onto the web UI proposes a destination —
by reading the archive's contents and picking the bucket that holds two thirds of them, or
`raw/` when nothing dominates — and then lets it be changed, including to a folder created on
the spot. A single level, deliberately: "sadece 2d olsun", not `2d/characters/humans`. The
proposal is a convenience; the human makes the call, which is the same principle §9.1 applies
to removal.

Underscore-prefixed directories are reserved and never treated as buckets or packs. That is
also the security boundary for ingest: a destination arriving from the browser is resolved to
an absolute path and confirmed to sit under `_inbox` before anything is moved, because a
prefix check alone accepts `_inbox/../2d/pack` and would move a library file — violating
invariant 1 by way of a text field.

Run as the NAS user that owns the library, never as root. Files extracted from `_inbox` by a
root container are owned by root and cannot be edited or deleted over SMB afterwards.

```
ssh datcal@meshnas.local
id                       # note the uid and gid
```

```yaml
services:
  ambar:
    user: "1026:100"     # substitute the real values
```

On Synology, human users typically start at uid 1026 and the `users` group is gid 100.
Document in the README that this must be set, and fail loudly at startup if `/data` is not
writable rather than limping along.

### Port

DSM occupies 5000/5001, and several Synology packages take 8080. Pick something unlikely to
collide — 8973 or similar — and make it a compose-level mapping so it is easy to change.

```
sudo docker ps
sudo netstat -tlnp
```

### Compose sketch

```yaml
services:
  ambar:
    image: ghcr.io/<owner>/ambar:latest
    container_name: ambar
    user: "1026:100"
    restart: unless-stopped
    ports:
      - "8973:8080"
    volumes:
      - /volume2/game/assets:/library
      - /volume2/game/ambar:/data
    env_file: .env
```

DSM 7.2+ ships Container Manager, which supports compose files directly through the UI as
"projects". Document both that path and plain `docker compose up -d` over SSH.

### Backups

The internal scheduler from §13 is the default and the recommended path: a ticker in-process
that runs `VACUUM INTO $AMBAR_BACKUP_DIR/ambar-<timestamp>.db` and rotates to
`AMBAR_BACKUP_KEEP` copies. Roughly thirty lines of Go, cannot be broken by a DSM update,
cannot be forgotten, and the application already knows the correct way to snapshot its own
database.

`ambar backup` remains available as a CLI command for manual and external use. If an
external schedule is preferred, use **DSM Task Scheduler** running
`docker exec ambar /ambar backup` — do **not** hand-edit the Synology crontab, since DSM
updates can overwrite it.

Because sidecars hold the authoritative metadata, the full recovery story is: restore
`/volume2/game/assets` from the NAS backup, run `ambar rebuild-index`, and let derivatives
regenerate. The database backups exist to make recovery fast, not to make it possible.

### Publishing

The image is intended to be public on GitHub Container Registry.

- Build and push via GitHub Actions on tag. The target NAS is `x86_64`, so `linux/amd64` is
  the only required platform. Adding `linux/arm64` costs little and covers ARM-based Synology
  models for other users, but is not needed for this deployment.
- Because the target is x86_64, the server-side Blender Workbench turntable path in §6 is
  viable and should be the primary one. The client-side canvas-capture fallback stays useful
  but is not load-bearing.
- **Generate `AMBAR_SESSION_SECRET` on first run** if unset, persisting it to `/data`.
  Requiring the user to invent one is friction that ends in a weak or shared value.
- `.gitignore`: `.env`, `/data`, backups. No real hostnames, paths, or usernames in the
  repository — `.env.example` uses placeholders only.
- A public repository means the code is readable by anyone who finds the instance. This is
  normal and fine, but it is a further reason §11 is a hard requirement rather than a
  nice-to-have.
- Pick a licence deliberately: MIT if the goal is maximum reuse, AGPL-3.0 if hosted
  commercial forks are unwelcome.

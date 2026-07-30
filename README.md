# Ambar

Self-hosted game asset library. Go + SQLite + htmx, single static binary, single
Docker container, built to run on a Synology NAS.

Indexes a large library of downloaded game assets — 2D sprites, 3D models,
audio — makes them searchable by tag, tracks provenance and licensing, and serves
a Godot editor plugin.

The full specification is in [docs/spec.md](docs/spec.md). Work is organised by
the milestones in §14.

## Status

**Every milestone in §14 is complete (M0 through M13).** What exists today, in the
detail the first three milestones were written up with:

- Configuration from the environment, with validation that fails at startup
- SQLite with WAL, a single writer connection and a separate read pool
- Embedded, forward-only migrations
- Authentication: argon2id passwords, server-side sessions, login rate limiting,
  CSRF tokens
- `/healthz` (public liveness) and `/api/v1/healthz` (authenticated detail)
- **`ambar scan`** — depth-agnostic pack detection, junk exclusion, classification,
  hashing, and reconciliation that recognises moves and never deletes a row
- **A searchable grid** of thumbnails with FTS5 filename and pack search, kind
  filters, keyset pagination, and original-file download
- **A background job queue** with retries, crash recovery and a status page — so a
  scan can be started from the UI without a long-running request
- **Thumbnails and previews** with the pixel-art path: nearest-neighbour resizing for
  pixel art, composited over mid-grey, plus a transparent variant and an animated GIF
  for multi-frame sources
- **Format-variant collapsing** — the PNG, PSD and ASEPRITE of one artwork are one
  grid tile, with the sources listed on the detail page
- **An `.aseprite` decoder** reading the binary format directly, including frame tags,
  durations and layer compositing
- **A 2D viewer**: zoom, pan, and a background toggle
- `ambar user add` / `ambar user list`
- Container image and compose file

Measured at 20,000 assets: first scan 2.9s, rescan 157ms with nothing re-hashed, and
both the asset grid and the group grid stay flat as you page — a page 10,000 rows deep
costs the same as the first.

Since then: tags with namespaces, hierarchy, aliases and the query language (M3);
`_inbox` ingest with archive extraction and provenance capture (M4); audio peaks and
keyboard audition (M5); glb normalisation and the three.js viewer (M6); spritesheet grid
detection (M7); the JSON API and tokens (M8); the Godot editor plugin (M9); licence risk
and `CREDITS.md` (M10); `verify`, `rebuild-index` and backups (M11); palette extraction
(M11.5); the junk view (M12); and duplicates with the removal path (M13):

- **`ambar dupes` and `/dupes`** — exact-hash duplicates, pack similarity (identical,
  contained, overlapping) computed from member hash sets, and near-duplicate image
  clusters reported as "review these" and never proposed for removal
- **A removal path built around refusing** — nothing is pre-selected, every selection is
  previewed in full before it moves, an asset a Godot project uses is a hard block, and
  the last live copy of a content hash can never be removed
- **A trash rather than a delete** — removals move to `AMBAR_TRASH_DIR` keeping their
  relative path, with a JSON record of where each came from and why; `/trash` restores
  without ever overwriting, and purging is manual, never scheduled
- **Linking preferred over deleting** — `reflink` (btrfs copy-on-write, via `FICLONE`)
  or `hardlink` reclaims the same space while every path keeps working, probed at
  startup and reported in the health endpoint
- **A reviewable shell script** as an alternative to acting in-app: the same operations,
  quoted, commented with the reasoning, and listing what Ambar refused
- **Colour search** — `color:#8b3a3a` finds assets containing that colour within a
  tolerance you can widen (`~40`) or close (`~0`), and `palette-near:<asset_id>` finds
  assets whose palette shares a majority of another's dominant colours
- **A pack palette consistency view** at `/palettes` answering §7's daily question —
  "does this tileset sit next to that character set" — as two coverage numbers, the
  colours behind them, and a sentence

### The interface (M14)

The library is the front door: `/` is the grid, in a three-pane workspace — folder tree
and filters on the left, thumbnails in the middle, the asset's tags, palette, facts and
actions on the right. Forms and reports stay centred documents.

- **A folder tree with rolled-up counts**, derived from the index, filtering the grid to
  any subtree
- **A thumbnail size slider** (⌘/Ctrl +/− too), and small pixel art that fills its tile
  instead of sitting in the middle of it as a speck
- **3D in the browser for `.obj`, `.fbx` and `.gltf`** through vendored three.js loaders,
  with the model's `.mtl`, `.bin` and textures served beside it — only `.blend` still
  needs Blender, and only for a grid thumbnail
- **"Open in…"** — the asset's path as *your* machine sees it
  (`AMBAR_LOCAL_LIBRARY_PATH`), one click to copy, plus the Godot projects already using
  it. A browser cannot launch Aseprite for you and the server runs on the NAS; this is
  the honest version of that feature
- **A spritesheet player** that says what frame detection is for: pause, step, frame
  counter, fps, grid overlay
- Documentation, archives and model companion files (`.mtl`, `.bin`) are no longer
  indexed as assets — on the target library that is 540 files that stopped being grid
  tiles

### The interface, second pass (M15)

- **Kinds, colours and tags first; folders on demand.** The sidebar leads with a re-scan
  action, the kinds, the library's own dominant colours as clickable filters, and the
  tags. The folder tree is there but starts collapsed
- **Colour search where you see the colour**: ⌕ on every swatch searches the library for
  it, ⧉ copies it
- **A preview for every kind**: waveform tiles for audio, rendered specimens for fonts
  (with a detail page you can type your own text into), and 3D thumbnails rendered by the
  browser once and cached server-side — no Blender required
- **“Open in Aseprite / Blender / Godot”** through an `ambar://` link plus a one-time
  helper you install and can read first (Settings). The copyable local path stays for
  before you install it
- **Add a user from Settings** — §11 still forbids self-registration, so it is behind
  auth and audited
- Fit scales small images *up* (integer factors for pixel art), so a 20×21 tile no longer
  opens at 20×21

### Known gaps

- **`.tga` and `.xcf` have no decoder.** Both are recorded as
  `derive_state=unsupported` with a reason, visible in the UI, rather than failing
  silently. See `docs/decisions.md`.
- **Blend modes other than Normal are approximated** as Normal when compositing
  Aseprite layers, and say so in the job log. No file in the 72-file corpus measured
  below uses one, so there is nothing to implement against yet.

### The `.aseprite` decoder is measured against real files

The decoder's fixtures are built to the documented format, which tests it against the
spec. That was not enough: every fixture used opaque colours, and a compositing bug that
only affected semi-transparent pixels passed all of them for two milestones.

`internal/aseprite/corpus_test.go` decodes real Aseprite-authored files from a directory
named by `AMBAR_ASEPRITE_CORPUS` and, where the pack also ships the vendor's own PNG
export of the same artwork, compares them pixel by pixel:

```
AMBAR_ASEPRITE_CORPUS=/volume2/game/assets go test ./internal/aseprite/
```

Nothing is vendored — the corpus available here is non-redistributable — so the test
skips when the variable is unset. On a 72-file corpus, 70 files match the vendor's export
byte for byte; the tolerance for the other two is narrow and explained in the test.

### FTS5

§15 item 1 is resolved. `modernc.org/sqlite` v1.55.0 (SQLite 3.53.3) ships FTS5
enabled, including the external-content form §4 specifies, `bm25()` ranking and
the `trigram` tokenizer — all with `CGO_ENABLED=0`, so the static binary and the
no-CGO invariant hold. No `mattn/go-sqlite3`, no CGO.

`internal/db/fts5_test.go` keeps those assertions permanently, because a
dependency bump that silently dropped FTS5 would break search with no other
signal.

## Building

Requires Go 1.26 or newer. No CGO, no C toolchain, no bundler.

```sh
make build      # -> dist/ambar
make test       # all packages
make test-race  # the rate limiter and session store are shared mutable state
make check      # gofmt + vet + test, what CI runs
make docker     # container image
```

`GO`, `GOFMT` and `DOCKER` are overridable if the toolchain is not on `PATH`:

```sh
make test GO="/usr/local/go/bin/go"
make docker DOCKER=podman
```

`make test-race` needs cgo and a C compiler, and overrides `CGO_ENABLED=0` for
that target alone. That does not weaken the no-CGO invariant: `make build` and
the Dockerfile both stay at `CGO_ENABLED=0`, and the Docker build fails outright
if the resulting binary is dynamically linked.

## Running locally

```sh
make user-add USERNAME=yourname   # first run only; there is no default account
make scan ARGS=--dry-run          # see what would be indexed, without writing
make scan                         # index ./testdata/library
make derive                       # generate thumbnails and previews
make run                          # serve on :8080
```

`make run` also generates derivatives in the background, so `make derive` is only
needed for a one-shot pass outside the server.

Then open <http://localhost:8080/>.

### Scanning

`ambar scan` is safe to re-run and is the only way to update the index in M1. A UI
trigger needs the job queue, which is M2 — invariant 8 forbids doing scan work in an
HTTP handler.

What it guarantees, and what the tests pin down:

- **Nothing in the library is modified.** A test hashes every file before and after
  a full scan cycle and asserts the set is byte-identical, mtimes included.
- **A moved file is recognised as a move**, keeping its row and its id, never
  becoming a second row (§9.1 rule 2).
- **A missing file is marked, never deleted** (§12). Restore the file, re-scan, and
  the entry comes back intact.
- **A library that looks unmounted is refused.** If the walk finds no files while
  the index holds thousands, the scan aborts rather than flagging everything —
  §12 calls destroying the index over a temporarily-absent share catastrophic.
- **Unchanged files are not re-read.** Change detection is `(size, mtime)`;
  re-hashing everything is `ambar verify`'s job (M11).

How the library is interpreted is configurable, because §17 requires the code not to
depend on the human-facing folder layout: `AMBAR_IGNORE_GLOBS` for junk (leaving
`__MACOSX` out of it roughly doubles the apparent asset count) and
`AMBAR_LIBRARY_BUCKETS` for the organisational parents that are not themselves
packs. A `.ambar.json` sidecar always overrides the heuristics.

### Thumbnails and background work

Scanning and thumbnail generation both run through a job queue, which is what lets the
UI start a scan without holding a request open. `/jobs` shows what is queued, running
and failed, with the error text — a failing preview pipeline should be obvious rather
than something you notice weeks later.

Three states are worth distinguishing on that page:

- **failed** — something went wrong and is worth retrying. Retryable from the UI.
- **unsupported** — there is no decoder for the format, so retrying changes nothing
  until the code does. Not an error.
- **pending** — queued, not yet generated.

Pixel art is resized with nearest-neighbour rather than smoothed. That is the single
thing §6 is most insistent about, and it is detected from colour count and edge
hardness rather than image size, so a large pixel-art tileset atlas is handled
correctly — see `docs/decisions.md` for the measurements behind the thresholds.

Configuration is entirely by environment variable. See
[.env.example](.env.example), which documents every one.

## Deploying

Copy `.env.example` to `.env`, adjust, and use
[docker-compose.yml](docker-compose.yml).

Three things that are easy to get wrong:

**Set `user:` to the uid/gid that owns the library.** Not optional. Files
extracted from `_inbox` by a root container are owned by root and cannot be
edited or deleted over SMB afterwards. Find the values with `ssh <user>@<nas> id`
— on Synology, human users typically start at uid 1026 and the `users` group is
gid 100. If the container cannot write to `/data` it refuses to start, which is
how a wrong value here surfaces immediately.

**Keep the data root on a real local volume, and outside the library tree.** WAL
corrupts on network filesystems, so never point `/data` at another machine's
SMB/NFS share. A Synology `/volume2/...` path is a local volume and is fine —
SMB is only how a desktop reaches it from outside. Keeping it a sibling of the
library also means `scan` never walks the database and a file-level library
backup never captures a live WAL database mid-write.

**Pick a host port that does not collide.** DSM occupies 5000/5001 and several
Synology packages take 8080. Check with `sudo netstat -tlnp`.

Then create the first user:

```sh
docker exec -it ambar /ambar user add <username>
```

Or non-interactively:

```sh
printf '%s' "$PASSWORD" | docker exec -i ambar /ambar user add <username> --password-stdin
```

### Running the image with rootless podman

Only relevant for local testing; the deployment target is Docker. Under rootless
podman, `--user` alone maps to a subuid that does not own the bind mount, so the
container correctly refuses to start. Add `--userns=keep-id`:

```sh
make docker-run DOCKER=podman ROOTLESS_FLAGS=--userns=keep-id
```

Docker passes host uids straight through and needs neither flag, which is why
`docker-compose.yml` only sets `user:`.

## Security notes

Authentication is required and is not deferrable. The daily access path is a
Tailscale tailnet, but the moment Tailscale Funnel or a Cloudflare Tunnel is
switched on, this application is on the public internet with no edge rate
limiting — see §11.

One deliberate deviation from the spec is worth knowing about. §11 mandates the
`Secure` attribute on the session cookie; a `Secure` cookie is never sent over
plain HTTP, and §8 documents plain-HTTP LAN access (`http://nas:8973`) as a real
path, so an unconditional flag would make LAN login impossible. `Secure` is
therefore derived from `AMBAR_BASE_URL`'s scheme, overridable with
`AMBAR_COOKIE_SECURE=auto|true|false`, and a warning is logged at startup when it
is off. **Set `AMBAR_COOKIE_SECURE=true` once access is HTTPS-only.**

## Licence

Not chosen yet. §17 frames it as MIT for maximum reuse versus AGPL-3.0 if hosted
commercial forks are unwelcome.

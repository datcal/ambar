# Ambar

Self-hosted game asset library. Go + SQLite + htmx, single static binary, single
Docker container, built to run on a Synology NAS.

Indexes a large library of downloaded game assets — 2D sprites, 3D models,
audio — makes them searchable by tag, tracks provenance and licensing, and serves
a Godot editor plugin.

The full specification is in [docs/spec.md](docs/spec.md). Work is organised by
the milestones in §14.

## Status

**M0, M1 and M2 complete.** What exists today:

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

Not yet: tags and the full query language (M3), ingest (M4), audio (M5), 3D and HDRI
(M6), spritesheet grid detection (M7), the JSON API (M8), the Godot plugin (M9).

### Known gaps in M2

- **`.tga` and `.xcf` have no decoder.** Both are recorded as
  `derive_state=unsupported` with a reason, visible in the UI, rather than failing
  silently. See `docs/decisions.md`.
- **The `.aseprite` decoder has never seen a file Aseprite actually wrote.** Its tests
  build files to the documented format, which verifies the implementation against the
  spec but not against reality. Dropping one real `.aseprite` into
  `testdata/fixtures/` is the most valuable contribution available.
- **Blend modes other than Normal are approximated** as Normal when compositing
  Aseprite layers, and say so in the job log.

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

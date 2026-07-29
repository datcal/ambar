# Decisions

Resolutions for the open questions in `spec.md` §15, plus deviations from the
spec that were taken deliberately. The spec is the decision record for *what* is
being built; this is the record for *how*, where the spec left the choice open.

Each entry says what was decided, why, and what would make it worth revisiting.

---

## §15.1 — FTS5 in `modernc.org/sqlite`

**Resolved in M0. FTS5 works; no CGO needed.**

`modernc.org/sqlite` v1.55.0 bundles SQLite 3.53.3 with `ENABLE_FTS5` present in
`PRAGMA compile_options`. Verified with `CGO_ENABLED=0`, and the resulting binary
is statically linked (`file` reports "statically linked"; `ldd` reports "not a
dynamic executable").

Everything §4 and §7 depend on was exercised, not just table creation:

- The **external-content** form (`content='assets', content_rowid='id'`) with
  trigger-maintained sync across insert, update and delete
- `'rebuild'`, `'integrity-check'` and `'optimize'` — `rebuild` is what
  `ambar rebuild-index` (§12) will lean on
- Prefix (`kenn*`), phrase, implicit AND, `OR`, `NOT`, column filters,
  column-set filters, `NEAR`
- `bm25()`, `ORDER BY rank`, `snippet()`, `highlight()`, and column weighting via
  `INSERT INTO t(t, rank) VALUES('rank', 'bm25(10.0, 1.0)')`
- Tokenizers: `unicode61` with `remove_diacritics`/`tokenchars`/`separators`,
  `porter`, `trigram`, `ascii`
- Malformed queries return errors rather than panicking, which matters because
  the §7 parser will hand user input to FTS5

The fallback (`mattn/go-sqlite3` with the `sqlite_fts5` build tag, costing CGO and
the static binary) is **not needed** and CLAUDE.md invariant 6 stands.

`internal/db/fts5_test.go` keeps these assertions permanently. A dependency bump
that dropped FTS5 would otherwise break search with no other signal.

**Two findings for M1:**

- **`bm25` scores compress towards a floor when a term appears in most rows.**
  SQLite clamps the IDF term at `1e-6`, so a term present in 2 of 4 documents
  scores `-1.43e-06` vs `-1.16e-06` — correctly *ordered*, but tiny; a term in 1
  of 4 scores a normal `-1.21`. Standard FTS5 behaviour, not a modernc quirk.
  **Consequence: sort by `rank`, never threshold on an absolute `bm25` value.**
- **`trigram` is available**, which is a cheaper route to §7's fuzzy filename
  matching (`swrd` → `wooden_sword_01.glb`) than a hand-rolled matcher. Worth
  measuring against a plain prefix index before committing.

---

## §15.2 — `templ` versus `html/template`

**Decided: `html/template`.** Confirmed with the operator before M0 was written.

- No codegen step, so no generated `.go` files in the repo and no extra tool in
  the Docker build or CI
- Contextual auto-escaping from the stdlib, which is the property that actually
  matters for a page rendering user-supplied filenames and tags
- Matches CLAUDE.md's "prefer boring, readable Go"

The real cost is that a template error surfaces at render time rather than
compile time. Mitigated by parsing every template once at startup
(`internal/server.parseTemplates`) so a broken template fails the process, and by
rendering into a buffer before writing, so a mid-render error cannot produce a
half-written body behind an already-sent 200.

**Revisit if** the template set grows past roughly a dozen pages and refactoring
across them starts causing render-time breakage that startup parsing does not
catch — i.e. wrong *data*, not wrong syntax.

---

## §15.3 — Where the JS islands come from

**Decided: vendored ES modules, no bundler.** The ergonomics do hold, with one
wrinkle worth knowing before M6.

`three@0.185.1` packaging, as it actually ships:

| File | Size | Imports |
| --- | --- | --- |
| `build/three.module.min.js` | 357 KB | `from "./three.core.min.js"` — **relative** |
| `build/three.core.min.js` | 376 KB | — |
| `examples/jsm/controls/OrbitControls.js` | 40 KB | `from 'three'` — **bare** |
| `examples/jsm/loaders/GLTFLoader.js` | 112 KB | `from 'three'` — **bare** |
| `examples/jsm/loaders/RGBELoader.js` | 0.3 KB | `from 'three'` — **bare** |
| `examples/jsm/utils/BufferGeometryUtils.js` | 37 KB | `from 'three'` — **bare** |

Roughly 925 KB total for the §8 viewer, ~200 KB over the wire with gzip. Served
from the NAS over a tailnet that is fine, and a CDN is not an option anyway: §2's
access topology assumes no general internet egress, and a CDN would break the
`default-src 'self'` CSP.

The core pair needs nothing special — that import is already relative, so the two
files work side by side in `internal/web/static/`. The **addons are the wrinkle**:
they import the bare specifier `'three'`, which a browser cannot resolve without
an import map.

Import maps are inline-only — the `src` attribute on `<script type="importmap">`
was specified but never shipped in any browser. An inline import map is subject to
`script-src`, so it would force either `'unsafe-inline'` (unacceptable — it
reopens exactly the XSS hole §11's `nosniff` and CSP rules are closing) or a
per-response nonce, which means the CSP header stops being a static string.

**So: rewrite the bare specifier at vendor time instead.** One `sed` in a
documented `make vendor-three` target:

```
s|from 'three'|from '/static/vendor/three/three.module.min.js'|
```

This keeps a strict, static `default-src 'self'` CSP with no inline script
anywhere, no nonce plumbing, and no bundler. The cost is one scripted line per
addon, re-run on upgrade, with the three.js version recorded in a constant beside
the files the way `web.HTMXVersion` already is.

**Revisit if** a future island needs a package whose internal import graph is deep
enough that patching specifiers stops being a one-liner. At that point a build
step earns its place — but it should be forced by evidence, not adopted
pre-emptively.

---

## §15.4 — Is `phash` cheap enough for M2, or does it belong in M11?

**Decided: compute and store in M2. Surface near-duplicates in M13, not M11.**

The question's framing has a false premise — cost is not the deciding factor,
because neither half is expensive:

- **Computing** a 64-bit dHash or pHash on an already-decoded image is
  microseconds. It is noise next to the decode and WebP encode that M2's
  `asset.derive` is already doing.
- **Searching** is also cheap at this library's size. All-pairs over 20,000
  images is ~200 million 64-bit XOR-and-popcount operations, well under a second
  single-threaded in Go. No BK-tree or LSH index is needed; if the library ever
  reaches a scale where it is, that is a contained change to one query.

What actually decides it is **when the pixels are in memory**. M2 decodes every
image once to build thumbnails. Computing `phash` in that same pass is free.
Deferring it means re-decoding twenty thousand images later, for no benefit — and
because derivatives are keyed on `sha256` + `derive_version` (§4), adding `phash`
in a later milestone forces a full re-derive anyway. That is the whole argument.

The *finder* belongs in **M13**, not M11, because that is where §9.1 lives, and
§9.1 is emphatic that near-duplicates are the category most likely to be
intentional (`@2x` variants, different tile sizes). They must be reported as
"review these", never as "delete these". Shipping a phash-driven near-duplicate
view before the §9.1 safety framing and its tests exist would invite exactly the
mistake that section is written to prevent.

**Concretely:** M2 stores `phash`; nothing reads it until M13. That is a column
populated ahead of its consumer, which is the opposite of the M0 migration policy
— justified here only because the alternative is a full re-decode.

---

## §15.5 — Spritesheet grid detection: the scoring heuristic

**Proposed for M7.** §6 requires "never silently guess wrong", and `frame_source`
must distinguish detected values from confirmed ones. The proposal below is a
plan, not code.

### Stage A — never guess when the answer is written down

Sidecar metadata always wins, and `frame_source='sidecar'`:

- TexturePacker JSON, Godot `.tres` `AtlasTexture`, Aseprite JSON export,
  Kenney XML (§6)
- The `.aseprite` file itself, which carries frame durations *and* frame tags —
  so for the `ASEPRITE/` folders the target library already contains, there is
  nothing to detect at all

### Stage B — alpha projection, which handles margins and padding

Divisor enumeration alone cannot express a sheet with an outer margin or
inter-frame spacing, and real packs have both.

1. Build a column profile `C[x]` = count of non-transparent pixels in column `x`,
   and a row profile `R[y]` likewise. One pass over the image.
2. Where runs of `C[x] == 0` exist, the geometry is **read directly rather than
   guessed**: the content runs give `frame_w`, the gaps give `spacing`, and the
   leading/trailing zero runs give `margin`.
3. Where frames touch and there are no zero gutters, autocorrelate `C[x]` and
   take the strongest peak at lag ≥ 4 as the candidate period.

### Stage C — divisor candidates, as the fallback for opaque sheets

For fully opaque sheets (tilesets, photographic atlases) neither gutters nor a
clean autocorrelation peak exists. Enumerate integer `cols`/`rows` from 1 to 64
and keep candidates whose remainder is exactly zero.

### Scoring

Every candidate is `(frame_w, frame_h, cols, rows, margin, spacing)`, scored by:

| Signal | Weight | Why |
| --- | --- | --- |
| Interior boundaries landing on fully-transparent columns/rows | highest | The strongest evidence a grid is real |
| No content crossing a cell boundary (per-cell bounding boxes) | high | The clearest evidence a grid is **wrong** |
| Cell occupancy — fraction of cells with any visible pixel | medium | A wrong grid produces many empty cells; reward near 1.0, penalise below 0.5 |
| Exact remainder: `W − 2·margin − cols·frame_w − (cols−1)·spacing == 0` | hard filter | Not a score; a non-zero remainder is disqualifying |
| Frame aspect ratio within [0.5, 2.0], square slightly preferred | low | A weak prior, easily wrong for wide UI strips |
| Familiar cell sizes: 8, 16, 24, 32, 48, 64, 96, 128 | lowest | A prior, never a rule — vendors ship 40×56 and it must still win on the strong signals |
| Degenerate shapes: `cols == W`, `rows == 1` with `cols == 1`, frame count > ~2048 | penalty | Rejects the trivially-fitting answers |

### How the user corrects a wrong guess

This is the part that makes the feature trustworthy, so it gets equal weight to
the detection.

- **Top three candidates as one-click chips** ("32×32 (8×4)"), best first. The
  common case is one click, which is what §6 asks for.
- **Grid overlay on the image**, redrawn live.
- **Editable numbers:** `cols`/`rows` and `frame_w`/`frame_h` are linked — editing
  either recomputes the other — plus `margin` and `spacing`. Arrow keys nudge.
- **An animation preview that actually plays** at the stored fps, beside the
  static overlay. An off-by-one that a grid overlay hides is obvious the moment
  the animation jitters. This is the check that catches what scoring cannot.
- **Nothing is applied until confirmed.** `frame_source` stays `detected` until
  the user accepts, at which point it becomes `manual`. A detected value is usable
  for a preview but never presented as fact.
- The loose PNG-sequence case from §5.1 (`PNG_Animations/Explosions/`) reuses this
  same confirmation UI, since the user decision is identical: "is this an
  animation, and at what rate".

---

## Deviations from the spec

Deliberate departures. Each is also commented at the site in the code, so it
cannot be discovered later as a surprise.

### Session cookie `Secure` is conditional (§11)

§11 mandates `Secure` on the session cookie. Taken literally that makes login
**impossible** over the plain-HTTP LAN access §8 documents as a real path, because
a `Secure` cookie is never sent over plain HTTP.

Resolved by deriving it from `AMBAR_BASE_URL`'s scheme, overridable with
`AMBAR_COOKIE_SECURE=auto|true|false`, with a startup warning when it resolves to
off. Confirmed with the operator. Tailscale and Cloudflare both give HTTPS, so the
hardened setting is the normal one; `AMBAR_COOKIE_SECURE=true` should be set once
access is HTTPS-only.

### `make test-race` overrides `CGO_ENABLED=0` (invariant 6)

Go's race detector requires cgo. Invariant 6 is about the binary that ships:
`make build` and the Dockerfile both stay at `CGO_ENABLED=0`, and the Docker build
**fails outright** if the result is dynamically linked. The exception applies to
one test target only, and is skipped where no C compiler exists.

### Pack detection needs a bucket concept §5.1 does not spell out (M1)

§5.1 defines a pack as "the shallowest directory that either contains a
`.ambar.json`, or contains asset files directly, or whose children are recognisable
format/variant folders". Implemented literally, that misattributes a very common
shape:

```
3d/kenney-sci-fi/Models/turret.glb     -> "Models" becomes the pack
3d/kenney-sci-fi/Sprites/ui_atlas.png  -> "Sprites" becomes another
```

`kenney-sci-fi` satisfies none of the three tests, so detection falls through to its
subfolders. That is §3's own example pack shape, and §3 gets away with it only
because its example carries a sidecar.

Pure structure cannot resolve it: `bucket/{packA,packB}` and `pack/{Models,Sprites}`
are the same tree. So a **fourth rule** was added — a directory at *pack level*
(directly under the library root, or directly under a bucket) with any asset beneath
it is a pack — plus a configurable list of bucket names defaulting to §5.1's own
`2d, 3d, mix, raw` and §17's `audio`.

Kept honest three ways:

- **`AMBAR_LIBRARY_BUCKETS` is configuration, not policy**, so §17's "must not
  depend on this layout" holds. Rename the buckets and change the line; abandon them
  and packs are found at the top level.
- **A `.ambar.json` sidecar overrides everything.** §5.1 lists the marker first, and
  a look-ahead pass finds one at any depth — which matters because §3 promises that
  "a copied folder carries its own metadata with it". Once M4 writes sidecars, the
  heuristic stops mattering.
- **The scan report names the buckets it recursed into**, so a wrong guess is
  visible on the first run rather than silently reshaping the grid.

### Scan trusts (size, mtime); `mtime` added to the schema (M1)

§4's assets sketch has no `mtime`. Without it, every scan must re-hash the whole
library — tens of GB on a NAS — to notice a change. §12 already separates the two
jobs: `scan` finds what changed, and `ambar verify` (M11) "re-hash[es] all or a
sample" to catch bit rot. So `mtime` is stored, and `sha256` is recomputed only for
files that are new or whose `(size, mtime)` moved.

Measured at 20,000 assets: first scan 1.7s, rescan 154ms with zero files re-hashed.

### assets_fts is a regular FTS5 table, not external-content (M1)

§4 specifies "FTS5 external-content over: filename, pack_name, tag_text, notes". That
column set spans a join — `pack_name` lives in `packs`, `tag_text` is derived from
`asset_tags` (M3) — and triggers cannot maintain it: renaming a pack would have to
rewrite every member row.

So the indexer owns those rows explicitly, and `ambar rebuild-index` (M11)
reconstructs them from `assets` + `packs` + tags, which is the same "SQLite is a
rebuildable index" philosophy the schema already rests on. Cost is a few MB of
duplicated short strings at 20k assets. The external-content form remains proven in
`internal/db/fts5_test.go` if this is ever revisited.

**Tokenizer note:** plain `unicode61`, deliberately *without* `tokenchars '_'`.
Splitting on `_` and `.` turns `wooden_sword_01.glb` into `wooden`/`sword`/`01`/`glb`,
so searching `sword` finds it. Keeping the underscore inside the token would match
only `wooden*`.

### `ambar scan` is CLI-only in M1

§12 wants scan runnable from the UI and on a schedule. Invariant 8 forbids
long-running HTTP handlers, and the job queue is M2. A goroutine with ad-hoc status
would be a worse job queue built twice, so the UI trigger waits for M2.

### A download route ships in M1, ahead of §10's M8 API

§14 gives M1 no way to see a file: thumbnails are M2 and `/api/v1/assets/{id}/file`
is M8. `GET /assets/{id}/download` closes that gap, with the §11 protections that
make serving library bytes safe — resolved through `safepath`,
`Content-Disposition: attachment`, `nosniff`, ETag and `Range`. Confirmed with the
operator.

### M0 created only the tables M0 uses

`schema_migrations`, `users`, `sessions`, `audit_log` — not the rest of §4. §4
calls itself "sketch, not gospel", and a schema is better shaped by the code that
reads it. `api_tokens` waits for M8.

The one accepted exception to this rule is `phash` in M2 (see §15.4 above), where
deferring the column would force re-decoding the whole library.

M1 followed the same policy: `packs` and `assets` carry only the columns M1
populates. §14 lists "pack model" under M4, but §5.1's detection rules belong to the
milestone whose grid depends on them — so M1 detects packs with identity columns
only, and M4 adds the provenance fields, the capture form and sidecars.

---

## Still open

- **Licence (§17).** MIT for maximum reuse, or AGPL-3.0 if hosted commercial forks
  are unwelcome. No `LICENSE` file exists yet. Worth settling before the
  repository is made public, since it is awkward to change once others have
  cloned it.

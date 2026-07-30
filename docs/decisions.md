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

### Pixel-art detection uses two of §6's three signals (M2)

§6 says to detect pixel art by "low dimensions (either axis under 256), low
unique-colour count, and hard edges", and is emphatic about the stakes:
"Bilinear-downscaling pixel art into mush is the single most annoying failure of every
existing tool; get this right."

**Dimensions are deliberately not used.** As a requirement the rule misclassifies the
image it most needs to catch — a 2048×2048 pixel-art tileset atlas is common in this
library, fails "either axis under 256", and is exactly what must not be
bilinear-downscaled. As a tie-breaker it only re-admits antialiased vector art, which
genuinely *should* be smoothly downscaled.

The other two signals do the work, with one refinement that turned out to matter:
**edge hardness is measured against transitions, not against all pixels.** Both pixel
art and a flat vector icon are mostly uniform interior, so soft-pixels-over-all-pixels
is near zero for both and separates nothing. Asking instead "of the places where
neighbouring pixels differ, how many differ only slightly" separates them cleanly.
Measured:

| Fixture | Colours | Soft-transition ratio | Verdict |
| --- | --- | --- | --- |
| flat-palette sprite, 128px | 5 | 0.000 | pixel art |
| the same sprite at 1024px | 5 | 0.000 | pixel art |
| antialiased vector shape | 120 | 0.542 | not pixel art |
| photographic gradient | 4096 (capped) | 1.000 | not pixel art |

The threshold sits at 0.40, in the middle of a wide gap. A test records these numbers
and fails if the margin narrows, so the threshold stays evidence-backed rather than
tuned until the tests went green.

### WebP is encoded losslessly by a native Go encoder (M2)

§3 and §6 want `thumb.webp`, but `golang.org/x/image/webp` is decode-only. Of the
cgo-free encoders, `github.com/HugoSmits86/nativewebp` was chosen: genuinely native Go,
MIT, one dependency (`x/image`, already present), and **lossless VP8L only**.

Lossless is the right trade rather than a limitation. Lossy compression destroys exactly
the hard edges §6 cares about, and at thumbnail sizes the file is small either way. The
alternative, `gen2brain/webp`, supports lossy and animation but embeds a
WASM-transpiled libwebp — multi-MB binary growth for a capability this application does
not want.

Animated previews are therefore GIF via the standard library, which §6 explicitly
permits ("Keep animation in GIF/WebP thumbnails").

### The 2D viewer loads a generated preview, never the original (M2)

§11 forbids serving library content inline — "an uploaded `.html` or `.svg` served
inline from the app origin is stored XSS" — but §8's viewer needs an inline image.

Resolved by generating `preview.webp` alongside the thumbnails and serving that. The
bytes came from our own encoder, so there is no XSS surface, and it makes the viewer
work identically for PSD, SVG and Aseprite sources. Originals remain download-only with
`Content-Disposition: attachment`.

### `.aseprite` is parsed from scratch; its fixtures are generated (M2)

§6 calls Aseprite support "first-class, not a nice-to-have". No library reads the binary
format: `github.com/solarlune/goaseprite`, the obvious candidate, parses Aseprite's
*JSON export*, which requires the user to have exported a spritesheet first — defeating
§6's stated goal that "an animated preview can be generated from the `.aseprite` itself
without needing the exported PNG sequence".

Implemented against `ase-file-specs`: colour depths 32/16/8, cel types raw/linked/zlib,
layer visibility and opacity, frame durations, and frame tags mapped onto
`animation_names`. **Blend modes other than Normal are treated as Normal**, and tilemap
cels are skipped — both reported through `Notes` rather than silently mishandled, since
a wrong composite is only dangerous when it is quiet.

**The fixtures are constructed to the spec, not authored by Aseprite.** That tests the
decoder against the documentation rather than against real output. The parser therefore
degrades to an error rather than guessing, and §6's `AMBAR_ASEPRITE_BIN` remains the
escape hatch. **Dropping one real `.aseprite` from the library into
`testdata/fixtures/` is the highest-value follow-up available.**

The fuzz sweep in those tests earned its place immediately: it found that
`make([]cel, 0, chunkCount)` trusted a 32-bit count read from the file, so a crafted
value asked for a 240 GB allocation. Every count is clamped now.

### New `AMBAR_MAX_IMAGE_PIXELS`, default 50 megapixels (M2)

Decoding allocates roughly `width × height × 4` bytes, so a 30000×30000 PNG is an
out-of-memory on a NAS — the image equivalent of §5's zip-bomb caps, which §13's
variable list predates. `image.DecodeConfig` reads only the header, so the guard costs
nothing. Over the cap is `derive_state=unsupported`, which the UI shows with its reason.

### `.tga` ships unsupported despite §6 listing it (M2)

`x/image` has no TGA decoder and both pure-Go options were last touched in 2015. It
degrades visibly — `derive_state=unsupported` with a reason, shown in the UI — rather
than silently. A minimal decoder (uncompressed plus RLE, roughly 120 lines) is a
contained follow-up. `.xcf` is unsupported too, which §6 already asks for, and
`.hdr`/`.exr` wait for M6 where §6 groups them with the 3D work.

### One derive job updates every asset sharing its content hash (M2)

§6 wants derivatives "idempotent, keyed on `sha256` + `derive_version`". Taken literally
for the *job* as well as the files, two identical files share one job — and if that job
only updated the asset it was dispatched for, the other copy sat at
`derive_state=pending` forever with no thumbnail, even though the thumbnail was on disk.

So the outcome is written to every non-missing asset with that hash. Identical bytes have
identical analysis, so this is exact rather than an approximation.

### `ambar derive` stops at "nothing runnable", not "queue empty" (M2)

A failed job is requeued behind an exponential backoff, which is right for a
long-running server and wrong for a one-shot command: the first version of `ambar derive`
sat through 30 seconds of backoff for a single undecodable file. It now stops when
nothing could start immediately, reports how many are waiting on a retry, and leaves them
for the next run.

### A completed job's bookkeeping survives shutdown (M2)

Found by the race detector, whose slowdown made the window wide enough to hit reliably:
if the context was cancelled between a handler succeeding and the completion being
written, the write failed and the row stayed `running` — so the next startup requeued it
and the job ran twice. Harmless for an idempotent derive, wrong in general. The
completion write now uses `context.WithoutCancel`.

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

M2 added `jobs`, `asset_groups`, and the derive and image-analysis columns — and took
the agreed exception for `phash`, which is written in M2 and read by nothing until M13.

M3 (0004_tags) added `tags`, `tag_closure`, `tag_aliases`, `asset_tags` and `pack_tags`.
`saved_searches` is §4/M3 too but waits for the M3 UI slice that writes it — same policy.

### A tag's `name` is the whole hierarchy path, not just the leaf (M3)

§4 sketches `tags(id, namespace, name, parent_id)` and §7 writes hierarchy as
`type:sfx:impact`. Two readings of `name` were possible: the leaf segment (`impact`)
with `parent_id` carrying the structure, or the full path within the namespace
(`sfx:impact`). We store the full path.

The leaf reading breaks the natural `UNIQUE(namespace, name)` identity — `type:sfx:impact`
and `type:ui:impact` would collide on `(type, impact)` — and would need
`UNIQUE(namespace, parent_id, name)`, which SQLite does not enforce across NULL parents
(two roots named `sfx` would both slip through). The full-path reading keeps a canonical
string a direct `(namespace, name)` lookup, and `parent_id` still records the single edge
up. `Tag.Leaf()` recovers the short form for display.

### Hierarchy is a closure table, not a recursive CTE (M3)

§7 offers either. The closure table (`tag_closure`, self-edge at depth 0) makes "searching
a parent returns children" one indexed `WHERE ancestor_id = ?` and the reverse (a tag's
ancestors, for building `tag_text`) one `WHERE descendant_id = ?`. It is derived data,
rebuilt from `parent_id` by `rebuild-index` (M11), so it is not a second source of truth.
The cost is a handful of rows per tag, which at this library's tag count is nothing.

### `assets_fts.tag_text` holds expanded canonical strings + aliases; structured filters do not use it (M3)

Tagging an asset rewrites its `tag_text` to the space-joined canonical strings of every
tag it carries (direct and inherited from its pack) expanded up to their ancestors, plus
those tags' aliases. The tokenizer splits `type:sfx:impact` into type/sfx/impact, so
free-text search finds an asset by any segment or alias. The `namespace:name` filters of
the M3 query language deliberately do **not** read this column — they query `asset_tags`
and `tag_closure` directly, where a filter is exact and hierarchy-aware. `tag_text` serves
only the free-text half of search. A pack tag change reindexes every member asset.

### Auto-path tags live in a `folder:` namespace (M3)

§7 shows path tags as bare words — `Models/Environment/Rocks/` → `models`, `environment`,
`rocks`. But §7 also makes the case that namespaces are what keep a 20k-asset library
navigable, and every other tag in the model is namespaced. Bare path tags would be a flat
soup mixed in with curated `type:`/`author:`/`style:` tags and impossible to facet
separately. So auto-path segments become `folder:<segment>` (`folder:rocks`), source
`auto_path`. They stay flat — one tag per segment, no hierarchy — matching the §7 example's
shape, since deep vendor paths would otherwise build noisy accidental hierarchies.

### Auto-tagging is a re-runnable `ambar retag`, not yet wired into scan/ingest (M3)

§7 frames auto-tagging as happening "on ingest", but ingest is M4 and the `style:pixel-art`
/ `has:alpha` signals depend on M2 derive results that land after a scan, not during it.
Rather than thread tagging through the scan and derive hot paths mid-milestone, M3 ships a
standalone `ambar retag` that recomputes auto tags for the whole index from current DB
state — additive, idempotent, manual-preserving. Wiring it to run automatically after
scan/derive, and into the M4 ingest flow, is a later, low-risk follow-up. Auto tags that no
longer apply (a reclassified file) are not pruned yet; that reconcile belongs with the same
follow-up.

### The query language is its own package; `index.FTSQuery` was removed (M3)

M1 shipped `index.FTSQuery`, a search-box-to-FTS5 quoter, with a note that the real §7
language would replace it in M3. It now has: `internal/search` parses a query into an AST
(OR of AND-groups, negation, quoted phrases, tag/kind/has/style/field terms) and compiles it
to SQL. `FTSQuery` and its test are gone rather than left beside the new parser — two FTS
quoting implementations would drift. `internal/search` has no database dependency; tag and
alias resolution reach it through a small `TagResolver` interface the index package
implements over the tag store.

### Each free-text and tag term compiles to its own asset-id subquery (M3)

Rather than translate the whole free-text part into one FTS5 MATCH expression (which cannot
mix with structured filters under OR/NOT), every term — a word, a phrase, a tag — becomes
`a.id IN (SELECT ... )`. These compose freely under AND, OR and NOT as ordinary SQL boolean
operators. The cost is several correlated subqueries per query, which at this library's size
is irrelevant, and the win is that `type:model OR "laser turret"` and `-has:alpha` just work.

### `type:` is the kind column, not a tag; `has:` / `style:pixel-art` are columns too (M3)

`type:model` and `kind:model` both compile to `a.kind = ?`, and `has:alpha` /
`has:animation` / `style:pixel-art` compile to the analysis columns — so these work off the
indexed truth regardless of whether `ambar retag` has run. `type:` with a non-kind value
(`type:sfx:impact`) is treated as a genuine namespaced tag. Arbitrary namespaces
(`theme:`, `author:`, `folder:`) go through the tag tables, hierarchy- and
inheritance-aware. A tag nobody holds matches nothing; a future/`color:` field is a no-op
with a surfaced warning.

### The tag UI is htmx fragments and native datalists, no inline script (M3)

Tag add/remove on the detail page swap a single `#tag-panel` fragment via htmx; the CSRF
token rides the global `hx-headers` already on `<body>`, so no per-form hidden field is
needed for the htmx posts. Autocomplete uses a native `<datalist>` filled by an htmx GET to
`/api/v1/tags/suggest` — no bundler, no inline `<script>`, nothing for the §11 CSP to
forbid. Non-htmx forms (save search, delete) carry the hidden `csrf_token` field as the M0
forms do.

### Facets count assets, direct + inherited, over the current filter (M3)

`index.Facets` runs List's exact asset-level filter (so a facet's count matches what
clicking it narrows to) and counts an asset once per tag it holds directly or inherits from
its pack. Counts are over assets, not groups: "17" next to a tag means seventeen things, not
seventeen files that might be the same artwork across format variants. Ancestor roll-up
(showing `type:sfx` next to an asset tagged only `type:sfx:impact`) is deliberately not done
yet — the facet lists tags actually present; drilling to a parent is a search, not a facet.

### Saved searches store the raw query; bulk tagging is POST-redirect-GET (M3)

A saved search keeps the raw §7 expression a person typed, re-parsed on use, unique by name
(re-saving a name updates it). Bulk tagging has two scopes — the checked tiles, or every
asset matching the current search via `index.MatchingAssetIDs` — and both redirect back to
the search with a one-shot `?msg=` flash rather than returning a fragment, so a browser
refresh cannot re-apply a tag to thousands of assets.

### `color:` / `palette-near:` are parsed but not matched in M3

§7 lists colour search under tagging/search, but the palette columns it needs
(`palette_json`, `palette_kind`) are §8's palette panel, scheduled in M11.5 — they do not
exist after M2. The M3 query parser will therefore accept `color:` / `palette-near:` tokens
without error but treat them as no-ops until M11.5 wires them to real palette data, rather
than pretending to filter on data that is not there.

### M4 provenance: licences are seeded reference data; every pack starts unverified

M4 (0006_provenance) adds the pack provenance columns and a `licenses` lookup, seeded in the
migration with a practical set (CC0, CC-BY family, OGA-BY, MIT, Apache, GPL, Unlicense,
Proprietary) and their commercial/attribution/share-alike flags. The lookup is application
reference data, not user metadata, so seeding it in the migration (and re-seeding on a
rebuilt database) is correct and does not violate "the filesystem is the source of truth" —
*which* licence a pack carries is the user metadata, and that lives on the pack and, from
4f, in its sidecar. Every pack — including the ones M1 already indexed — defaults to
`provenance_state=needs_provenance` per §9: fully usable, just flagged until the capture
form clears it.

### M4 archive extraction: pure-Go deps, and traversal aborts rather than neutralises

The two new dependencies §5 names — `github.com/nwaples/rardecode/v2` (RAR5) and
`github.com/bodgit/sevenzip` (7z) — were verified to build under `CGO_ENABLED=0` before being
adopted, so invariant 6 holds; the whole binary still links statically. zip stays on the
stdlib.

All three formats funnel through one `sink` that is the sole place bytes reach disk, so the
§5 safety rules live in exactly one spot and the (crafted-zip) tests exercise them for every
format. The deliberate choice: a traversal or absolute entry **aborts the whole extraction**
(`ErrUnsafeEntry`) rather than being silently rewritten to an in-root path. `cleanEntryName`
therefore uses `path.Clean` *without* anchoring at root, preserving `..` and leading `/` for
`safepath.Resolve` to reject — an archive that attempts an escape is hostile and belongs in
`_quarantine`, not quietly extracted. Symlinks and other non-regular entries are skipped and
reported. Uncompressed-size and entry-count caps (defaults 8 GiB / 200k, overridable)
defend against zip bombs, enforced both from the listing and again byte-by-byte during copy.

### M4 ingest extracts and records the pack, but the scanner indexes it

Ingest (internal/ingest) does the filesystem-shaping half of §5 — inspect, safe-extract,
retire the original to `_archives`, create the pack row with what provenance it knows — and
then enqueues a normal library scan to index the extracted files. It does **not** index
inline. Three reasons: the indexer already does classification, hashing, grouping and
derivative-enqueue correctly, so reusing it avoids a second implementation; two ingests
running inline scans would race the single-writer scan the indexer explicitly says is not
concurrency-safe, whereas the scan job is deduplicated; and provenance set at pack creation
survives, because the scanner's pack upsert (0002) touches only identity columns
(name/slug/kind/last_seen), never the provenance columns 0006 added.

A consequence: the scan reconciles the ingested directory as an ordinary `folder` pack, so
`kind` ends up `folder`, not `archive`. That is acceptable — `original_archive_name` being
non-empty is the durable "came from an archive" signal, and it is what §9's receipt trace
needs. Removing the consumed archive from `_inbox` (when `AMBAR_KEEP_ARCHIVES=false`) is
ingest handling its own transient input, not the application deleting indexed library
content, so it does not violate invariant 3.

Two follow-ups are deliberately left open: a **targeted per-pack index** (a full scan per
inbox drop is correct and idempotent but walks the whole tree), and **auto-tagging on the
serve ingest path** — the `ambar ingest` CLI runs `retag` itself, but the serve job only
enqueues a scan, so a pack ingested through the poller (M4-4e) is indexed and derived but
not auto-tagged until `ambar retag` runs. Both are the same class of "wire autotag into the
scan/ingest completion" work noted under M3.

### M4 archive flatten never strips "." or ".."

A single redundant top-level folder is flattened away (§5), but `flattenPrefix` refuses to
treat `.` or `..` as that folder. Without the guard, a hostile archive whose only entry is
`../evil` would have its `../` stripped as a "wrapper", neutralising the traversal into a
valid in-root write and robbing `safepath` of the chance to reject it. The guard keeps the
"traversal aborts extraction" contract intact for the lone-entry case; a regression test
covers it.

### M4 web upload: multipart CSRF rides the header, which M0 already planned for

The upload is multipart, and the M0 CSRF middleware deliberately refuses to parse a
multipart body to find the token — parsing it would buffer the whole upload before the
handler could impose a size cap. So the upload form posts through htmx, which carries the
CSRF token in the header from the global `hx-headers`, and the handler's
`http.MaxBytesReader` is therefore the first thing to touch the body and the cap actually
bites. A too-large upload is rejected with the §5-mandated message pointing at `_inbox`. The
handler answers htmx with an `HX-Redirect` header (a 303 body would be swapped into the DOM
instead of navigating); `redirectWithMessage` now emits that for htmx callers and a plain
303 otherwise, so the M3 bulk/saved-search forms are unaffected. Uploads are extension-gated
to zip/rar/7z, written into `_inbox` through `safepath`, then enqueued — the same job the
poller would raise, deduplicated on the path.

### M4 inbox poller: stability over two polls, dedup by path, sidecar URL sniff

`_inbox` is polled, not watched (inotify does not cross SMB or bind mounts), and a file is
ingested only once its size and mtime are unchanged across two consecutive polls, so a
half-copied bundle is never extracted. Enqueue is deduplicated on the archive path, so a
file still sitting in `_inbox` on later polls does not pile up jobs. The poller acts only on
zip/rar/7z by extension and reads a source URL from a `<archive>.url`/`.txt` sidecar
(tolerating the Windows `URL=` INI form), so provenance can be captured in the browser at
download time. It runs as a goroutine under `serve`, skipped entirely when the library is
read-only. The `.url` sidecar is left in `_inbox` after ingest — harmless, since the poller
ignores non-archives, though a later sweep could tidy them.

### M4 sidecars carry human metadata only; auto tags are never written

`.ambar.json` (internal/sidecar) holds a pack's provenance, licence (as an SPDX string),
notes and **manual** tags — pack-level and per-asset. Auto tags (`folder:`, `type:`, …) are
deliberately excluded: they are derived and regenerate from `ambar retag`, so writing them
would bloat the sidecar and create spurious diffs. This makes the sidecar exactly the set of
facts a human authored and the index cannot reconstruct on its own.

Import is gated by §3's rule "if a sidecar exists and the DB row does not, import ... newest
updated_at wins": a pack with no human metadata in the index imports unconditionally; a pack
that already has some keeps it unless the sidecar is strictly newer, and the divergence is
logged either way. Auto-import runs at the end of every scan (CLI and the serve job), which
is what recovers a rebuilt or freshly-copied library; `ambar sidecar sync` writes them in
bulk for a library indexed before sidecars existed, and `ambar sidecar import` runs the
import on demand. Verified end-to-end: sync, delete the whole database, rescan — provenance
and manual tags come back from the files.

On a read-only library the sidecar is written to `$AMBAR_DATA_ROOT/sidecars/<pack>/…`
instead of beside the originals (§3, invariant 1). Writes are atomic (temp + rename) so a
reader never sees a partial file.

Deferred: writing the sidecar on *every* metadata change (debounced, per §3). Today it is
written by `sidecar sync` and — from 4g — after a provenance edit; wiring a write into every
individual tag mutation is the same "write-through on change" follow-up noted under M3.

### M4 provenance UI: three views, sniff-to-prefill, and a sidecar write on save

The capture UI (§9) has three views — the needs-provenance backlog, the licence-risk view
(no licence, or `commercial_ok=0`, or attribution required with no text), and all packs.
Nothing is gated: every pack stays fully usable, the views are just the backlog to work
through. A per-pack form edits every field; the licence is a dropdown keyed on SPDX id. The
source URL is sniffed to pre-fill site and author (itch subdomain → author) as
non-destructive placeholder suggestions, so an edit never overwrites what the user typed.
Saving persists to the database and then writes the sidecar (§3) — this is the concrete
"write on metadata change" for the provenance path. A licence can also be set across a
multi-selection from the list. Verified end-to-end against the running server: risk view
lists an unlicensed pack, saving a licence marks it complete and rewrites its `.ambar.json`.

That closes M4: provenance model and licences (4b), safe archive extraction (4c), the ingest
pipeline and CLI (4d), the `_inbox` poller and web upload (4e), sidecars (4f), and this
capture UI (4g).

### M5 audio: four pure-Go decoders, mono analysis, a separate derive path

M5 (0007_audio) adds the audio columns and `internal/audio`, which decodes wav/mp3/ogg/flac
with the four §6 pure-Go libraries (`go-audio/wav`, `hajimehoshi/go-mp3`,
`jfreymuth/oggvorbis`, `mewkiz/flac`) — all verified to build under `CGO_ENABLED=0` before
adoption, so the binary stays statically linkable (invariant 6). Everything is mixed to mono
for analysis: a waveform and a peak level need no per-channel detail, and the source channel
count is still reported. Peaks are exactly ~2000 evenly-spaced min/max buckets in [-1,1] —
stored, not rendered, so the client draws the waveform on a canvas (§6, §8). `is_loopable` is
an advisory guess: a sound loops cleanly when it is still sounding at both ends (so a decayed
one-shot is excluded) and the wrap-around seam is small enough not to click.

Audio takes a wholly separate branch in `derive.Generate` — no image is decoded — that writes
`peaks.json` and fills the audio columns. Because audio files were recorded `unsupported`
before M5, the derive `Version` was bumped 1→2 so every asset is reconsidered once and the
audio ones now derive. A *malformed* audio file is a failure (retried), not unsupported;
only a format with no decoder at all stays unsupported.

### M5 audio serving: peaks as a derivative, the original streamed inline

The §8 audio viewer draws a canvas waveform from `peaks.json` (served like any other
derivative) and plays the original through an `<audio>` element. Playing the original means
serving a library file inline — the one exception to the "never serve originals inline" rule
(M1) — which is safe here because the response carries an explicit `audio/*` Content-Type and
the global `nosniff` header (§11) stops the browser re-interpreting the bytes as HTML; audio
is not an executable type, and an `<audio>` element cannot play an `attachment`. `ServeContent`
supplies Range, so seeking a long track does not re-download it. The viewer is a plain-JS
island keyed on `#audio-viewer`, a no-op elsewhere, config passed in `data-` attributes so
the CSP needs no inline script.

### M5 keyboard audition is a dormant grid island with a preset quick-tag

The §8 audition mode (`audition.js`) is the feature that "makes the difference between using
the library and avoiding it": with it on, the arrow keys step through only the audio tiles in
the current result set, each playing immediately through one shared `Audio` element, and a
single `t` tags the current sound with whatever is in the quick-tag box — POSTed to the M3
tag endpoint with the CSRF token from a `<meta>`, since this fires outside htmx. The island
reveals its toolbar only when the grid actually contains audio tiles (marked with
`data-audio`), so it is invisible on an all-image library. `a` toggles the mode, space
plays/pauses, escape exits. This closes M5.

### M6 3D: glTF/GLB derive in pure Go; FBX/.blend wait for Blender

M6 (0008_model) adds the model columns and `internal/model`, which reads glTF/GLB with the
pure-Go `qmuntal/gltf` (CGO-free, invariant 6), extracts triangle/vertex counts, the bounding
box (metres, for §8's scale reference), material count and animation names, and normalises
the input to a single self-contained `preview.glb` via `SaveBinary` — the §6 "normalise
everything to preview.glb" for the formats readable without Blender. `derive.Generate` gains
a model branch alongside the audio one; FBX and `.blend` return the new
`derive_state=needs_blender` (not a failure, not retried — the UI can offer to fetch Blender),
and the other model formats (obj/dae/stl/…) report unsupported with a reason until a converter
is built. Derive `Version` bumped 2→3 so previously-unsupported models re-derive.

### M6 3D viewer: three.js vendored r128, loaded only on 3D detail pages

The §8 3D viewer (`model-viewer.js`) is a plain-JS island over the global `THREE` from a
**vendored** three.js r128 (`static/vendor/three/`: the UMD `three.min.js` plus the non-module
`GLTFLoader`/`OrbitControls`, which attach to `THREE`). Vendored rather than from a CDN
because §2/§11 run a `default-src 'self'` CSP and the NAS often has no internet egress — the
same reasoning that vendors htmx. r128's single-file UMD builds are chosen over a modern
module tree specifically so three scripts suffice. The ~600 KB library is loaded only inside
the `{{if .IsModel}}` block on a model's detail page, so an all-2D library never ships it. The
viewer gives OrbitControls, grid and axis helpers sized to the model, a wireframe toggle and
the §8 1.8 m human-height scale reference; the triangle/vertex/material counts and bounding
box are rendered server-side from the columns. `preview.glb` is served inline like any
generated derivative (`model/gltf-binary`).

### M6 OBJ is converted to glTF in pure Go

`.obj` is parsed in `internal/model/obj.go` (§6: "obj converts in pure Go") and built into a
glTF document with `qmuntal/gltf`'s modeler, then normalised to `preview.glb` and analysed by
the same path as native glTF. OBJ stores position/normal/texcoord indices independently, so
the parser de-indexes each unique `v/vt/vn` combination into the single shared index buffer
glTF requires, triangulating polygons by a fan and resolving OBJ's 1-based and negative
(relative) indices. Materials/.mtl are ignored — the viewer needs geometry, and the counts
and bounding box come from the positions.

### M6 FBX/.blend derive through an optional external Blender, never a bundled one

`internal/blender` drives a Blender CLI as a subprocess to convert FBX/.blend to `preview.glb`
(a `--background --factory-startup --python` invocation that imports the source into an empty
scene and exports one GLB). Blender is never in the image (§6: the 250 MB vs 2 GB difference);
`Locate` finds it from `AMBAR_BLENDER_BIN` or `$DATA_ROOT/tools/blender/`, and when neither is
present those formats stay `needs_blender` — usable, just unpreviewed. A Blender *failure* is
a real failure (retried); producing no output is a failure too. The wiring is verified with a
stub Blender that emits a known GLB, so the subprocess plumbing and derive routing are tested
without a 300 MB binary.

That gives M6 full format coverage: glTF/GLB/OBJ in pure Go, FBX/.blend through a configured
Blender. Still open as optional extras (§6): the runtime *download* of Blender with checksum
verification (configuring `AMBAR_BLENDER_BIN` is the manual equivalent today), and Workbench
turntable thumbnails. Neither blocks previewing or browsing a 3D asset.

### M7 spritesheet grid detection: divisor candidates scored by seam transparency

§15.5 asked for the scoring heuristic to be proposed before implementing; this is it (in
`internal/spritesheet`). Candidate column/row counts are the exact divisors of each dimension
that give a cell ≥ 8 px and ≤ 64 frames per axis. Each candidate grid scores as
`0.6·seamTransparency + 0.25·squareness + 0.15·cellContent`, where seamTransparency is the
fraction of interior seam-line pixels that are fully transparent (a real grid has transparent
gutters), squareness rewards square cells, and cellContent is the fraction of cells holding
some opaque pixel. A grid with < 50 % non-empty cells is rejected outright. On a score tie the
*finer* grid wins — a coarser grid only ties when its seams fall on the finer grid's gutters,
so the finer one is the true frame size.

A detection is only `Confident` when the seams are ≥ 90 % transparent and ≥ 75 % of cells have
content. A tight, gutterless sheet scores low on purpose: §6 insists the tool must "never
silently guess wrong", so a low-confidence guess is shown with a grid overlay for the user to
confirm or correct (`frame_source` = detected → manual), rather than trusted. Sidecar formats
(TexturePacker/Godot/Aseprite/Kenney) override detection when present.

Migration 0009 adds `frame_w/h/cols/rows` and `frame_source`; `frame_count`/`fps` already
exist from 0003.

Detection runs in the image derive path for still images whose name or dimensions make them
candidates, storing the grid as `frame_source=detected`; a *confident* guess also gets a
`sheet.gif` built from its cells at 12 fps (§6: "so the grid view shows animation instead of
forty tiny squares"). The confirm/correct UI draws the grid as a pure-CSS overlay of repeating
gradients over the thumbnail, and a one-click confirm or a corrected columns/rows count
promotes the grid to `frame_source=manual`. A manual (or sidecar) grid is preserved across a
re-detection — `confirmedFrames` reads it back so a derive `Version` bump does not revert a
human's correction. `Version` bumped 3→4 so existing images are checked for a grid once.

Still open in M7: parsing the sidecar atlas formats (TexturePacker JSON, Godot `.tres`,
Aseprite JSON, Kenney XML) to override detection with exact geometry when a sibling file is
present. Detection + confirmation covers the gutter-grid case that dominates the target
library today.

### M8 API tokens reuse the session token discipline; the JSON API is bearer-only

M8 (0010_api_tokens) adds `api_tokens` and `internal/auth` token support. A token is 32 bytes
of CSPRNG output shown once as `ambar_<base64url>` and stored as an unsalted SHA-256 — the
same reasoning as session tokens (§11): the input is already high-entropy, so a slow KDF buys
nothing. Scopes are `read < write < admin` with a hierarchy (`write` implies `read`); an empty
set floors at `read`. Tokens carry expiry, revocation (set, not deleted, for the audit trail)
and a best-effort `last_used_at` so a stale one is visible. Creation and revocation are
audit-logged.

The JSON API (§10) lives under `/api/v1`, authenticated by `Authorization: Bearer` via
`TokenStore.RequireToken(scope, …)`, which injects the resolved user into the same context key
the session path uses — so the serving handlers (thumb, file, preview.glb, peaks) are reused
unchanged under token auth. Reads require the `read` scope; the search/asset/pack/tags
endpoints return JSON DTOs that omit kind-irrelevant fields and carry `links` to the binary
endpoints. Token management is a session-authed settings page that shows the plaintext exactly
once and never redirects it (a token in a URL would land in logs).

Deferred to later milestones: the write endpoints `/projects/{project}/uses` and
`/credits.md` (they need the `projects` table, M9) and `/packs/{id}/download` as a zip.

### M9 write API: projects keyed on UUID, uses deduplicated and soft-removed

M9 (0011_projects) adds `projects` and `project_uses` and `internal/projects`. A project is
identified by its UUID (invariant 10), created on first use, so the same Godot checkout at
different paths on two machines is one project. `RecordUse` is idempotent on
`(project, asset, res_path)` — a repeat re-activates a soft-removed row rather than
duplicating it, which is how two people importing the same asset produce one row. Removal sets
`removed_at` rather than deleting, so credits and history survive, and it is what M13 will read
to keep an in-use asset off every removal list (invariant 5). `credits.md` is rendered from
the used assets' provenance, deduplicated by pack and grouped by licence then author (§9).

The write endpoints (`POST`/`DELETE /api/v1/projects/{project}/uses`) require the `write`
scope; `credits.md` needs only `read`. Bearer-token requests are exempted from CSRF in the
middleware — CSRF exploits ambient cookies a browser attaches automatically, which a token
request has none of, and the token is itself the proof of intent — so the Godot plugin can
POST without a CSRF cookie it has no way to obtain.

### M9 Godot plugin is GDScript, unverified in this environment

The editor plugin (`addons/ambar/`, §10) is GDScript and cannot be exercised from the Go test
suite or without a Godot editor — it is the one component with no automated verification here.
It is written to the §10 contract (UUID identity, additive manifest, import presets, offline
queue) and must be smoke-tested in Godot before it is trusted.

### M11 ops orchestrate existing pieces behind a testable API

`internal/ops` holds rebuild-index, verify and backup (§12) behind a package API rather than
only in the CLI, because rebuild-index fidelity is a §16 quality-bar test and invariant 2 is
only real if it passes. `RebuildIndex` removes the database file (and its -wal/-shm), opens a
fresh one, migrates, scans for identity, imports sidecars for provenance and manual tags, then
re-auto-tags — reconstructing everything from the filesystem without touching the library;
derivatives keyed by content hash survive and are left for `ambar derive`. The fidelity test
proves a manual tag and provenance survive a full drop-and-rebuild via the sidecar, and that
auto tags regenerate. `Verify` re-hashes every present file (even when size and mtime are
unchanged, unlike scan) and flags a mismatch with `content_changed_at`; it exits non-zero so a
scheduled run alerts, and never deletes or rewrites the stored hash. `Backup` is `VACUUM INTO`
a timestamped copy (§12: SQLite-level, since a filesystem snapshot of a live WAL database can
be inconsistent), refusing to overwrite. Three thin CLI commands wrap them; all verified
end-to-end.

M10 (licensing) is already covered without a dedicated milestone: the licence-risk view is the
`/provenance?view=risk` page (M4-4g) and CREDITS.md generation is the M9 write API.

### M11.5 palette: exact-or-quantized by colour count, extraction folded into derive

0012_palette adds `palette_json` and `palette_kind` to `assets`; `internal/palette` does the
extraction and every export format. The one decision that matters is §8's "exact, not
approximate": an image with ≤256 distinct **visible** colours is enumerated exactly
(`palette_kind=exact`), and only an image over that threshold is reduced by median-cut
(`quantized`, N=16), which the UI labels as approximate. 256 is the indexed-PNG maximum, so
every indexed image lands on the exact path without a separate `PLTE`-chunk reader — counting
the pixels gives the same colours plus their real counts, and surfaces only colours actually
used. Fully transparent pixels are excluded (§8: or the palette is "dominated by transparent
black"); semi-transparent pixels still count and set `has_semitransparent`. Median-cut, not
k-means, because it is deterministic — identical bytes must yield byte-identical
`palette_json`, or every rescan would look like a change.

Extraction runs inside `derive.Generate`'s image path (Version bumped 4→5, so existing
libraries re-derive once) rather than as a separate job: the image is already decoded there,
and a second pass over 20k files to add a palette would be exactly the waste `derive_version`
exists to avoid. Palette is derived analysis, like `phash`/`color_count` — it is **not**
written to sidecars and is reconstructed by re-running derive after `rebuild-index`, not from
the filesystem, consistent with the other derived columns.

Copy and export live server-side (`/assets/{id}/palette/{format}`, one route, format as a path
segment because Go 1.22 wildcards must be whole segments — a `palette.{format}` segment is
invalid). The panel's click-to-copy is a JS island: swatch chip colours are set through the
CSSOM, never an inline `style` attribute, so the CSP stays `default-src 'self'` with no
`unsafe-inline`; copy prefers `navigator.clipboard` and falls back to a hidden-textarea
`execCommand('copy')` so it does not silently fail on plain-HTTP LAN (§8's explicit gotcha).
The GDScript `Color(…)` rendering is shared between the copy island and the `.gd`/JS paths at
three decimals, so a copied value is byte-identical to an exported one.

**Deviation surfaced:** the earlier note "`color:` / `palette-near:` … no-ops until M11.5 wires
them" is only half-honoured. M11.5 delivers the columns those tokens need, but wiring the
**search matching** (a colour box query with tolerance, palette-distance ranking) needs an
indexed per-swatch table and query-compiler work that is §7 search, not §8 panel — so the
tokens remain accepted no-ops for now, and the matching is deferred to its own slice rather
than bolted on here. Flagged rather than done silently.

### M12 junk view: reporting-only, and the sweep runs on the job queue

`internal/junk` detects the §9.1 clutter — `__MACOSX` shadow trees, OS metadata files
(`.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*`), zero-byte files, empty directories, and
orphaned derivative directories — and returns a report sorted by reclaimable bytes, largest
win first. It is a **detector and reporter only** (invariant 3): the package contains no
delete, move, or trash code, and the web page offers no removal control. The human-selected
removal path, its trash staging, and the safety invariants all ship together in M13 — §9.1
calls removal "the highest-risk surface in the codebase", so building selection-for-deletion
UI before that safety net exists would be exactly the hazard it warns against. Surfaced here
rather than papered over: "manual selection" in the §14 M12 line is delivered as a read-only
report, not clickable checkboxes, deliberately.

Two design choices worth recording:

- **The sweep is a job, not a synchronous handler.** A junk sweep re-walks the whole library,
  and invariant 8 forbids a long-running HTTP handler — every other library walk (scan,
  ingest) already goes through the queue. So `/junk/scan` enqueues `maintenance.junk` (deduped,
  one pending at a time) and the worker caches the result to `$DATA_ROOT/junk-report.json`
  (atomic write, beside the derivatives — it is rebuildable generated data, not source of
  truth, so it does not belong in the database). `/junk` reads that cache and shows "last
  scanned at …"; `ambar junk` prints the same report synchronously for a one-shot CLI check.
- **`internal/junk` stays free of SQL,** like `internal/library`. Orphan detection needs the
  set of live content hashes, so the caller supplies it (`index.ContentHashes`, passed as a
  `HashProvider` callback) and the whole package is testable against a fixture tree with no
  database. A **nil** hash set means "not supplied, skip the orphan check" and is deliberately
  distinct from an **empty** set ("no assets exist, so every derivative is an orphan") — without
  that distinction a caller that forgot the hashes would report the entire derivative cache as
  removable. `.git` and the reserved `_trash`/`_inbox`/… directories are never reported; the
  trash especially must not be surfaced as clutter to clean up. A zero-byte `.ambar.json` is
  never proposed either — it is a pack's provenance anchor, not junk.

### M13 duplicates and removal: two packages, and the safety rules live in code

The milestone that finally touches user data destructively is split so that the
destructive half is small enough to read in one sitting:

- **`internal/dupes` detects and explains.** It contains no code that can move or
  delete a file — the same discipline `internal/junk` follows. It answers §9.1's four
  relationships separately (exact hash, moved file, near-duplicate, format variant),
  resolves packs before files, and annotates each copy with the keep-policy *facts*
  plus one labelled hint. Unlike `internal/junk` it is not SQL-free: duplicate
  detection is inherently a query over the whole index, and threading twenty thousand
  rows through a callback would have bought nothing.
- **`internal/removal` acts.** A `Planner` that only ever refuses or permits, and an
  `Executor` that performs a `Plan` and owns the trash. The Executor has no policy of
  its own: if the Planner did not produce an `Op`, the Executor never sees it.

Decisions worth recording:

- **The last-copy rule is arithmetic over the whole selection, not per row.** Selecting
  every copy of a hash removes all but one; the refused copy is the first in sorted
  order, which is arbitrary but deterministic and named in the preview. Refusing *all*
  of them would have been the other defensible answer, but it makes "select all in this
  finding" useless on a finding where every copy was selected. Honoured literally, this
  also means one zero-byte file survives a junk sweep that selects two hundred of them —
  they share the empty-string hash. That is invariant 4 as written, and carving an
  exception into it silently is exactly what the invariant exists to prevent.
- **Exact-hash findings are not suppressed within an asset group.** Invariant 7 is about
  *format variants*, and a PNG and its PSD cannot share a sha256. Two byte-identical
  files are the same file stored twice whatever folders they sit in. The group model is
  consumed where it actually matters: the near-duplicate pass never pairs two variants of
  one group (they have the same perceptual hash by construction), and "N source variants
  depend on this copy" is one of the keep annotations.
- **Linking is allowed for a project-used asset; removal is not.** §9.1's hard block is
  on *removal*. A reflink or hardlink keeps the path working and the bytes identical, so
  an imported asset is not endangered by one. Surfaced here because it is the one place
  the invariant-5 block is deliberately narrower than "never touch this file".
- **The bytes are re-hashed immediately before a link replaces a file.** The Planner
  already checked that the index says the two are identical, but the index can be stale,
  and this is the only operation in Ambar that writes over an existing library path. If
  the bytes diverged since the last scan, the entry fails rather than replacing one
  file's content with another's.
- **The plan is re-derived at every step, three times.** `/removals/plan` builds it,
  `/removals/apply` builds it again from the same submitted paths, and the worker builds
  it a third time from the job payload. Nothing is carried in a token or a session, so a
  stale page, an edited payload, or a library that changed in between can only ever
  *narrow* what happens. The alternative — trusting the payload — would have made the
  job queue a way to bypass every safety rule in the package.
- **`safepath.LstatUnder` was added rather than joining paths locally.** `Resolve`
  follows symlinks, which is what makes it safe, and therefore cannot tell a link from
  its target — but §9.1's removal must refuse links. Putting the join inside `safepath`
  keeps the rule that no other package joins a root with untrusted input.
- **Trash layout: `<trash>/<batch-id>/<root>/<original relative path>`,** with an
  `ambar-trash.json` manifest per batch. The batch directory is what makes retention
  purging a directory-level decision and stops two removals of the same path from
  colliding. The manifest is written **before** the first file moves, so an interrupted
  batch leaves a readable record of intent; restoring treats it as untrusted input, since
  it is a file a person can edit.
- **`AMBAR_TRASH_DIR` is now validated.** The scanner only skips underscore-prefixed
  directories at the *library root*, so a trash directory nested deeper (or named
  `trash`) would be walked and every removed file re-indexed as a new asset — the
  duplicate would come straight back. Config refuses that at startup instead.
- **Curation transfer is injected, not imported.** `internal/removal` knows nothing about
  tags or provenance; the job runner takes a `TransferFunc`, and `cmd/ambar/serve.go`
  passes `dupes.TransferCuration`. It runs **before** any file moves and aborts the whole
  batch on failure, which is the only ordering that satisfies "transfer its tags and
  provenance onto the superset first". A plan that asks for a transfer with no function
  wired is refused rather than quietly losing the curation. The transfer fills gaps only,
  matches asset tags by content hash rather than path, and reports manually tagged files
  that have no counterpart in the superset instead of dropping them silently.
- **The shell-script export is a first-class path, not a fallback** (§9.1 says to expect
  it to be the primary one). It mirrors the in-app behaviour exactly — `mv` into a trash
  batch, never `rm` — quotes every path with POSIX single-quote escaping (the library
  really does contain `PNG_Parts&Spriter_Animation`), guards each link with `cmp -s`, and
  includes the refusals as comments so the script is a record of the decision rather than
  only of its outcome. It says plainly that it cannot transfer curation, because that
  lives in the database.
- **Purging is the only irreversible operation, and it is the only one that asks twice.**
  Never scheduled, never triggered by disk pressure, refused outright when no retention
  window is configured, and a dry run by default on the CLI (`--yes` to mean it). `Purge`
  with a zero cutoff is an error, because "no retention configured" must never be able to
  read as "delete everything".
- **M12's deferred selection landed here, as promised.** The junk view now has checkboxes
  and a "select all in this finding" control — §9.1 explicitly permits the latter — and
  both feed the same preview-then-confirm flow as the duplicate view. `data-select-all` is
  a small external JS island so the CSP stays `default-src 'self'`; without JavaScript the
  header checkbox is inert and the rows still work.
- **Reflink uses the `FICLONE` ioctl through `golang.org/x/sys/unix`,** which is pure Go,
  so invariant 6 holds. It is probed at startup and reported in the health endpoint as
  `dedupe_link_mode`; a library that cannot reflink is a fact about the volume, not an
  unhealthy service, so it never turns the endpoint red. `reflink_other.go` keeps the
  package building on non-Linux developer machines, where hardlink and trash still work.

### The `.aseprite` decoder, measured against real files — and what that found

The M2 write-up recorded that the decoder "has never seen a file Aseprite actually
wrote", and that dropping one real file into the tests was the most valuable
contribution available. There are 72 in the target library, so that gap is now closed —
but not by vendoring one: the packs are CraftPix free-licence artwork, which does not
permit redistribution. `internal/aseprite/corpus_test.go` instead walks a directory
named by `AMBAR_ASEPRITE_CORPUS` and skips when it is unset, so the check is repeatable
locally and the repository stays clean.

It found a real bug within minutes, which is the entire argument for the exercise:

- **The compositor produced straight (non-premultiplied) alpha and returned it in an
  `*image.RGBA`,** whose documented contract is premultiplied. Every fully opaque pixel
  was correct, so all 30-odd builder-fixture tests passed for two milestones, while a
  shadow layer at 35% opacity came out nearly three times too bright in every thumbnail,
  animated preview and extracted palette derived from an `.aseprite`. The fix is
  `*image.NRGBA` throughout — straight alpha is exactly what that type holds, so the
  existing blend arithmetic became correct by contract rather than by accident.

Because the packs ship the *same artwork twice* — `.aseprite` sources and a PNG
spritesheet Aseprite itself exported — the test does not stop at "it decodes". It finds
the sheet row matching each file and compares pixel by pixel. That is a far sharper
instrument than a fixture, and it prompted a second change:

- **`blendNormal` now reproduces Aseprite's own integer arithmetic** (`rgba_blender_normal`
  from `doc/blend_funcs.cpp`, including `MUL_UN8` and C truncating division) instead of
  the equivalent float formula. The two differ by one unit per channel on some pixels,
  and matching the original makes 70 of the 72 files byte-identical to the vendor's
  export.

Two deliberate tolerances, both narrow and both explained in the test:

- **±1 per channel with identical alpha is accepted**, because this corpus proves
  Aseprite's arithmetic changed: within one pack, Plant1's export matches the current
  integer formula exactly while Plant2's and Plant3's match the older rounded one. No
  implementation can match both, so the code follows the current source.
- **Up to 0.1% of pixels may differ by more**, because a vendor sheet is not always a
  clean export — two files here have a stray pixel or a shadow blended twice in the sheet
  that is not in the source. Anything beyond that fails, and the counts are always
  logged. For scale, the premultiplied-alpha bug showed up as ~3% of pixels off by up to
  94, in every affected file.

A builder-fixture regression test covers the same ground without a corpus
(`TestSemiTransparentCompositeIsStraightAlpha`): the earlier fixtures all used opaque
colours, which is precisely why they never caught this.

### Licence: MIT (§17)

Settled: MIT, for maximum reuse. The alternative considered was AGPL-3.0, which would
have made a hosted commercial fork share its changes — but this is a self-hosted tool for
one person's asset library, the value is in the operator running it rather than in
controlling redistribution, and MIT is the lower-friction choice for anyone who wants to
lift a piece of it (the `.aseprite` decoder and the archive extractor are both useful on
their own). `LICENSE` carries the standard text.

### Colour search, the pack palette view, and auto-tag reconciliation

The three deferrals §7 was still carrying, delivered together because they share the
data they read.

**`color:` and `palette-near:` (the slice M11.5 deferred).** M11.5 stored the palette as
JSON on the asset, which is right for the panel and wrong for a query: matching "assets
containing this colour within a tolerance" against a JSON blob means parsing every row.
So 0013 adds `asset_swatches` (one row per swatch, channels as separate indexed
columns), written by derive alongside `palette_json` and backfilled in the migration
from what M11.5 already stored — via `json_each`, so colour search works on an existing
library immediately rather than only after a full re-derive. `TestJSON1IsAvailable`
asserts the JSON1 extension is compiled into `modernc.org/sqlite`, because a migration is
the wrong place to find out otherwise.

Decisions inside that:

- **`color:` is a box in RGB, not a perceptual distance.** The query is "this hex, give or
  take", which is what someone pasting a colour out of the palette panel means, and a box
  is three indexed range scans. The default tolerance is a tight ±12 per channel: pixel
  artists reuse exact palette entries, so a wide default would return every brownish
  asset in the library. `color:#8b3a3a~40` widens it, `~0` demands an exact match.
- **A swatch below 0.5% of an asset's visible pixels does not count as "containing" a
  colour.** Three stray anti-aliased pixels are not an art-direction match, and the tail
  of a palette is exactly where they live.
- **`palette-near:` is nearest-neighbour over swatches, not earth-mover.** §7 allows
  either; EMD needs an optimisation pass per candidate, which is not something to run
  inside a SQL filter over 20k assets. The question asked instead — of the reference's
  dominant colours, does the candidate have a majority of them? — has the same practical
  answer and compiles to one grouped subquery with a `count(DISTINCT CASE …)` over the
  reference boxes. Counting distinct *reference* colours matters: a candidate with twelve
  shades of one reference colour has still matched one colour.
- The compiler learned a second resolver (`SwatchResolver`) rather than a database
  handle, keeping `internal/search` free of SQL exactly as `TagResolver` does.

**The pack palette consistency view (§7's last unbuilt sentence).** `/palettes` aggregates
each pack's swatches into a coarse colour grid weighted by how much of the pack each
colour covers, and compares two packs by how much of each one's weight has a near-enough
colour in the other. Two numbers rather than one score, because they answer different
questions: a small character set can sit entirely inside a large tileset's palette while
the tileset keeps colours the characters never use. The verdict is a sentence, not a
number — this is a hint for a human deciding whether two packs will look right together.
Measured against the real library it agrees with the eye: the bright saturated spaceship
pack scores 0% against everything, while the undead tileset and the crystals pack score
83%/90% and do belong in the same scene.

**Auto-tagging is now wired in, and reconciles.** M3 recorded that `ambar retag` was
manual and that stale auto tags were never pruned. Both are fixed: `tags.PruneAutoTags`
removes `auto_path`/`auto_type` rows the current pass did not produce (manual and
inherited rows are never touched, so a person's `type:image` survives a pass that would
not apply it), and the tagger gained `RetagAssets`/`RetagContent` so a scan retags the
index for path tags while derive retags just-analysed content for `style:pixel-art` and
`has:alpha`. Derive learned an `AfterAnalyse` callback rather than importing the tagger —
the same shape that keeps `internal/index` unaware of `internal/derive`. A failing hook is
logged and swallowed: a missing tag is recomputable, a lost thumbnail is not worth it.

### Pack detection collapsed the whole library when a loose file sorted first

Found by pointing the scanner at the real library rather than at `testdata`: it reported
**one** pack holding 1,562 assets instead of nine.

`filepath.WalkDir` visits entries in lexical order. A loose asset file at the library root
registered the root in `packOf` — the map meaning "this directory is inside a real pack" —
and every pack directory visited *after* it was then treated as part of that synthetic
standalone pack. So whether the library split into packs depended on whether the loose
file's name sorted before the pack directories: `TileSet_V2.png` (uppercase, sorts first)
broke it; the same file named `zz.png` did not. The target library has exactly such a file
at its root.

The fix is one map: standalone ownership lives in `standaloneOf`, consulted only when
resolving a file's owner, so the synthetic pack owns the loose files and nothing else.
Both spellings are now covered by a table test, and the real library reports nine packs
with the two loose root files in their own standalone pack.

Worth recording as a lesson rather than only a fix: every pack-level feature — provenance,
inherited tags, pack similarity (§9.1), the palette view above — reads pack membership,
and all of them had been quietly degraded to "one pack" on the only library that matters.
A fixture tree with tidy names never showed it.

### M14: the interface, rebuilt around the library (and four bugs it uncovered)

Not a spec milestone. It came from using the thing: the grid was four tiles wide in a
60rem column, there was no folder navigation anywhere, `.gdignore` and `.gitkeep` sat
in the grid as assets, 20×21 tiles were specks, dragging an image picked it up instead
of panning, every `.obj` and `.fbx` said "this needs Blender", and the spritesheet
panel never said what detection was for. Modelibr and Eagle were read for shape;
what follows is what was taken and what was deliberately not.

**The library is the application, so it gets the window.** `/` is the grid now, and the
old dashboard moved to `/status`. Library pages render a three-pane shell — navigation
left, content centre, inspector right, three independent scroll areas under one
toolbar — while forms and reports stay centred documents, because a 60rem measure is a
feature there rather than a cage. One flag on the page data picks the layout; there is
no second template tree.

Taken from Eagle: search in the toolbar rather than inside a page, a **thumbnail size
slider** (with ⌘/Ctrl +/− because muscle memory deserves respect), folders and tags
permanently visible on the left, and the inspector on the right. Taken from Modelibr:
its `cardWidthStore` habit of remembering the tile size per view, and — the important
one — how it answers "open in an external app" (below). Not taken: tabbed dual panels
and URL-encoded workspace layouts. They are the right answer for an app you live in
all day; here they would be a second navigation model competing with the browser's.

**The folder tree is derived, never stored.** `index.Tree` groups live assets by
directory in SQL, builds the tree in Go, rolls counts up, and `Flatten` emits the top
level plus the open branch. A `dir=` filter on the grid compares against the assembled
library path with a prefix test rather than LIKE, so `%` and `_` in real filenames need
no escaping. Depth is capped at four (bucket/pack/format-folder/subject, the shape §5.1
describes); deeper directories stay reachable through search. Indentation comes from a
`data-depth` attribute, so the CSP stays free of `unsafe-inline`.

**Small images were unreadable for a one-line reason.** `.thumb img` used
`max-width: 100%`, which never scales *up*, so a 20×21 tile from a tileset rendered at
20×21 in an 11rem tile. `object-fit: contain` with `image-rendering: pixelated` for
pixel art fixes it without re-deriving anything, and the slider does the rest.

**Panning fought the browser.** The 2D viewer's `pointerdown` did not `preventDefault`,
so the browser started its own native image drag as soon as the pointer moved over the
`<img>` — which is why panning only worked from the empty space around the image. The
fix is `preventDefault` plus `draggable="false"`, a `dragstart` guard, and
`user-select`/`-webkit-user-drag` off. The viewer also grew to 72vh, since the centre
column is the point of the page.

**3D no longer waits for Blender, because it never had to.** §8's viewer loaded a
derived, normalised `preview.glb`, so a format derive could not normalise had no viewer
at all — three.js has read OBJ and FBX natively for years. The viewer now picks a
loader from the extension and loads the *original* file; `needs_blender` applies only to
`.blend`, which is Blender's own project format and genuinely nothing else can open it.
Even there the message is now accurate: what is missing is a grid *thumbnail*, not the
ability to look at the model.

That needed one new route. `GET /assets/{id}/file/{name...}` serves a file from the
model's own directory by name, because an `.obj` names its `.mtl`, an `.mtl` names its
textures, and a `.gltf` names its `.bin`. Keeping the model's basename in the URL means
relative resolution inside the loaders just works. It is deliberately narrow: an
allow-list of extensions a model may reference, `path.Clean` before any filesystem
call, `safepath` after it (invariant 9), and §11's `attachment` + `nosniff` on the way
out — which three.js does not mind, because its loaders fetch bytes rather than
navigate. The traversal tests assert on file *contents* rather than on words from the
path, because net/http's path-cleaning redirect echoes the URL and made the first
version of that test pass for the wrong reason.

**"Open in Aseprite" cannot be a button, so it is a path.** A browser cannot launch an
application on the machine you are sitting at, and this server runs on the NAS — a
server-side "open" would launch Aseprite *there*, headless. Modelibr solves this by
deriving the native SMB/UNC path and offering copy-to-clipboard and reveal-in-file-
manager, and that is what Ambar does now: `AMBAR_LOCAL_LIBRARY_PATH` says how the
library is mounted where the operator works, the rail shows the composed path with a
copy button and the applications worth suggesting for that extension, and it says
plainly why it cannot do more. The separator style comes from the template's own shape
(`\\nas\share` and `Z:\` get backslashes) rather than from sniffing the user agent,
which lies. Unset, the section explains how to enable itself instead of showing a path
that would not resolve. Godot keeps its real integration — the editor plugin pushes,
and the rail now reports which projects already use the asset, which is also why
removal refuses to touch it.

**Spritesheet detection says what it is for.** The panel now explains that confirming
the grid is what makes the animated preview correct, what `frames:` searches find, and
what the Godot import turns into an `AnimatedSprite2D` — and that nothing is written to
the file. The player became a player: pause, frame stepping, a frame counter, an fps
control and a grid overlay toggle. Stepping needs no new derivative — it moves a window
over the sheet using the detected columns and rows — while playing keeps using the
derived GIF, whose timing is baked in and which is therefore honest about ignoring the
fps control.

**Two more bugs the real library exposed, both in indexing rather than in the UI:**

- **Non-assets were indexed.** `IsAssetFile` existed from M1 but was only consulted when
  *confirming* a pack, never when indexing a file — so readmes, licences, coupons,
  `.url` shortcuts and dotfiles all became grid tiles. It now gates indexing, and it
  additionally excludes downloaded archives and model companion files (`.mtl`, `.bin`).
  One real 3D pack contributed 484 companion tiles before that rule existed. On the
  target library the scan now skips 540 files and says so in its summary, because "why
  is my file not in the grid" deserves an answer in the report rather than in the
  source. Nothing is deleted: rows for files that are no longer considered assets are
  marked missing by the next scan, which the grid already hides.
- **Pack detection collapsed the whole library when a loose file sorted first** — written
  up in its own entry above, found by pointing the scanner at the real library instead
  of at `testdata`.

### M15: the second pass over the interface

M14 rebuilt the shell; this is what using it for an evening turned up. Each item was a
specific complaint, so each one gets a specific answer.

**The sidebar leads with what the work is about.** Order is now: re-scan (an action that
was previously at the bottom of a page nobody scrolls), kinds, **colours**, tags,
filters, then folders — and folders are a `<details>` element that starts *closed*. The
operator's own words: "whole library should always be there, and I go into a folder when
I want to; I will work with kinds, tags and colours." A native `<details>` keeps that
state in the browser, so nothing is remembered server-side and nothing needs JavaScript.

**Colour search became clickable.** `index.LibraryColours` aggregates the whole library's
swatches into the same coarse buckets the pack view uses, and the sidebar renders them as
a row of chips that link to `color:<hex>~24`. On the asset page each swatch now carries
two explicit icons — ⌕ searches for that colour, ⧉ copies it — because the previous
arrangement (whole chip copies, a small "find" word underneath) hid the feature §7 calls
the real daily problem. Chips are painted from `data-hex` through the CSSOM, never an
inline style, so the CSP stays `default-src 'self'`.

**The centre column is only the work.** The keyboard-audition strip moved into the panel's
Tools section, and the bulk-tag box became one toolbar line. The spritesheet panel — added
in M14 with a player — was removed from the detail page entirely at the operator's request:
the animation already plays in the viewer above it, so what remains is one line of facts,
a link to what confirming the grid is *for*, and the control. The space it freed now holds
the palette and the tags, which are tools rather than reference.

**"Fit" scales up.** The 2D viewer's `fitScale` carried the comment "never scale up to
fit", which meant a 20×21 tile opened at 20×21 in a 46rem stage and you had to click 800%
to see it — the same mistake the grid's tiles made with `max-width`. Fit now upscales, and
for pixel art it snaps to a whole factor so edges stay hard.

**Three kinds that had no preview now have one.**

- **Audio**: a waveform tile drawn from the peaks the analysis already computes, so it
  costs no extra decode. A grid of two hundred `.wav` files was previously two hundred
  identical chips.
- **Fonts**: §4 has had a `font` kind since M1 and nothing ever rendered one. A specimen
  is drawn server-side with `sfnt` + `opentype` (pure Go, invariant 6) for the tile, and
  the detail page registers the real face through the FontFace API so you can **type your
  own text** — the only question a font list has to answer. `.woff/.woff2` are recorded as
  unsupported with a reason rather than guessed at; sfnt cannot read the compressed
  wrappers.
- **3D in the grid**: the server has no renderer and Blender is optional (§6) — but the
  browser has one and is already loading these models. So a model tile with no thumbnail
  carries `data-model`, and an island renders it once off-screen, snapshots the canvas and
  POSTs the PNG. The route decodes it, bounds-checks it, **re-encodes it as WebP with our
  own encoder** (never storing client bytes), refuses to replace an existing derivative,
  and records `derive_state=ok` for that content hash. Rendering is capped per page load
  and driven by an IntersectionObserver, so browsing 900 models does not turn the tab into
  a render farm. three.js is loaded on the grid *only* when the page actually holds such a
  tile.

**"Open in Aseprite" is a real button now.** The M14 version handed over a path to copy,
which is honest but is not what a button should do. The operator was right about the
mechanism: `tel:` works because a scheme is registered with the OS. Aseprite, Blender and
Godot ship no scheme of their own — so Ambar registers `ambar://` and generates the
one-time helper that answers it (a `.desktop` + `xdg-mime` on Linux, an app bundle's
`Info.plist` on macOS, `HKCU\Software\Classes` on Windows). The link carries the app key
and the local path, so the helper needs no credentials, no network and no knowledge of
Ambar. Two details worth recording: `html/template` rewrites an unknown scheme in an
`href` to `#ZgotmplZ`, so the URL is typed `template.URL` (safe — it is assembled from
escaped parts); and every helper refuses an `smb://` path with an explanation, because no
application can open a URL — the operator has to point `AMBAR_LOCAL_LIBRARY_PATH` at a
*mounted* location. The copyable path stays, because a feature that needs a one-time
install must degrade to something that works immediately.

**Adding a user is possible from the UI.** §11 forbids self-registration and gives the two
users equal rights, which had meant `ambar user add` on the server was the only way. Any
signed-in user can now create another from Settings, audited with who did it. Deleting a
user and changing someone else's password are deliberately absent: sessions, audit rows and
manual tags all reference a user, and "two equal users" gives nobody the authority to lock
the other out.

**More non-assets stopped being indexed**: `.import` (Godot writes one beside every
imported file) and `.meta` (Unity). With M14's rules that is 623 files skipped on the
target library — and the operator was still seeing `.import` tiles because rows from an
earlier scan persist until a rescan marks them missing.

**Renames**: `ingest` is `upload` in the navigation (the route stays, plus a `/upload`
alias), and `palettes` left the top bar for the sidebar's Tools section — it is a view you
visit occasionally, not a primary destination.

## Still open

Every milestone in §14 is delivered, and so is every deferral recorded above. What
remains is two documented non-goals:

- **`.tga` and `.xcf` have no pure-Go decoder** and are recorded as
  `derive_state=unsupported` with a reason. Deliberate: §6 keeps external converters
  optional rather than baking them into the image.
- **Blend modes other than Normal are approximated** as Normal when compositing Aseprite
  layers, and say so in the job log. No file in the 72-file corpus uses one, so there is
  no evidence to implement against yet.

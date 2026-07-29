-- M2: the job queue, asset groups, and the derivative columns.
--
-- Same conventions as earlier migrations: STRICT tables, INTEGER Unix seconds UTC,
-- and only the columns this milestone actually writes.

-- The job queue (§4).
--
-- Invariant 8: "No long-running HTTP handlers. All ingest, scan, and derivative work
-- goes through the job queue with pollable status." This table is what makes that
-- possible, and §12 additionally wants failures inspectable in the UI rather than
-- only in container logs — which is why last_error is kept rather than just logged.
CREATE TABLE jobs (
    id           INTEGER PRIMARY KEY,
    type         TEXT    NOT NULL,
    payload_json TEXT    NOT NULL DEFAULT '{}',
    state        TEXT    NOT NULL CHECK (state IN ('queued', 'running', 'done', 'failed')),

    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT    NOT NULL DEFAULT '',
    -- Higher runs first. Interactive work (a scan the user just asked for) outranks
    -- the thousands of derive jobs it will enqueue.
    priority     INTEGER NOT NULL DEFAULT 0,
    -- Not before this time. Carries the retry backoff.
    run_after    INTEGER NOT NULL,

    -- Collapses duplicate work. §6 requires derivatives be "idempotent, keyed on
    -- sha256 + derive_version, so rescans do no work" — with the partial unique index
    -- below, that becomes a property of the schema rather than a convention every
    -- caller has to remember. NULL means "no deduplication for this job".
    dedupe_key   TEXT,

    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    started_at   INTEGER,
    finished_at  INTEGER
) STRICT;

-- The claim query orders by (priority DESC, id) over queued rows whose run_after has
-- passed, so this is the index it rides on.
CREATE INDEX jobs_claim_idx ON jobs(state, run_after, priority DESC, id);

-- Two identical jobs cannot be pending at once. Partial, so a completed job does not
-- block re-enqueueing the same work later — which matters when derive_version is
-- bumped and everything legitimately needs redoing.
CREATE UNIQUE INDEX jobs_dedupe_idx ON jobs(dedupe_key)
    WHERE dedupe_key IS NOT NULL AND state IN ('queued', 'running');

-- For the /jobs view and the failed-job count in the health report.
CREATE INDEX jobs_state_created_idx ON jobs(state, created_at DESC);

-- Asset groups (§5.1).
--
-- "PNG/Plant1/idle.png, PSD/Plant1/idle.psd, and ASEPRITE/Plant1/idle.aseprite are
-- one logical asset in three formats — not three assets. Indexing them independently
-- means every sprite appears three or four times and the grid becomes noise."
--
-- This is also invariant 7: format variants are never surfaced as redundant copies.
-- M13's duplicate finder consumes this model rather than re-deriving it.
CREATE TABLE asset_groups (
    id      INTEGER NOT NULL PRIMARY KEY,
    pack_id INTEGER NOT NULL REFERENCES packs(id) ON DELETE CASCADE,

    -- The §5.1 key: the pack-relative path with format-folder segments removed and
    -- the extension dropped. See library.StripFormatFolders.
    group_key TEXT NOT NULL,

    -- The engine-ready variant, chosen by §5.1's precedence (png > webp > glb > ...).
    -- ON DELETE SET NULL rather than CASCADE: losing the primary must not delete the
    -- group and orphan its other variants.
    primary_asset_id INTEGER REFERENCES assets(id) ON DELETE SET NULL,

    -- Denormalised so the grid can show "3 variants" without a per-row subquery.
    variant_count INTEGER NOT NULL DEFAULT 0,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE UNIQUE INDEX asset_groups_key_idx ON asset_groups(pack_id, group_key);
CREATE INDEX asset_groups_primary_idx ON asset_groups(primary_asset_id);

-- --- new asset columns ------------------------------------------------------
--
-- SQLite only supports one ADD COLUMN per statement.

-- Group membership. NULL only transiently, between a scan inserting an asset and the
-- grouping phase running.
ALTER TABLE assets ADD COLUMN group_id INTEGER REFERENCES asset_groups(id) ON DELETE SET NULL;

-- Image analysis, all written by asset.derive.
ALTER TABLE assets ADD COLUMN has_alpha INTEGER;
-- §8: in pixel art semi-transparent pixels "are usually an authoring mistake and
-- worth surfacing", which is why this is separate from has_alpha.
ALTER TABLE assets ADD COLUMN has_semitransparent INTEGER;
-- Exact count up to a cap; NULL when the image was not analysed.
ALTER TABLE assets ADD COLUMN color_count INTEGER;
-- Decides nearest-neighbour resizing (§6) and image-rendering: pixelated (§8).
ALTER TABLE assets ADD COLUMN is_pixel_art INTEGER;

-- 64-bit perceptual hash, stored as 16 hex characters.
--
-- Written in M2 and read by nothing until M13's near-duplicate view. That inverts the
-- usual "no column without a writer" policy, deliberately: M2 already has every image
-- decoded, and adding it later would mean re-decoding the whole library. See
-- docs/decisions.md §15.4.
ALTER TABLE assets ADD COLUMN phash TEXT;

-- Animation, populated from the .aseprite decoder and from multi-frame GIFs.
-- The spritesheet geometry columns (frame_w, frame_h, frame_cols, frame_rows,
-- frame_source) belong to M7's grid detection and are not created here.
ALTER TABLE assets ADD COLUMN frame_count INTEGER;
ALTER TABLE assets ADD COLUMN fps REAL;
-- JSON array of names. From Aseprite these are its own frame tags, which §6 maps
-- directly onto animation_names.
ALTER TABLE assets ADD COLUMN animation_names TEXT;

-- Derivative state (§4, §6).
--
-- 'pending'        never derived, or derive_version is stale
-- 'ok'             derivatives on disk and current
-- 'failed'         tried and errored; retryable from the UI
-- 'unsupported'    no decoder for this format (.xcf, .tga, tilemap cels), or the
--                  image exceeds AMBAR_MAX_IMAGE_PIXELS. Not an error, and not
--                  worth retrying without a code change.
-- 'needs_blender'  M6: .fbx and .blend, until Blender is downloaded
ALTER TABLE assets ADD COLUMN derive_state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE assets ADD COLUMN derive_error TEXT NOT NULL DEFAULT '';
-- §4: "when the thumbnail algorithm improves, bump the version and only stale
-- derivatives regenerate. Without it, every improvement means manually re-triggering
-- twenty thousand files."
ALTER TABLE assets ADD COLUMN derive_version INTEGER NOT NULL DEFAULT 0;

-- Finding the work: what still needs deriving, and what failed.
CREATE INDEX assets_derive_state_idx ON assets(derive_state, derive_version);
CREATE INDEX assets_group_idx ON assets(group_id);
-- M13 near-duplicate scanning; partial because most rows are non-images.
CREATE INDEX assets_phash_idx ON assets(phash) WHERE phash IS NOT NULL;

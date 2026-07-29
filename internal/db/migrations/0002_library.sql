-- M1: the library index — packs, assets, and the FTS table over them.
--
-- Same conventions as 0001_init.sql: STRICT tables, INTEGER Unix seconds UTC.
--
-- Scope follows M0's policy of creating only what this milestone populates. The
-- rest of §4's assets sketch (phash, palette_json, frame_*, tri_count,
-- derive_*) arrives with the milestone that writes it. ALTER TABLE ADD COLUMN is
-- cheap in SQLite, and a column with no writer is a column with no meaning.

-- Packs (§5.1). Identity only in M1; §14 puts the provenance fields, the capture
-- form and sidecars in M4, which will add:
--   source_url, source_site, source_author, source_author_url, license_id,
--   license_note, attribution_*, acquired_at, price_paid_cents, currency,
--   order_ref, original_archive_*, provenance_state, notes
CREATE TABLE packs (
    id               INTEGER PRIMARY KEY,
    -- Display name, taken from the directory name as-is.
    name             TEXT    NOT NULL,
    -- URL-safe form. §10 uses it for res://assets/<kind>/<pack-slug>/.
    slug             TEXT    NOT NULL,
    -- §4: archive | folder | standalone. 'standalone' is for loose files that
    -- have no pack of their own (§5.1); 'archive' arrives with M4 ingest.
    kind             TEXT    NOT NULL CHECK (kind IN ('archive', 'folder', 'standalone')),
    -- Slash-separated, relative to AMBAR_LIBRARY_ROOT. The one place a pack's
    -- location is recorded, so renaming a pack directory is a single-row update
    -- rather than one per member asset.
    library_rel_path TEXT    NOT NULL UNIQUE,
    first_seen_at    INTEGER NOT NULL,
    -- Updated by every scan that still finds the pack. A pack whose directory is
    -- gone keeps its row: the assets under it are marked missing, never deleted.
    last_seen_at     INTEGER NOT NULL,
    created_at       INTEGER NOT NULL,
    updated_at       INTEGER NOT NULL
) STRICT;

CREATE INDEX packs_slug_idx ON packs(slug);

-- One row per file (§4).
CREATE TABLE assets (
    id       INTEGER PRIMARY KEY,
    pack_id  INTEGER NOT NULL REFERENCES packs(id) ON DELETE CASCADE,

    -- Slash-separated and relative to the PACK, not to the library root. The
    -- library-relative path is packs.library_rel_path || '/' || assets.rel_path.
    --
    -- Pack-relative because it makes a pack rename one UPDATE instead of N, and
    -- because §5.1's asset_group key ("relative path with the format-folder
    -- segment removed") is naturally pack-relative — M2 needs exactly this.
    rel_path TEXT    NOT NULL,
    -- Denormalised from rel_path so the grid can sort and search on it without
    -- string work in SQL, and so FTS has a column to mirror.
    filename TEXT    NOT NULL,
    -- Lowercased, without the leading dot. Empty string for no extension.
    ext      TEXT    NOT NULL,

    -- §4's kinds plus tilemap and rig from §5.1. In M1 this is decided by
    -- extension alone: spritesheet detection needs the §6 heuristics (M7) and
    -- texture needs filename-suffix heuristics, so both index as 'image' here.
    kind     TEXT    NOT NULL CHECK (kind IN (
                 'image', 'spritesheet', 'texture', 'model', 'audio', 'video',
                 'font', 'script', 'material', 'hdri', 'tilemap', 'rig', 'other')),

    size     INTEGER NOT NULL,
    -- Modification time, which §4's sketch omits. Load-bearing: together with
    -- size it is the cheap change signal that lets a rescan skip hashing an
    -- unchanged file. Without it every scan would re-read the whole library, and
    -- §12 already assigns re-hashing to `ambar verify`.
    mtime    INTEGER NOT NULL,
    -- Hex sha256. Computed on first index and whenever (size, mtime) changes.
    sha256   TEXT    NOT NULL,

    -- Cheap header-only metadata (§5 step 4). NULL for non-images and for images
    -- whose header could not be read.
    width    INTEGER,
    height   INTEGER,

    first_seen_at      INTEGER NOT NULL,
    -- When the content was last confirmed by hashing, as opposed to merely seen.
    last_verified_at   INTEGER NOT NULL,
    -- Set when a scan no longer finds the file; cleared if it reappears.
    --
    -- §12: missing files are NEVER hard-deleted, "because a NAS share can be
    -- temporarily unmounted and destroying the index over that would be
    -- catastrophic". There is no DELETE on this table anywhere in M1.
    missing_since      INTEGER,
    -- Set when the bytes at a stable path changed. §12 wants these "flagged for
    -- review"; M1 flags and reports, and the review UI comes later.
    content_changed_at INTEGER,

    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- Identity of a file within its pack.
CREATE UNIQUE INDEX assets_pack_rel_path_idx ON assets(pack_id, rel_path);

-- Move detection (§9.1 rule 2) and duplicate detection (M13) both look up by
-- content hash, so this one is load-bearing rather than speculative.
CREATE INDEX assets_sha256_idx ON assets(sha256);

-- Keyset pagination for the grid orders by (filename, id); §8 requires staying
-- responsive at 20k+ rows, and OFFSET degrades with depth.
CREATE INDEX assets_filename_idx ON assets(filename, id);

-- The same ordering, filtered by kind.
CREATE INDEX assets_kind_filename_idx ON assets(kind, filename, id);

-- Partial: only the rows a "what went missing" view cares about, which is
-- normally none of them.
CREATE INDEX assets_missing_idx ON assets(missing_since) WHERE missing_since IS NOT NULL;

-- Same, for the changed-content review list.
CREATE INDEX assets_content_changed_idx ON assets(content_changed_at) WHERE content_changed_at IS NOT NULL;

-- Full-text search (§4, §7).
--
-- A regular FTS5 table, deliberately NOT the external-content form §4 sketches.
-- The reason is that the column set spans a join: pack_name lives in packs and
-- tag_text is derived from asset_tags (M3). Triggers cannot maintain that — a
-- pack rename would have to rewrite every member row — so the indexer owns these
-- rows explicitly, and `ambar rebuild-index` (M11) reconstructs them from
-- assets + packs + tags. That is the same "SQLite is a rebuildable index"
-- philosophy the whole schema rests on.
--
-- rowid is always assets.id, so a match joins straight back.
--
-- Tokenizer: plain unicode61 with diacritic folding, deliberately WITHOUT
-- tokenchars '_'. Splitting on _ and . turns wooden_sword_01.glb into
-- wooden/sword/01/glb, so searching "sword" finds it. Keeping the underscore
-- inside the token would match only "wooden*", which is the opposite of what
-- filename search needs.
CREATE VIRTUAL TABLE assets_fts USING fts5(
    filename,
    pack_name,
    tag_text,       -- empty until M3
    notes,          -- empty until M4
    tokenize = 'unicode61 remove_diacritics 2'
);

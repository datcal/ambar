-- M3: tagging (§4, §7).
--
-- Same conventions as earlier migrations: STRICT tables, INTEGER Unix seconds
-- UTC, and only the tables this milestone's writers populate. saved_searches
-- (§7) arrives with the M3 UI slice that writes it; a table with no writer is a
-- table with no meaning.

-- A tag is `namespace:name`, where `name` is the full colon-separated hierarchy
-- path within the namespace (§7: `type:sfx:impact` implies `type:sfx`).
--
-- Storing the whole path in `name` — rather than only the leaf segment — keeps
-- (namespace, name) a genuine unique identity and makes the canonical string a
-- direct lookup. parent_id points at the tag one segment shorter (`type:sfx`
-- for `type:sfx:impact`), NULL for a top-level name. The transitive hierarchy
-- lives in tag_closure below; parent_id is the single edge, closure is its
-- reachability.
CREATE TABLE tags (
    id          INTEGER PRIMARY KEY,
    -- Lowercased at the boundary. `type`, `theme`, `license`, `author`, ...
    namespace   TEXT    NOT NULL,
    -- Full hierarchy path within the namespace, colon-separated: `sfx:impact`.
    -- For a flat tag this is a single segment: `cc0`.
    name        TEXT    NOT NULL,
    description TEXT    NOT NULL DEFAULT '',
    -- The parent tag (`type:sfx` for `type:sfx:impact`). ON DELETE is RESTRICT by
    -- omission: a tag with children cannot be removed out from under them; the
    -- caller deletes leaves first. Deletion is an M3-UI concern, not M1 scan.
    parent_id   INTEGER REFERENCES tags(id),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,

    UNIQUE (namespace, name)
) STRICT;

CREATE INDEX tags_parent_idx ON tags(parent_id);
-- Namespace facet listing and the faceted sidebar's per-namespace grouping.
CREATE INDEX tags_namespace_idx ON tags(namespace, name);

-- The transitive-closure table for §7 hierarchy: "searching a parent returns
-- children". Every tag has a self-row at depth 0; each ancestor gets a row at
-- its distance. Descendants of T (T included) are the rows with ancestor_id = T,
-- which is the set a tag filter expands to. Rebuilt from parent_id by
-- rebuild-index (M11), so it is derived data, never a second source of truth.
CREATE TABLE tag_closure (
    ancestor_id   INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    descendant_id INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    depth         INTEGER NOT NULL,
    PRIMARY KEY (ancestor_id, descendant_id)
) STRICT;

-- The hot direction: given a tag, all its descendants. Covered by the PK. The
-- reverse (a tag's ancestors, for building tag_text) rides this index.
CREATE INDEX tag_closure_descendant_idx ON tag_closure(descendant_id);

-- Aliases (§7): `sfx` -> `type:sfx`, `cc0` -> `license:cc0`. Typing the short
-- form resolves to the canonical tag. An alias is globally unique so resolution
-- is unambiguous.
CREATE TABLE tag_aliases (
    id         INTEGER PRIMARY KEY,
    tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    alias      TEXT    NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

CREATE INDEX tag_aliases_tag_idx ON tag_aliases(tag_id);

-- Direct tags on an asset (§4). `source` distinguishes a human's choice from the
-- machine's guess so §7's "auto-tag ... overridable by manual tags" holds:
--   manual     a person added it
--   auto_path  derived from folder segments (§7)
--   auto_type  derived from kind/analysis (`type:model`, `has:alpha`)
--   inherited  materialised from a pack tag (kept explicit so an asset can drop
--              one without losing the others; the pack tag itself is unchanged)
CREATE TABLE asset_tags (
    asset_id   INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    source     TEXT    NOT NULL CHECK (source IN ('manual', 'auto_path', 'auto_type', 'inherited')),
    -- The user who added a manual tag, for the audit trail (§4 audit_log). NULL
    -- for machine sources.
    created_by INTEGER REFERENCES users(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (asset_id, tag_id)
) STRICT;

CREATE INDEX asset_tags_tag_idx ON asset_tags(tag_id);

-- Pack-level tags (§4, §7 "inherited tags"). Applied to every member asset,
-- shown greyed in the UI and overridable per asset.
CREATE TABLE pack_tags (
    pack_id    INTEGER NOT NULL REFERENCES packs(id) ON DELETE CASCADE,
    tag_id     INTEGER NOT NULL REFERENCES tags(id) ON DELETE CASCADE,
    source     TEXT    NOT NULL CHECK (source IN ('manual', 'auto_path', 'auto_type')),
    created_by INTEGER REFERENCES users(id),
    created_at INTEGER NOT NULL,
    PRIMARY KEY (pack_id, tag_id)
) STRICT;

CREATE INDEX pack_tags_tag_idx ON pack_tags(tag_id);

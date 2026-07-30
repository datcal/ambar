-- M9: projects and their asset usage (§4, §10).
--
-- §10/invariant 10: a project's identity is a UUID, never a filesystem path —
-- the Godot project is checked out at different paths on different machines, and
-- keying on a path would split the credits list. The path is at most a display
-- hint.

CREATE TABLE projects (
    id         INTEGER PRIMARY KEY,
    uuid       TEXT    NOT NULL UNIQUE,
    name       TEXT    NOT NULL DEFAULT '',
    note       TEXT    NOT NULL DEFAULT '',
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

-- One row per asset placed into a project (§10). Deduplicated on
-- (project_id, asset_id, res_path) so two people importing the same asset
-- independently produce one row, not two. Soft-removed (removed_at) rather than
-- deleted, so credits and history survive an asset being taken back out.
CREATE TABLE project_uses (
    id           INTEGER PRIMARY KEY,
    project_id   INTEGER NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    asset_id     INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    -- Where it landed in the Godot project, e.g. res://assets/models/…/x.glb.
    res_path     TEXT    NOT NULL,
    -- The content hash at import time, so the "outdated" badge (§10) can compare
    -- against the library's current sha256.
    asset_sha256 TEXT    NOT NULL DEFAULT '',
    added_at     INTEGER NOT NULL,
    removed_at   INTEGER,
    UNIQUE (project_id, asset_id, res_path)
) STRICT;

-- Invariant 5 ("anything referenced by project_uses is never a removal
-- candidate", M13) and the outdated-badge lookup both scan by asset, so this is
-- load-bearing rather than speculative.
CREATE INDEX project_uses_asset_idx ON project_uses(asset_id) WHERE removed_at IS NULL;

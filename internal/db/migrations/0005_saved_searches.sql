-- M3 (UI slice): saved searches (§7 "Saved searches as first-class pinnable
-- objects").
--
-- Deferred out of 0004 until this slice, per the same "a table with no writer is
-- a table with no meaning" policy the earlier migrations follow.
--
-- The query is stored as the raw §7 expression, not a compiled form: it is the
-- source of truth a person typed, re-parsed on use, so an improvement to the
-- parser applies to every saved search for free — the same rebuildable-index
-- philosophy as the rest of the schema.
CREATE TABLE saved_searches (
    id         INTEGER PRIMARY KEY,
    -- The human label, unique so "my sci-fi props" names exactly one search.
    name       TEXT    NOT NULL UNIQUE,
    -- The raw query expression, e.g. `type:model theme:sci-fi -style:realistic`.
    query      TEXT    NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) STRICT;

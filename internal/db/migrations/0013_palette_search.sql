-- §7 colour search: `color:#8b3a3a` and `palette-near:<asset_id>`.
--
-- M11.5 stored the palette as JSON on assets, which is the right shape for the
-- panel (§8) and the wrong one for a query: matching "assets containing a colour
-- within a tolerance" against a JSON blob means scanning and parsing every row.
-- §7 calls colour search the real daily problem — "palette mismatch is the main
-- thing that makes such a game look incoherent" — so it gets an indexed table.
--
-- This is derived data, like phash and palette_json: it is reconstructed by
-- re-running derive, never read back as a source of truth, and `rebuild-index`
-- rebuilds it the same way (§12, invariant 2).

CREATE TABLE asset_swatches (
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    -- Position in the palette, 0 = most used. Part of the key so re-deriving an
    -- asset replaces its rows rather than accumulating them.
    rank     INTEGER NOT NULL,

    -- 0–255 per channel, kept as separate columns so a tolerance box is a range
    -- query the index can use rather than arithmetic over a packed integer.
    r        INTEGER NOT NULL CHECK (r BETWEEN 0 AND 255),
    g        INTEGER NOT NULL CHECK (g BETWEEN 0 AND 255),
    b        INTEGER NOT NULL CHECK (b BETWEEN 0 AND 255),

    -- Share of visible pixels, 0–1. Lets a query ignore a colour that is one stray
    -- pixel: "contains this colour" should not mean "has three pixels of it".
    ratio    REAL    NOT NULL,

    PRIMARY KEY (asset_id, rank)
) STRICT;

-- The colour-box lookup: `r BETWEEN ? AND ?` uses the leading column and the rest
-- filter. ratio trails so the "ignore near-invisible swatches" predicate is covered
-- by the same index.
CREATE INDEX asset_swatches_rgb_idx ON asset_swatches(r, g, b, ratio);

-- palette-near reads one asset's swatches back, and a derive re-run deletes by
-- asset. The primary key already covers both, so no second index here.

-- Backfill from what M11.5 already stored, so colour search works on an existing
-- library immediately rather than only after a full re-derive. json_each is
-- available in modernc.org/sqlite (asserted by TestJSON1IsAvailable, because a
-- migration is the wrong place to discover otherwise).
--
-- The rank comes from json_each.key, which for an array is the element index — the
-- same most-used-first order internal/palette writes.
INSERT INTO asset_swatches (asset_id, rank, r, g, b, ratio)
SELECT a.id,
       s.key,
       json_extract(s.value, '$.r'),
       json_extract(s.value, '$.g'),
       json_extract(s.value, '$.b'),
       coalesce(json_extract(s.value, '$.ratio'), 0)
FROM assets a
JOIN json_each(a.palette_json) s
WHERE a.palette_json IS NOT NULL
  AND json_valid(a.palette_json)
  AND json_extract(s.value, '$.r') IS NOT NULL
  AND json_extract(s.value, '$.g') IS NOT NULL
  AND json_extract(s.value, '$.b') IS NOT NULL;

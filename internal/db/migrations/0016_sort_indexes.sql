-- M16: indexes for the grid's browse orders.
--
-- Until now the grid had one order — filename A→Z — and `assets.filename` was already
-- indexed for it. M16 lets you sort by when a file was indexed, its own mtime, its size and
-- its pixel area, and each of those is an ORDER BY over the whole result set feeding a
-- LIMIT. Without an index SQLite sorts every matching row in a temp b-tree before returning
-- a hundred of them, which on a NAS with a weak CPU and pure-Go SQLite is exactly the kind
-- of per-page cost this milestone is trying to remove.
--
-- first_seen_at is the default order, so it matters most.
--
-- No index for the pixel-area sort: it orders by width*height, a computed expression, and an
-- expression index would have to be maintained on every scan for the least-used order in
-- the list. It sorts in memory, which for the size of result set that order is useful on
-- (one folder of sprites, not the whole library) is the right trade.

CREATE INDEX IF NOT EXISTS idx_assets_first_seen ON assets (first_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_assets_mtime ON assets (mtime DESC);
CREATE INDEX IF NOT EXISTS idx_assets_size ON assets (size DESC);
CREATE INDEX IF NOT EXISTS idx_assets_kind_filename ON assets (kind, filename);

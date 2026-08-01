-- M17: sorting the grid by triangle count.
--
-- 0016 added an index per sort order for the same reason this one exists: a grid sort is
-- an ORDER BY over the whole table with a small LIMIT, which without an index is a full
-- scan and a sort of every row to show a hundred. The library has 926 models today and the
-- point of the order is to browse them by cost, so it is a query that gets used.
--
-- Partial: the count is NULL for every image, audio file and font, and those rows would be
-- most of an unfiltered index while never being what the order is for. A partial index is
-- also what the descending direction wants — `coalesce(tri_count, 0) DESC` cannot use an
-- index anyway, but the ascending direction's `tri_count IS NULL, tri_count ASC` reads
-- straight off this one for the rows that have a value.

CREATE INDEX IF NOT EXISTS idx_assets_tri_count
    ON assets (tri_count)
    WHERE tri_count IS NOT NULL;

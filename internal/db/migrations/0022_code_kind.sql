-- C and C++ source is `script` now, not `other`.
--
-- Unreal packs ship code — a plugin's Source/ folder, a sample's game module — and it was
-- indexed as `other`, next to loose text files, so "the code in this pack" had no filter.
-- The scan only classifies files it is writing, and an unchanged file is not rewritten, so
-- the rows already in the index are moved here once.
--
-- The `type:other` tag the auto tagger gave these rows goes too: the tagger is additive
-- (it adds `type:script` on its next pass) and leaves a tag it no longer derives in place,
-- which would keep them in the "other" facet for ever. Only the machine's own tag is
-- removed; a person's is left alone.

DELETE FROM asset_tags
WHERE source = 'auto_type'
  AND tag_id IN (SELECT id FROM tags WHERE namespace = 'type' AND name = 'other')
  AND asset_id IN (
      SELECT id FROM assets
      WHERE kind = 'other'
        AND lower(ext) IN ('c', 'cc', 'cpp', 'cxx', 'h', 'hh', 'hpp', 'hxx', 'inl', 'ush', 'usf'));

UPDATE assets
SET kind = 'script', updated_at = unixepoch()
WHERE kind = 'other'
  AND lower(ext) IN ('c', 'cc', 'cpp', 'cxx', 'h', 'hh', 'hpp', 'hxx', 'inl', 'ush', 'usf');

-- M16 repair: re-derive PSDs, whose previews have a background baked into them.
--
-- Until M16 the PSD decoder read only the flattened composite Photoshop writes
-- (`SkipLayerImage: true`). Vendor PSDs ship a filled `Background` layer at the bottom —
-- CraftPix's is a 1920x1080 leftover behind a 32x32 sprite — so that composite has no
-- alpha at all, and the derived thumbnail and preview are a sprite inside an opaque white
-- box. On the checkerboard the asset looks broken, and the PSD variant of an artwork looks
-- worse than its own PNG.
--
-- The decoder now flattens the visible layers itself and drops a canvas-covering opaque
-- bottom layer, so new derives are correct. The derivatives already on disk are not, and
-- they are keyed by content hash, so nothing re-derives them on its own: derive skips any
-- row that is already 'ok' at the current version.
--
-- Narrow on purpose. Bumping derive.Version would fix these too, at the cost of decoding
-- every asset in the library again — on a NAS with a weak CPU that is hours of work to
-- repair a few hundred files, and CPU cost on that box is a standing complaint. Only PSDs
-- were affected, so only PSDs are reset.
--
-- derive_version = 0 forces the work even if the current version happens to match.

UPDATE assets
SET derive_state   = 'pending',
    derive_error   = '',
    derive_version = 0,
    updated_at     = unixepoch()
WHERE ext = 'psd'
  AND derive_state IN ('ok', 'failed');

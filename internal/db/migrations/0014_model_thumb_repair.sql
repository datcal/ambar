-- M15 repair: undo derive states that a browser-rendered thumbnail wrote by mistake.
--
-- The M15 thumbnailer records `derive_state = 'ok'` after storing a thumbnail, because
-- that is what the grid reads to decide whether a tile has a picture. But 'ok' is also
-- what the *detail page* read to decide that a normalised `preview.glb` existed, and a
-- browser thumbnail produces no glb at all. Every .fbx therefore ended up pointing its
-- viewer at a URL that 404s, opening to an empty stage with no error anywhere.
--
-- The page no longer guesses from this column (it stats the file), and the upload no
-- longer accepts the blank snapshots that FBX produced in the first place. What is left
-- is the bad data already written, which has to be corrected here rather than by hand:
-- an 'ok' row with no preview keeps the tile showing a blank square and the asset
-- claiming a preview it does not have.
--
-- The discriminator is deliberately narrow, because 'ok' is legitimate for most rows:
--
--   * ext in ('fbx', 'blend') — the only formats deriveModel cannot handle in pure Go.
--     glTF, GLB and OBJ normalise here, so their 'ok' is real.
--   * vert_count IS NULL — a real conversion runs model.Analyze over the glb it just
--     wrote and fills the geometry columns (0008). A thumbnail upload touches none of
--     them. So this is precisely "marked derived without anything having been derived",
--     which also leaves rows that Blender genuinely converted alone.
--
-- Setting them back to 'pending' with version 0 makes derive pick them up again and
-- reach its own honest conclusion — needs_blender where there is no Blender, a real
-- preview where there is one.

UPDATE assets
SET derive_state   = 'pending',
    derive_error   = '',
    derive_version = 0,
    updated_at     = unixepoch()
WHERE derive_state = 'ok'
  AND ext IN ('fbx', 'blend')
  AND vert_count IS NULL;

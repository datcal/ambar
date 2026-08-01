-- M17 repair: re-derive glTF, whose preview.glb was never self-contained.
--
-- `gltf.SaveBinary` writes the GLB's BIN chunk only when the first buffer has no URI, and
-- a `.gltf` on disk always has one — the name of the `.bin` beside it. So "normalise to a
-- single self-contained preview.glb" (§6) produced a 1,396-byte file still pointing at a
-- 202 KB buffer, plus a copy of that buffer in the derivative directory that no route
-- serves. Every one of the library's 442 glTF assets opened to an empty 3D stage.
--
-- internal/model now clears the URI so the bytes become the BIN chunk, and inlines any
-- texture that sits beside the model. The derivatives already on disk are keyed by content
-- hash and derive skips anything already 'ok' at the current version, so they will not
-- rebuild on their own.
--
-- Narrow on purpose, for the same reason 0015 was: bumping derive.Version would repair
-- these and decode the entire library again, which on this NAS is hours of CPU to fix 442
-- files. Only glTF was affected — a `.glb` source already has its buffer inline, and OBJ
-- is built in memory — so only glTF is reset.
--
-- Note the viewer no longer depends on this file: since M17 it loads the original through
-- the companion route, which also resolves textures kept in a pack's shared Textures/
-- directory. The repair is for preview.glb's other consumer, the API.
--
-- derive_version = 0 forces the work even if the current version happens to match.
--
-- `missing_since IS NULL` matters, and 0015 should have had it too: EnqueueStale skips
-- absent files and recordForContent skips their rows, so resetting one to 'pending'
-- parks it in a state nothing will ever clear. On this library that would have been 221
-- rows — old paths from a pack that moved, whose content is indexed elsewhere.

UPDATE assets
SET derive_state   = 'pending',
    derive_error   = '',
    derive_version = 0,
    updated_at     = unixepoch()
WHERE ext = 'gltf'
  AND derive_state IN ('ok', 'failed')
  AND missing_since IS NULL;

-- M7: spritesheet frame geometry (§4, §6).
--
-- §14 assigns spritesheets to M7. frame_count and fps already exist (0003, for
-- animation); these carry the grid the detector proposes and the user confirms.
-- All nullable — only spritesheet assets carry them.

ALTER TABLE assets ADD COLUMN frame_w    INTEGER;
ALTER TABLE assets ADD COLUMN frame_h    INTEGER;
ALTER TABLE assets ADD COLUMN frame_cols INTEGER;
ALTER TABLE assets ADD COLUMN frame_rows INTEGER;
-- §6: how the geometry was determined, so a detected guess is distinguishable
-- from a confirmed value. 'sidecar' | 'detected' | 'manual'.
ALTER TABLE assets ADD COLUMN frame_source TEXT;

-- M6: 3D model metadata (§4, §6).
--
-- §14 assigns 3D to M6. The model derive path fills these; the §8 viewer overlays
-- them. animation_names already exists (0003) and is reused for glTF animation
-- clip names. All nullable — only model assets carry them.

ALTER TABLE assets ADD COLUMN tri_count      INTEGER;
ALTER TABLE assets ADD COLUMN vert_count     INTEGER;
-- Bounding-box size per axis in the model's units (metres by glTF convention),
-- the basis for §8's 1.8 m scale reference.
ALTER TABLE assets ADD COLUMN bbox_x         REAL;
ALTER TABLE assets ADD COLUMN bbox_y         REAL;
ALTER TABLE assets ADD COLUMN bbox_z         REAL;
ALTER TABLE assets ADD COLUMN material_count INTEGER;

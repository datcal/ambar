-- M11.5: the extracted colour palette (§8 "Palette panel").
--
-- Same policy as 0002/0003: these columns arrive with the milestone that writes
-- them. asset.derive fills both for 2D images; everything else leaves them NULL.
--
-- palette_json is the swatch list as written by internal/palette:
--   [{"hex":"#rrggbb","r":..,"g":..,"b":..,"count":..,"ratio":..}, ...]
-- ordered by frequency (most-used first). NULL until an image is analysed; an
-- empty array is possible for a fully transparent image, where there are no
-- visible colours to show.
ALTER TABLE assets ADD COLUMN palette_json TEXT;

-- exact | quantized (§8). 'exact' when every distinct visible colour is listed —
-- an indexed image or one with few enough colours that a hex is a promise, not an
-- approximation. 'quantized' for photographic textures where the swatches are a
-- median-cut summary and the UI must say so. NULL when not analysed.
ALTER TABLE assets ADD COLUMN palette_kind TEXT;

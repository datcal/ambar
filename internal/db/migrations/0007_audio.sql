-- M5: audio metadata (§4, §6).
--
-- §14 assigns audio to M5; M2's 0003 created the image/animation derive columns
-- but not these. They are written by the audio derive path and read by the §8
-- audio viewer. All nullable — only audio assets carry them.

ALTER TABLE assets ADD COLUMN duration_ms INTEGER;
ALTER TABLE assets ADD COLUMN sample_rate INTEGER;
ALTER TABLE assets ADD COLUMN channels    INTEGER;
ALTER TABLE assets ADD COLUMN bit_depth   INTEGER;
-- Peak level in dBFS; REAL because it is logarithmic and negative.
ALTER TABLE assets ADD COLUMN peak_dbfs   REAL;
-- §6: probable loop, detected from a sustained, click-free wrap. Advisory.
ALTER TABLE assets ADD COLUMN is_loopable INTEGER;

-- M16: let a running job say how far along it is.
--
-- §12 asked for pollable status and the queue delivered states — queued, running, done,
-- failed — which answers "is it working" but not "will this finish before lunch". On the real
-- library a scan walks twenty thousand files and a derive pass decodes every one of them, and
-- for the whole of that the UI could only say "running". That is the difference between a
-- progress bar and a spinner, and it is the reason people reload pages.
--
-- Three columns rather than a percentage, because the interesting readout is "2431 / 20000
-- files", and a percentage throws away the numbers that make it meaningful. The note carries
-- the phase ("hashing", "reading dimensions") for the jobs that have more than one.
--
-- No index: these are read for the handful of rows that are currently running, which is
-- already a filtered query on state.

ALTER TABLE jobs ADD COLUMN progress_done  INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN progress_total INTEGER NOT NULL DEFAULT 0;
ALTER TABLE jobs ADD COLUMN progress_note  TEXT    NOT NULL DEFAULT '';

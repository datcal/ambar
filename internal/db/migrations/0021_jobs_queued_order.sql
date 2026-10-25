-- The claim query, made a seek.
--
-- jobs_claim_idx is (state, run_after, priority DESC, id), and the claim asks for queued rows
-- whose run_after has passed, highest priority first. The range on run_after means the index
-- cannot also deliver the priority order, so SQLite read every queued row and sorted them —
-- on every claim. Invisible with a few hundred jobs; after a 92,000-file import the queue held
-- 170,000, and two workers re-sorting it for each job they took was most of the server's CPU.
--
-- This index is in claim order, over queued rows only, so the claim reads it from the front
-- and stops at the first row whose run_after has passed — almost always the first row. The
-- query names it with INDEXED BY (jobs.go), because the planner, left to itself, preferred
-- the old index. A delayed retry at the front costs one skipped entry, not a sort.

CREATE INDEX IF NOT EXISTS jobs_queued_order_idx
    ON jobs (priority DESC, id)
    WHERE state = 'queued';

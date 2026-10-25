-- Remote workers: claiming the next queued job of one type.
--
-- A remote worker asks for `asset.derive` work, and while one is connected the server's
-- own workers ask for everything *except* that. jobs_claim_idx orders by priority over
-- every queued row regardless of type, so either question walked the whole queue to find
-- its first match — and after a large import the queue is ninety thousand derive jobs, so
-- the server's idle poll every two seconds would have been a full scan, twice.
--
-- Partial, because only queued rows are ever claimed: a finished job never needs to be
-- found by type again, and done rows are most of the table.

CREATE INDEX IF NOT EXISTS jobs_queued_type_idx
    ON jobs (type, priority DESC, id)
    WHERE state = 'queued';

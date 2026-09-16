-- +goose Up
-- The admission pass's persisted ownership of a queued row mid-release
-- (docs/04-data-model.md section 3.3): 1 while a queued task holds an
-- engine transfer the pass still owes work to — a pending file selection
-- retrying through the resume path, or a release whose final transition
-- did not land over a running transfer. Such a row counts toward the
-- concurrency totals and skips the hold gates, because re-gating it would
-- stamp a hold over a running transfer. engine_ref alone cannot carry
-- that meaning: an ordinary resume requeues a task that still holds its
-- stopped engine handle, and that row must pass both gates like a fresh
-- one. Every state transition clears the flag — ownership exists only
-- while the row is queued.
ALTER TABLE tasks ADD COLUMN admission_pending INTEGER NOT NULL DEFAULT 0;

-- +goose Down
ALTER TABLE tasks DROP COLUMN admission_pending;

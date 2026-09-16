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
-- while the row is queued. A crash that orphans the mark needs no
-- recovery sweep: the next pass selects the queued row and finishes the
-- release through its resume path.
ALTER TABLE tasks ADD COLUMN admission_pending INTEGER NOT NULL DEFAULT 0;

-- Rows that were mid-release under the old counted set — queued and
-- already holding an engine handle — keep their slot and their gate
-- exemption across the upgrade, so the counted set the migration lands
-- on is the one the old predicate produced. The mark's new meaning is
-- stricter than the old predicate: a queued row holding a stopped handle
-- gets a one-time exemption the next pass resolves by releasing it,
-- which the old code did unconditionally.
UPDATE tasks SET admission_pending = 1
WHERE state = 'queued' AND engine_ref IS NOT NULL;

-- +goose Down
ALTER TABLE tasks DROP COLUMN admission_pending;

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// Job is one row of the jobs table — the durable queue of
// docs/04-data-model.md section 3.6.
type Job struct {
	ID          string  `db:"id"`
	Kind        string  `db:"kind"`
	TaskID      *string `db:"task_id"`
	PayloadJSON string  `db:"payload_json"`
	State       string  `db:"state"` // pending | running | done | failed
	Attempts    int     `db:"attempts"`
	MaxAttempts int     `db:"max_attempts"`
	RunAfter    int64   `db:"run_after"` // unix ms
	LockedAt    *int64  `db:"locked_at"`
	LastError   *string `db:"last_error"`
}

const (
	queryEnqueueJob = `INSERT INTO jobs
(id, kind, task_id, payload_json, run_after, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`

	// The claim statement is byte-identical to docs/04-data-model.md
	// section 3.6; RETURNING makes the claim atomic.
	queryClaimJob = `UPDATE jobs
   SET state = 'running', locked_at = :now, attempts = attempts + 1, updated_at = :now
 WHERE id = (SELECT id FROM jobs
              WHERE state = 'pending' AND run_after <= :now
              ORDER BY run_after LIMIT 1)
RETURNING id, kind, task_id, payload_json, attempts, max_attempts;`

	queryCompleteJob = `UPDATE jobs
SET state = 'done', updated_at = ?
WHERE id = ?`

	queryRescheduleJob = `UPDATE jobs
SET state = 'pending', locked_at = NULL, last_error = ?, run_after = ?, updated_at = ?
WHERE id = ?`

	queryFailJob = `UPDATE jobs
SET state = 'failed', last_error = ?, updated_at = ?
WHERE id = ?`

	// Boot recovery, docs/04-data-model.md section 3.6 rule 2.
	queryRecoverRunningJobs = `UPDATE jobs SET state='pending', locked_at=NULL WHERE state='running'`
)

const (
	backoffBaseDelayMS = int64(5_000)
	backoffMaxDelayMS  = int64(600_000)
	// 5_000 << 7 = 640_000 already exceeds the cap, so attempts at or past
	// this shift always land on it; the shift never overflows.
	backoffMaxShift = 7
)

// Backoff is run_after = now_ms + min(600000, 5000 * 2^attempts)
// (docs/04-data-model.md section 3.6, rule 3).
func Backoff(now int64, attempts int) int64 {
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= backoffMaxShift {
		return now + backoffMaxDelayMS
	}

	return now + (backoffBaseDelayMS << attempts)
}

// EnqueueJob inserts one pending row and returns its id; payload is stored
// as JSON. state, attempts and max_attempts take their DDL defaults.
func EnqueueJob(ctx context.Context, db *sqlx.DB, kind string, taskID *string, payload any, runAfter int64) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("store: marshal %q job payload: %w", kind, err)
	}

	id := NewID(PrefixJob)
	now := time.Now().UnixMilli()
	if _, err := db.ExecContext(ctx, queryEnqueueJob, id, kind, taskID, string(data), runAfter, now, now); err != nil {
		return "", fmt.Errorf("store: enqueue %q job: %w", kind, err)
	}

	return id, nil
}

// ClaimJob runs the claim statement of docs/04-data-model.md section 3.6
// against the oldest eligible row. It returns ErrNotFound when nothing is
// eligible. The returned row carries only the claimed columns.
func ClaimJob(ctx context.Context, db *sqlx.DB, now int64) (Job, error) {
	query, args, err := sqlx.Named(queryClaimJob, map[string]any{"now": now})
	if err != nil {
		return Job{}, fmt.Errorf("store: bind claim job: %w", err)
	}

	var job Job
	err = db.GetContext(ctx, &job, query, args...)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, fmt.Errorf("store: claim job: %w", ErrNotFound)
	}
	if err != nil {
		return Job{}, fmt.Errorf("store: claim job: %w", err)
	}

	return job, nil
}

// CompleteJob sets state='done'.
func CompleteJob(ctx context.Context, db *sqlx.DB, id string, now int64) error {
	if _, err := db.ExecContext(ctx, queryCompleteJob, now, id); err != nil {
		return fmt.Errorf("store: complete job %q: %w", id, err)
	}

	return nil
}

// FailJob records lastErr and either reschedules with run_after =
// Backoff(now, attempts), or sets state='failed' once
// attempts >= maxAttempts; the failed row stays as its own dead-letter
// record.
func FailJob(ctx context.Context, db *sqlx.DB, id string, attempts, maxAttempts int, lastErr string, now int64) error {
	if attempts >= maxAttempts {
		if _, err := db.ExecContext(ctx, queryFailJob, lastErr, now, id); err != nil {
			return fmt.Errorf("store: fail job %q: %w", id, err)
		}

		return nil
	}

	if _, err := db.ExecContext(ctx, queryRescheduleJob, lastErr, Backoff(now, attempts), now, id); err != nil {
		return fmt.Errorf("store: reschedule job %q: %w", id, err)
	}

	return nil
}

// RecoverRunningJobs returns rows stranded in 'running' by a crash to
// 'pending' and reports how many. Single-process only (ADR-0015): a second
// live worker would have its in-flight rows reset and re-run.
func RecoverRunningJobs(ctx context.Context, db *sqlx.DB) (int64, error) {
	result, err := db.ExecContext(ctx, queryRecoverRunningJobs)
	if err != nil {
		return 0, fmt.Errorf("store: recover running jobs: %w", err)
	}

	recovered, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: recover running jobs: %w", err)
	}

	return recovered, nil
}

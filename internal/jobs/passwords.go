package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/L-K-M/dl-tool/internal/store"
)

// PasswordSource yields the ordered candidate list for one task. It
// generates nothing, loads no wordlist and never retries a failed
// candidate: a fixed operator-supplied list is not a dictionary.
type PasswordSource interface {
	// Candidates returns, in this order: the empty string (an unencrypted
	// archive), the task's own tasks.extract_password when set, then each
	// entry of the extract_passwords settings array in list order.
	// Duplicates are removed keeping the first occurrence. At most
	// MaxCandidates entries are returned after the empty string.
	Candidates(ctx context.Context, taskID string) ([]string, error)

	// Remember appends pw to the shared extract_passwords list when it is
	// not already present. It is called once, after the candidate has
	// successfully opened an archive.
	Remember(ctx context.Context, pw string) error
}

// MaxCandidates caps the shared list, per doc 12 section 4.2.
const MaxCandidates = 16

// queryTaskExtractPassword reads the per-task candidate. The column holds
// a secret: it is bound as a parameter and never enters a log line, an
// error string or a task_events detail.
const queryTaskExtractPassword = `SELECT extract_password FROM tasks WHERE id = ?`

// storePasswords is the PasswordSource backed by tasks.extract_password
// and the extract_passwords settings key.
type storePasswords struct {
	db       *sqlx.DB
	settings *store.SettingsStore
}

// NewStorePasswords returns the PasswordSource backed by
// tasks.extract_password and the extract_passwords settings key.
func NewStorePasswords(db *sqlx.DB) PasswordSource {
	return &storePasswords{db: db, settings: store.NewSettingsStore(db)}
}

// Candidates implements the doc 12 section 4.2 order: the empty string
// first, then the task's own extract_password, then the shared list —
// de-duplicated, first occurrence winning, the non-empty portion capped at
// MaxCandidates.
func (p *storePasswords) Candidates(ctx context.Context, taskID string) ([]string, error) {
	var taskPassword sql.NullString
	err := p.db.GetContext(ctx, &taskPassword, queryTaskExtractPassword, taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("jobs: password candidates for task %q: %w", taskID, store.ErrNotFound)
	}
	if err != nil {
		return nil, fmt.Errorf("jobs: password candidates for task %q: %w", taskID, err)
	}

	shared, err := p.settings.ExtractPasswords(ctx)
	if err != nil {
		return nil, fmt.Errorf("jobs: password candidates for task %q: %w", taskID, err)
	}

	candidates := []string{""}
	seen := map[string]struct{}{"": {}}
	add := func(pw string) {
		// The empty string is already first, so the cap on the remaining
		// entries is len(candidates) - 1 <= MaxCandidates.
		if len(candidates) > MaxCandidates {
			return
		}
		if _, dup := seen[pw]; dup {
			return
		}
		seen[pw] = struct{}{}
		candidates = append(candidates, pw)
	}
	if taskPassword.Valid {
		add(taskPassword.String)
	}
	for _, pw := range shared {
		add(pw)
	}

	return candidates, nil
}

// Remember appends pw to the shared list through
// SettingsStore.AppendExtractPassword, which keeps the list de-duplicated
// and capped. The empty candidate is the unencrypted-archive sentinel, not
// a password, so it is never stored.
func (p *storePasswords) Remember(ctx context.Context, pw string) error {
	if pw == "" {
		return nil
	}

	return p.settings.AppendExtractPassword(ctx, pw)
}

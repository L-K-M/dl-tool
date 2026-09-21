package jobs

import (
	"context"
	"fmt"

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

// MaxCandidates caps the shared list, per doc 12 section 4.2. It aliases
// the store's write-side bound so the two policies cannot drift apart.
const MaxCandidates = store.MaxExtractPasswords

// storePasswords is the PasswordSource backed by tasks.extract_password
// and the extract_passwords settings key. Both reads stay inside the
// store layer so the secret column never crosses it in raw form.
type storePasswords struct {
	tasks    *store.TaskStore
	settings *store.SettingsStore
}

// NewStorePasswords returns the PasswordSource backed by
// tasks.extract_password and the extract_passwords settings key.
func NewStorePasswords(tasks *store.TaskStore) PasswordSource {
	return &storePasswords{tasks: tasks, settings: tasks.Settings()}
}

// Candidates implements the doc 12 section 4.2 order: the empty string
// first, then the task's own extract_password, then the shared list —
// de-duplicated, first occurrence winning, the non-empty portion capped at
// MaxCandidates.
func (p *storePasswords) Candidates(ctx context.Context, taskID string) ([]string, error) {
	taskPassword, err := p.tasks.ExtractPassword(ctx, taskID)
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
	add(taskPassword.Reveal())
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

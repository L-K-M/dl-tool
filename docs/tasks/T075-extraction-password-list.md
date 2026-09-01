# T075 — Try the per-task and shared extraction passwords

| Field | Value |
|---|---|
| **ID** | T075 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T074 |
| **Blocks** | T108 |
| **Parallel-safe** | no — extends `internal/jobs/handlers_extract.go` |
| **Implements** | [FR-101](../02-requirements.md#fr-101-try-a-shared-password-list-and-per-task-passwords) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 2 new files, ~200 LOC |

## Goal
An encrypted archive is opened by trying the task's own `extract_password` first and then each entry of the
shared `extract_passwords` settings list, each candidate exactly once. A per-task password supplied at
creation is appended to the shared list. Exhausting the list sets `extract_failed_wrong_password`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/12-security-and-threat-model.md` §4.2 Passwords](../12-security-and-threat-model.md#42-passwords)
2. [`docs/02-requirements.md` FR-101](../02-requirements.md#fr-101-try-a-shared-password-list-and-per-task-passwords)
3. [`docs/11-config-reference.md` §5 Database-backed settings](../11-config-reference.md#5-database-backed-settings)
4. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
5. [`docs/tasks/T074-auto-extract-archives.md`](T074-auto-extract-archives.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/passwords.go` | create | `PasswordSource`, `Candidates` and the append-on-success rule. |
| `internal/jobs/passwords_test.go` | create | Order, once-only, append and exhaustion cases. |
| `internal/jobs/handlers_extract.go` | modify | Drive pass 2 from `Candidates` instead of a single password. |
| `internal/store/settings.go` | modify | Add `ExtractPasswords` and `AppendExtractPassword`. |

No other file may be modified.

## Interface contract

```go
package jobs

// PasswordSource yields the ordered candidate list for one task. It generates nothing, loads no
// wordlist and never retries a failed candidate: a fixed operator-supplied list is not a dictionary.
type PasswordSource interface {
	// Candidates returns, in this order: the empty string (an unencrypted archive), the task's own
	// tasks.extract_password when set, then each entry of the extract_passwords settings array in
	// list order. Duplicates are removed keeping the first occurrence. At most MaxCandidates entries
	// are returned after the empty string.
	Candidates(ctx context.Context, taskID string) ([]string, error)

	// Remember appends pw to the shared extract_passwords list when it is not already present.
	// It is called once, after the candidate has successfully opened an archive.
	Remember(ctx context.Context, pw string) error
}

// MaxCandidates caps the shared list, per doc 12 §4.2.
const MaxCandidates = 16

// NewStorePasswords returns the PasswordSource backed by tasks.extract_password and the
// extract_passwords settings key.
func NewStorePasswords(st *store.Store) PasswordSource
```

```go
package store

// ExtractPasswords reads the extract_passwords settings key. It returns an empty slice when the key
// is absent. The value is a secret: it is never logged and GET /settings renders it "__redacted__".
func (s *SettingsStore) ExtractPasswords(ctx context.Context) ([]string, error)

// AppendExtractPassword appends pw to that array in one transaction when it is absent, keeping at
// most jobs.MaxCandidates entries, oldest dropped first.
func (s *SettingsStore) AppendExtractPassword(ctx context.Context, pw string) error
```

Pass 2 keeps the argument vector of T074 and substitutes only `-p<candidate>`. A candidate that fails is
distinguished from a corrupt archive by the `7zz` exit status: a wrong password is retried with the next
candidate, an invalid archive aborts the whole job at once with `ErrInvalidArchive`.

## Steps
1. Create `internal/jobs/passwords.go` with `PasswordSource`, `MaxCandidates` and `NewStorePasswords`.
2. Implement `Candidates` in the documented order, de-duplicating while keeping the first occurrence, and
   capping the shared portion at `MaxCandidates`.
3. Implement `Remember` over `AppendExtractPassword`, called exactly once per successful extraction.
4. Add `ExtractPasswords` and `AppendExtractPassword` to `internal/store/settings.go`, both writing an
   explicit column list and the append running inside one `sqlx.Tx`.
5. Edit `internal/jobs/handlers_extract.go` so pass 2 loops over `Candidates`, deleting `tmp` between
   attempts, and returns `ErrWrongPassword` after the last candidate fails.
6. Ensure no candidate, and no `-p` argument, ever reaches a log record, an `error` string or a
   `task_events.detail_json` value.
7. Create `internal/jobs/passwords_test.go`: assert the task password is tried before the shared list;
   assert a duplicate is tried once; assert a successful candidate is appended to the shared list and a
   failing one is not; assert the list is capped at `MaxCandidates`; assert an archive whose password is in
   no list ends with `extract_failed_wrong_password`; assert no test log line contains the password.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] Candidate order is empty string, then `tasks.extract_password`, then the shared list in order.
- [ ] Each candidate is tried exactly once; a failed candidate is never retried.
- [ ] A password that opened an archive is present in `extract_passwords` afterwards.
- [ ] The shared list never exceeds `MaxCandidates` entries.
- [ ] Exhausting the list sets `error_code` `extract_failed_wrong_password` and leaves the archive in place.
- [ ] No password appears in any log record or error message produced by the package.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/store/..." && echo EXTRACT_PW_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestCandidateOrder`, `TestCandidateTriedOnce`, `TestSuccessAppendsToSharedList`,
`TestSharedListCappedAt16`, `TestExhaustedListSetsWrongPassword` and `TestPasswordNeverLogged` each
reported as `--- PASS`. The final line of stdout is exactly `EXTRACT_PW_OK`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT change the three-pass recipe, the caps or the failure mapping; T074 owns them.
- Do NOT generate, mutate, permute or brute-force any password, and do NOT ship a default list.
- Do NOT expose `extract_passwords` through `GET /settings` in clear text; T092 owns the redaction.
- Do NOT read a password from a file, an environment variable or the archive itself.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

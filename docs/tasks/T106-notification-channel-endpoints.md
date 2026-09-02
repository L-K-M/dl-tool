# T106 — Manage notification channels and test one

| Field | Value |
|---|---|
| **ID** | T106 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T077, T084 |
| **Blocks** | T108, T120 |
| **Parallel-safe** | no — it also edits the shared files `internal/api/server.go`, `internal/store/settings.go` |
| **Implements** | [FR-107](../02-requirements.md#fr-107-manage-notification-channels), [FR-104](../02-requirements.md#fr-104-send-notifications-and-offer-a-per-channel-test) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
`/notifications` creates, lists, updates and deletes channels of kind `webhook`, `ntfy`, `gotify` and
`apprise`. No read response ever contains the stored secret. `POST /notifications/{id}/test` delivers one
synthetic event and returns the **raw upstream status line and body**, so a failing channel is diagnosable
without reading the server log.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §14 Notification channels](../05-api-contract.md#14-notification-channels)
2. [`docs/05-api-contract.md` §14.1 `POST /notifications/{id}/test`](../05-api-contract.md#141-post-notificationsidtest)
3. [`docs/04-data-model.md` §4.8 `notification_channels.kind`](../04-data-model.md#48-notification_channelskind)
4. [`docs/12-security-and-threat-model.md` §2 SSRF](../12-security-and-threat-model.md#2-ssrf)
5. [`docs/tasks/T077-notification-delivery.md`](T077-notification-delivery.md)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/api/notifications.go` | create | The five operations, the per-kind config validation and the secret rules. |
| `internal/api/notifications_test.go` | create | CRUD, secret, mask, immutable-kind and raw-reply cases. |
| `internal/store/settings.go` | modify | Add `CreateNotificationChannel`, `UpdateNotificationChannel`, `DeleteNotificationChannel`. |
| `internal/api/server.go` | modify | Register the five operations. |

No other file may be modified.

## Interface contract

```go
package api

// ChannelView is the only shape returned for an existing channel. There is no secret member:
// secret_set reports whether one is stored.
type ChannelView struct {
	ID         string         `json:"id"`   // ntf_ + ULID
	Kind       string         `json:"kind"` // webhook | ntfy | gotify | apprise
	Name       string         `json:"name"`
	Enabled    bool           `json:"enabled"`
	Config     map[string]any `json:"config"`
	SecretSet  bool           `json:"secret_set"`
	EventMask  []string       `json:"event_mask"` // task_events.code values, or ["*"]
	LastSendAt *time.Time     `json:"last_send_at"`
	LastError  *string        `json:"last_error"`
	CreatedAt  time.Time      `json:"created_at"`
	UpdatedAt  time.Time      `json:"updated_at"`
}

// ChannelWriteBody is the create and patch body. Secret is write-only.
type ChannelWriteBody struct {
	Kind      string         `json:"kind,omitempty" enum:"webhook,ntfy,gotify,apprise"`
	Name      string         `json:"name,omitempty"`
	Enabled   *bool          `json:"enabled,omitempty"`
	Config    map[string]any `json:"config,omitempty"`
	Secret    *string        `json:"secret,omitempty"` // "__redacted__" leaves it unchanged; null clears it
	EventMask []string       `json:"event_mask,omitempty"`
}

// TestInput selects which event template is rendered; the default code is "task.completed".
type TestInput struct {
	ID   string `path:"id"`
	Body struct {
		Code string `json:"code,omitempty"`
	}
}

// TestOutput is jobs.RawReply verbatim: status_line, status, headers and the first 8 KiB of body,
// neither parsed nor reformatted. A channel that answered is always 200 with ok reflecting the
// upstream status; a channel that could not be reached is 200 with response null and the transport
// failure in error.
type TestOutput struct{ Body jobs.RawReply }

func (h *NotificationHandlers) List(ctx context.Context, in *struct{}) (*ListChannelsOutput, error)
func (h *NotificationHandlers) Create(ctx context.Context, in *CreateChannelInput) (*ChannelOutput, error)
func (h *NotificationHandlers) Patch(ctx context.Context, in *PatchChannelInput) (*ChannelOutput, error)
func (h *NotificationHandlers) Delete(ctx context.Context, in *DeleteChannelInput) (*struct{}, error)
func (h *NotificationHandlers) Test(ctx context.Context, in *TestInput) (*TestOutput, error)
```

Enforced rules: `kind` is immutable after creation and a `PATCH` changing it is
`422`; a duplicate `name` is `409` `/problems/conflict`; the `config` key set is fixed per kind by doc 04
§4.8 and an unknown key is `422`; `event_mask` defaults to `["*"]`; a `config` URL is checked against the
SSRF guard at save time and again at send time, with a blocked target returning `403`
`/problems/ssrf-blocked`. A test send is not recorded against `event_mask` and writes no `task_events` row.

## Steps
1. Add the three write queries to `internal/store/settings.go`, storing the secret in `secret_enc` and
   never selecting it into a struct that reaches an API response.
2. Create `internal/api/notifications.go` with `ChannelView`, `ChannelWriteBody` and the five operations,
   registered in `internal/api/server.go`.
3. Validate `config` against the per-kind key set of doc 04 §4.8, rejecting an unknown key with `422` and an
   `errors[].location` naming it.
4. Implement the three secret behaviours exactly: a new value replaces, `"__redacted__"` leaves unchanged,
   `null` clears. Report only `secret_set` on read.
5. Reject a `PATCH` that changes `kind` with `422`, and a duplicate `name` with `409`.
6. Implement `Test` by calling `jobs.Notifier.Send` with the template for `code` (default
   `task.completed`) and returning its `RawReply` unchanged — do not re-render, re-wrap or summarise it.
7. Edit `internal/api/server.go` to register the five operations.
8. Create `internal/api/notifications_test.go` with `humatest` and an `httptest` stub: create one channel of
   each kind and assert no read response contains the secret; assert `secret_set` is true after a write and
   false after a `null`; assert `"__redacted__"` leaves the stored value unchanged; assert a mask of
   `["task.completed"]` receives a `completed` event and not an `error` one; assert a `kind` change is `422`;
   assert a duplicate name is `409`; assert `Test` against a `403` stub returns `ok:false` with the stub's
   status line and body verbatim.
9. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] One channel of each of the four kinds can be created, patched, listed and deleted.
- [ ] No read response contains the stored secret; `secret_set` reports its presence.
- [ ] `"__redacted__"` leaves the secret unchanged and `null` clears it.
- [ ] A mask holding only `completed` receives a `completed` event and not an `error` one.
- [ ] Changing `kind` is `422`; a duplicate `name` is `409`.
- [ ] `POST /notifications/{id}/test` returns the upstream status line and body verbatim.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/api/... ./internal/store/..." && echo NOTIFY_API_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/api` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestChannelCrudAllKinds`, `TestSecretNeverReturned`, `TestRedactedLeavesSecret`,
`TestNullClearsSecret`, `TestEventMaskFilters`, `TestKindImmutable`, `TestDuplicateNameConflict`,
`TestTestReturnsRawUpstreamReply` and `TestNonAdminForbidden` each reported as `--- PASS`. The final line of
stdout is exactly `NOTIFY_API_OK`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT re-implement delivery, the per-kind request shapes or the SSRF-guarded client; T077 owns
  `internal/jobs/handlers_notify.go` and this task calls `Send`.
- Do NOT build the notification settings matrix UI; T053 owns the settings screens.
- Do NOT parse, pretty-print, JSON-decode or summarise the upstream body; return the first 8 KiB verbatim.
- Do NOT invent an event enum; the mask holds `task_events.code` values or the single element `["*"]`.
- Do NOT record a test send against `event_mask` or write a `task_events` row for it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

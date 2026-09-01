# T077 — Deliver task events to notification channels

| Field | Value |
|---|---|
| **ID** | T077 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T054, T074 |
| **Blocks** | T106 |
| **Parallel-safe** | no — it also edits the shared files `internal/jobs/postprocess.go`, `internal/store/settings.go` |
| **Implements** | [FR-104](../02-requirements.md#fr-104-send-notifications-and-offer-a-per-channel-test) |
| **Decisions** | [ADR-0015](../decisions/0015-db-backed-in-process-job-queue.md) |
| **Est. size** | 2 new files, ~330 LOC |

## Goal
A `task_events` code reaches every enabled notification channel whose `event_mask` selects it, over the
`webhook`, `ntfy`, `gotify` and `apprise` request shapes, through the SSRF-guarded client. Every send returns
the raw upstream status line and body so a failing channel is diagnosable.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/04-data-model.md` §4.8 `notification_channels.kind`](../04-data-model.md#48-notification_channelskind)
2. [`docs/05-api-contract.md` §14 Notification channels](../05-api-contract.md#14-notification-channels)
3. [`docs/12-security-and-threat-model.md` §2 SSRF](../12-security-and-threat-model.md#2-ssrf)
4. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
5. [`docs/11-config-reference.md` §2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/jobs/handlers_notify.go` | create | `Notifier`, the four kind renderers, `Send` and the `webhook` job handler. |
| `internal/jobs/handlers_notify_test.go` | create | Mask, per-kind shape, redaction and stub-server cases. |
| `internal/store/settings.go` | modify | Add `ListNotificationChannels`, `GetNotificationChannel`, `TouchNotificationChannel`. |
| `internal/jobs/postprocess.go` | modify | Enqueue one `webhook` job per matching channel. |

No other file may be modified.

## Interface contract

```go
package jobs

// Event is the payload rendered into a channel request. Code is a task_events.code value.
type Event struct {
	Code     string            `json:"code"`
	TaskID   string            `json:"task_id"`
	Name     string            `json:"name"`
	State    string            `json:"state"`
	Message  string            `json:"message"`
	At       time.Time         `json:"at"`
	Detail   map[string]any    `json:"detail,omitempty"`
}

// RawReply is exactly what the upstream answered, unparsed and unreformatted. It is the body of
// POST /notifications/{id}/test in docs/05-api-contract.md §14.1.
type RawReply struct {
	OK         bool              `json:"ok"`
	ElapsedMS  int64             `json:"elapsed_ms"`
	Request    struct {
		Method string `json:"method"`
		URL    string `json:"url"` // credentials and query secrets redacted
	} `json:"request"`
	Response *struct {
		StatusLine string            `json:"status_line"`
		Status     int               `json:"status"`
		Headers    map[string]string `json:"headers"`
		Body       string            `json:"body"` // first 8 KiB verbatim, truncated, never summarised
	} `json:"response"`
	Error *string `json:"error"` // transport failure when Response is nil
}

// BodyCap is the verbatim-body cap of doc 05 §14.1.
const BodyCap = 8 << 10

// Notifier delivers one Event to one channel.
type Notifier struct{ /* store *store.Store; client *http.Client */ }

// NewNotifier takes the SSRF-guarded client returned by secure.NewClient; it never builds its own.
func NewNotifier(st *store.Store, client *http.Client) *Notifier

// Send renders ev for the channel's kind, checks the target URL against the SSRF guard at send time
// as well as at save time, performs the request and returns the raw reply. A non-2xx upstream is
// not a Go error: it is returned with OK false and the response filled in.
func (n *Notifier) Send(ctx context.Context, ch store.NotificationChannel, ev Event) (RawReply, error)

// Matches reports whether ch.EventMask selects code. The mask ["*"] selects every code.
func Matches(mask []string, code string) bool

// Fanout enqueues one "webhook" job per enabled channel whose mask selects ev.Code.
func (n *Notifier) Fanout(ctx context.Context, ev Event) error
```

Request shape per kind, built from `config_json`:

| `kind` | `config_json` keys used | Request |
|---|---|---|
| `webhook` | `url`, `method`, `headers`, `body_template` | `<method>` (default `POST`) to `url` with `headers`; body is `body_template` rendered from `Event`, or the `Event` JSON when the template is empty. |
| `ntfy` | `server_url`, `topic`, `priority`, `tags`, `click_url` | `POST <server_url>/<topic>`, plain-text body, `Priority`, `Tags` and `Click` headers, the secret as `Authorization: Bearer`. |
| `gotify` | `server_url`, `priority` | `POST <server_url>/message`, form fields `title`, `message`, `priority`, the secret as the `X-Gotify-Key` header. |
| `apprise` | `base_url`, `config_key`, `urls`, `tag`, `type`, `format` | `POST <base_url>/notify/<config_key>`, JSON body carrying `urls`, `tag`, `type`, `format`, `title` and `body`. |

The key sets are owned by [`04-data-model.md` §4.8](../04-data-model.md#48-notification_channelskind); an
unknown key is a validation error, never a silently ignored field.

## Steps
1. Add `ListNotificationChannels`, `GetNotificationChannel` and `TouchNotificationChannel` to
   `internal/store/settings.go`, decrypting `secret_enc` only inside the notifier and never selecting it
   into a struct that is serialised to an API response.
2. Create `internal/jobs/handlers_notify.go` with `Event`, `RawReply`, `Notifier`, `Matches` and `Fanout`.
3. Implement one renderer per kind from the table above, reading only the keys named there and failing with
   a validation error on an unknown key.
4. Route every request through the T054 SSRF-guarded client, re-checking the resolved peer address at send
   time; a blocked target returns `RawReply` with `Error` set and writes `last_error` on the channel.
5. Capture the upstream status line verbatim and the first `BodyCap` bytes of the body verbatim; truncate,
   never summarise, and never parse.
6. Redact credentials and query secrets from `Request.URL`, and never echo the stored secret anywhere.
7. Register the `webhook` job handler with the T012 worker pool and make it idempotent on
   `(kind, task_id)`; a delivery that already succeeded is not repeated on retry.
8. Edit `internal/jobs/postprocess.go` to call `Fanout` with the chain's terminal event.
9. Create `internal/jobs/handlers_notify_test.go` against `httptest` stubs: assert a channel whose mask is
   `["task.completed"]` receives a `task.completed` event and not a `task.error` one; assert one request
   shape per kind; assert a `403` stub yields `OK:false` with the stub's status line and body verbatim;
   assert a body over `BodyCap` is truncated to exactly `BodyCap` bytes; assert the stored secret appears in
   no response and no log line; assert a disabled channel receives nothing.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] A `task.completed` event reaches a stub webhook and the stub records the `Event` JSON.
- [ ] `event_mask` filtering is exact: `["task.completed"]` excludes `task.error`; `["*"]` includes both.
- [ ] Each of the four kinds produces the request shape in the table above.
- [ ] A `403` upstream returns `ok:false` with `status_line` and `body` verbatim, and `error` null.
- [ ] An unreachable upstream returns `ok:false`, `response:null` and the transport error in `error`.
- [ ] The stored secret never appears in a `RawReply`, a log record or a `task_events` row.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/jobs/... ./internal/store/..." && echo NOTIFY_OK
```
Expected: `ok  github.com/L-K-M/dl-tool/internal/jobs` and `ok  github.com/L-K-M/dl-tool/internal/store`,
with `TestMaskSelectsOnlyListedCodes`, `TestWebhookShape`, `TestNtfyShape`, `TestGotifyShape`,
`TestAppriseShape`, `TestRawReplyCarriesStatusLine`, `TestBodyTruncatedAt8KiB` and
`TestSecretNeverEchoed` each reported as `--- PASS`. The final line of stdout is exactly `NOTIFY_OK`.
No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add the `/notifications` CRUD endpoints or `POST /notifications/{id}/test`; T106 owns both and
  calls `Send` from this task.
- Do NOT build the notification settings matrix UI; T053 owns the settings screens.
- Do NOT invent an event vocabulary: the mask values are `task_events.code` values and nothing else.
- Do NOT write your own HTTP client, redirect policy or DNS resolution; use the T054 guarded client.
- Do NOT retry a delivery inside `Send`; the worker pool's backoff is the only retry.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

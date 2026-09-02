# T120 — Build the Account and Notifications settings sections

| Field | Value |
|---|---|
| **ID** | T120 |
| **Milestone** | M6 |
| **Status** | todo |
| **Depends on** | T053, T084, T106 |
| **Blocks** | — |
| **Parallel-safe** | no — it edits `SettingsScreen.tsx` and `settings.json`, shared with T116–T119 and T121 |
| **Implements** | — (renders [FR-104](../02-requirements.md#fr-104-send-notifications-and-offer-a-per-channel-test) covered by T077, [FR-107](../02-requirements.md#fr-107-manage-notification-channels) covered by T106, and [FR-117](../02-requirements.md#fr-117-issue-and-revoke-api-tokens) covered by T084) |
| **Decisions** | [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md), [ADR-0013](../decisions/0013-mandatory-built-in-authentication.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`/settings/account` changes the operator's password and issues API tokens whose secret is shown exactly
once and never again. `/settings/notifications` renders the per-event × per-channel matrix
whose `Send test` shows the raw upstream status line and body.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/09-web-ui-spec.md` §9 Settings screens](../09-web-ui-spec.md#9-settings-screens) — the
   **Account** and **Notifications** rows, and the dirty-bar rule.
2. [`docs/05-api-contract.md` §12 The account and API tokens](../05-api-contract.md#12-the-account-and-api-tokens)
   — the account object, the `current_password` rule, the create-only `token` member, and every status code.
3. [`docs/05-api-contract.md` §14 Notification channels](../05-api-contract.md#14-notification-channels) — the
   channel object, `secret_set`, the immutable `kind` and `event_mask`.
4. [`docs/05-api-contract.md` §14.1 `POST /notifications/{id}/test`](../05-api-contract.md#141-post-notificationsidtest)
   — the raw-reply shape, the always-`200` rule and the 8 KiB body cap.
5. [`docs/14-conventions.md` §4 The `task_events` code vocabulary](../14-conventions.md#4-the-task_events-code-vocabulary)
   — the codes an event mask is drawn from.
6. [`docs/11-config-reference.md` §1 Precedence rule](../11-config-reference.md#1-precedence-rule) and
   [§2 `DLTOOL_` variables (application)](../11-config-reference.md#2-dltool_-variables-application) — why
   session lifetime is read-only here.
7. [`docs/tasks/T053-settings-screens.md`](T053-settings-screens.md) — `SECTIONS`, `IMPLEMENTED` and the
   `Save / Revert` bar.

## Files
| Path | Action | Purpose |
|---|---|---|
| `web/src/components/Settings/AccountSection.tsx` | create | Password change, session-lifetime note and API tokens. |
| `web/src/components/Settings/NotificationsSection.tsx` | create | Channel list, the event × channel matrix and `Send test`. |
| `web/src/components/Settings/AccountSection.test.tsx` | create | Both sections: reveal-once, password change, matrix round-trip and raw reply. |
| `web/src/components/Settings/SettingsScreen.tsx` | edit | Add `account` and `notifications` to `IMPLEMENTED` and render them. |
| `web/src/locales/en/settings.json` | edit | Labels, column headers, the reveal warning and the event names. |

No other file may be modified.

## Interface contract

```tsx
// web/src/components/Settings/AccountSection.tsx

/** GET /account, doc 05 §12. There is no password member in any read shape. */
export interface Account {
  id: string;                          // usr_ + ULID
  username: string;
  enabled: boolean;
  locale: string;
  last_login_at: string | null;
  created_at: string;
}

/** PATCH /account. `current_password` is required whenever `password` is present, and the call revokes
 *  every session except the caller's. API tokens are unaffected. */
export interface AccountPatch {
  username?: string;
  locale?: string;
  password?: string;                   // minimum 12 characters
  current_password?: string;
}

/** One row of GET /api-tokens. Never carries the secret. */
export interface TokenRow {
  id: string;                          // tok_ + ULID
  name: string;
  prefix: string;                      // first 8 characters, display only
  last_used_at: string | null;
  expires_at: string | null;
  created_at: string;
}

/** The create-once reveal. `token` comes only from the 201 body of POST /api-tokens and is held in
 *  component state alone: it is never written to prefs, localStorage, a query cache or a log, and the
 *  dialog cannot be reopened once closed. Closing it clears the state. */
export interface TokenRevealProps {
  token: string;                       // "dlt_" + 32 hex characters
  onClose: () => void;                 // must set the holding state to null
}
export function TokenRevealDialog(props: TokenRevealProps): JSX.Element;

export function AccountSection(): JSX.Element;
```

Account fields and their writes. dl-tool has one account
([ADR-0019](../decisions/0019-single-account-no-ownership.md)), so there is no user list, no role and no
delete:

| Field | Write |
|---|---|
| Username, Locale | `PATCH /account` |
| Password | `PATCH /account` with `password` and `current_password`, minimum 12 characters |

`403 /problems/forbidden` means `current_password` did not match, and renders as a field-level message on
that input, not a generic toast. `422` is a password shorter than 12 characters.

**Session lifetime is not editable.** `DLTOOL_SESSION_TTL` is an `infrastructure` variable
([`11-config-reference.md` §1](../11-config-reference.md#1-precedence-rule)), no endpoint returns its value,
and no API can change it. The row is static text — `Session lifetime is set by DLTOOL_SESSION_TTL, default
720h; restart the container to change it.` — with no input and no value read from the server.

```tsx
// web/src/components/Settings/NotificationsSection.tsx

/** One row of GET /notifications, doc 05 §14. There is no secret member: secret_set reports
 *  whether one is stored. */
export interface ChannelRow {
  id: string;                                            // ntf_ + ULID
  kind: 'webhook' | 'ntfy' | 'gotify' | 'apprise';       // immutable after creation
  name: string;
  enabled: boolean;
  config: Record<string, unknown>;
  secret_set: boolean;
  event_mask: string[];                                  // task_events codes, or ["*"]
  last_send_at: string | null;
  last_error: string | null;
}

/** Matrix rows, in this order. Values are task_events.code strings from doc 14 §4.
 *  A stored mask entry outside this list is rendered as an extra read-only row so an existing
 *  mask is never silently dropped when the matrix is saved. */
export const NOTIFIABLE_EVENTS = [
  'task.created', 'task.completed', 'task.error', 'task.paused', 'task.resumed',
  'task.removed', 'task.data_deleted', 'task.force_completed',
  'engine.rejected', 'engine.unavailable',
  'postprocess.extract.completed', 'postprocess.extract.failed',
  'postprocess.move.failed', 'postprocess.hook.failed',
] as const;

/** A checked box adds the code to that channel's event_mask; clearing every box sends [].
 *  The single "All events" row is the wire value ["*"]: checking it replaces the mask with ["*"]
 *  and disables the per-event boxes, and unchecking it restores the codes that were checked. */
export function NotificationsSection(): JSX.Element;
```

`Send test` posts `POST /notifications/{id}/test` and renders the reply **verbatim**:

```json
{"ok":false,"elapsed_ms":214,
 "request":{"method":"POST","url":"https://ntfy.sh/dl-tool-alice"},
 "response":{"status_line":"HTTP/1.1 403 Forbidden","status":403,
             "headers":{"content-type":"application/json"},
             "body":"{\"code\":40301,\"http\":403,\"error\":\"forbidden\"}"},
 "error":null}
```

- `status_line` is rendered as-is in a monospace block; `body` is rendered as-is in a scrolling
  `<pre>` — never parsed, prettified, summarised or truncated further than the server's 8 KiB cap.
- `ok:false` is a normal `200` and must **not** raise an error toast; it is the diagnostic result.
- When `response` is `null` the transport `error` string is rendered instead, in the same block.
- The secret input is write-only: it renders empty with `secret_set` shown as `Stored`, sending `null`
  clears it, and the UI never sends `"__redacted__"`.

## Steps
1. Edit `web/src/locales/en/settings.json`: add an `account` subtree (the field labels, the password rule,
   the session-lifetime sentence, the token column headers and the reveal warning) and a `notifications`
   subtree with one label per `NOTIFIABLE_EVENTS` entry plus the four channel kinds.
2. Create `AccountSection.tsx` reading `GET /account` through the T014 `api` client, with a Change-password
   form that sends `password` and `current_password` to `PATCH /account`.
3. State beside the token panel that a token's secret is shown once, is not recoverable, and can only be
   replaced by revoking it and issuing a new one.
4. Render the session-lifetime row as static text with the `DLTOOL_SESSION_TTL` sentence and no input.
5. Build the API-token panel: `GET /api-tokens` listing name, prefix, last used, expires; `POST /api-tokens`
   opening `TokenRevealDialog` with the `token` from the `201` body; `DELETE /api-tokens/{id}` revoking.
6. Make the reveal one-shot: hold the value in component state only, clear it on close, offer `Copy` and a
   warning that it cannot be shown again, and never place it in a query cache, prefs or the URL.
7. Create `NotificationsSection.tsx` with the channel list (add, edit, delete, enable) and the write-only
   secret field, refusing to change `kind` on an existing channel because `PATCH` answers `422`.
8. Build the matrix: `NOTIFIABLE_EVENTS` as rows, channels as columns, the `All events` row for `["*"]`, and
   an extra read-only row for any stored code outside the list; save each column with
   `PATCH /notifications/{id}` carrying only `event_mask`.
9. Add `Send test` per channel, rendering the raw reply exactly as specified, with no error toast on
   `ok:false` and a distinct rendering when `response` is `null`.
10. Edit `SettingsScreen.tsx` to add `'account'` and `'notifications'` to `IMPLEMENTED` and render the two
    components; change nothing else in that file.
11. Create `AccountSection.test.tsx` covering the acceptance criteria below against stubbed endpoints.
12. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestTokenRevealedOnceOnly` asserts the token text is rendered after `201`, is gone after close, and
      appears in no later render, in no `GET /api-tokens` row and in no storage write.
- [ ] `TestWrongCurrentPasswordRendersInline` asserts a `403 /problems/forbidden` renders on the
      `current_password` input, not as a generic toast.
- [ ] `TestPasswordChangeSendsCurrentPassword` asserts the `PATCH /account` body carries both members.
- [ ] `TestMatrixRoundTripsUnknownCode` asserts a stored `event_mask` entry outside `NOTIFIABLE_EVENTS`
      survives a save unchanged.
- [ ] `TestAllEventsRowSendsStar` asserts checking the `All events` row sends `event_mask: ["*"]`.
- [ ] `TestSendTestRendersRawReply` asserts the exact `status_line` and the exact `body` string appear on
      screen and that `ok:false` raises no error toast.
- [ ] No response body rendered by either section contains a token, a password or a channel secret.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make typecheck && make test-web && echo ACCOUNT_NOTIFY_OK
```
Expected: Vitest lists `src/components/Settings/AccountSection.test.tsx` among the passed files, each of the
six tests named above appears with a `✓`, no file reports a failure, and the final line of stdout is exactly
`ACCOUNT_NOTIFY_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the five paths in the Files table and nothing else. Use `git status`, not `git diff`: three
of these files are new and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT store, cache, log or re-display an API token after its reveal dialog closes. `GET /api-tokens`
  returns only the prefix, and there is no endpoint that returns a token again.
- Do NOT add an input for session lifetime, engine credentials or any other `infrastructure` variable of
  [`11-config-reference.md` §2](../11-config-reference.md#2-dltool_-variables-application); the environment
  wins at boot and no API can change it.
- Do NOT add a user list, a role selector, an invite or a delete-account control; there is exactly one
  account, per [`05-api-contract.md` §12](../05-api-contract.md#12-the-account-and-api-tokens).
- Do NOT parse, reformat or truncate the `Send test` body; a raw reply that is not raw is useless.
- Do NOT build the Downloads, BitTorrent, Bandwidth or Advanced sections; **T118**, **T119** and **T121** own
  them.
- Do NOT change `web/src/components/Settings/SettingsScreen.tsx` beyond `IMPLEMENTED` and the two new
  section branches; T116–T119 and T121 all edit that same file.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

# T018 — Map aria2 JSON-RPC status to the canonical task state

| Field | Value |
|---|---|
| **ID** | T018 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T016 |
| **Blocks** | T019 |
| **Parallel-safe** | yes — touches only `internal/engine/aria2/` |
| **Implements** | [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~320 LOC |

## Goal
`internal/engine/aria2/map.go` decodes an `aria2.tellStatus` result into `engine.TaskInfo` and an
`aria2.getFiles` result into `[]engine.FileEntry`, applying the state table and the `errorCode` table of
[`06-download-engines.md`](../06-download-engines.md) row for row. Parsing is pure: no network, no daemon.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §4.4 `tellStatus` keys dl-tool reads](../06-download-engines.md#44-tellstatus-keys-dl-tool-reads)
2. [`docs/06-download-engines.md` §4.6 State normalisation](../06-download-engines.md#46-state-normalisation--reproduce-exactly)
3. [`docs/06-download-engines.md` §4.7 `errorCode` mapping](../06-download-engines.md#47-errorcode-mapping)
4. [`docs/06-download-engines.md` §1.1 File priority vocabulary](../06-download-engines.md#11-file-priority-vocabulary)
5. [`docs/13-testing-and-verification.md` §5 Golden-file fixtures](../13-testing-and-verification.md#5-golden-file-fixtures)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/aria2/map.go` | create | Wire structs and the two normalisation tables. |
| `internal/engine/aria2/map_test.go` | create | Table tests for every state row and every error code row, plus the golden-file diff. |
| `internal/engine/aria2/testdata/aria2_tellstatus_1.37.0.json` | create | One recorded `aria2.tellStatus` response. |

No other file may be modified.

## Interface contract

```go
package aria2

// statusResult is one aria2.tellStatus result. Scalar values arrive as JSON strings, including the
// numeric ones, so every number is parsed. files, uris and followedBy are arrays; bitfield,
// followedBy, belongsTo, verifiedLength and verifyIntegrityPending are conditionally absent.
type statusResult struct {
	GID                    string      `json:"gid"`
	Status                 string      `json:"status"`
	TotalLength            string      `json:"totalLength"`
	CompletedLength        string      `json:"completedLength"`
	UploadLength           string      `json:"uploadLength"`
	DownloadSpeed          string      `json:"downloadSpeed"`
	UploadSpeed            string      `json:"uploadSpeed"`
	Dir                    string      `json:"dir"`
	Files                  []fileEntry `json:"files"`
	ErrorCode              *string     `json:"errorCode"`
	ErrorMessage           *string     `json:"errorMessage"`
	InfoHash               *string     `json:"infoHash"`
	NumSeeders             *string     `json:"numSeeders"`
	Seeder                 *string     `json:"seeder"`
	Connections            *string     `json:"connections"`
	FollowedBy             []string    `json:"followedBy"`
	VerifiedLength         *string     `json:"verifiedLength"`
	VerifyIntegrityPending *string     `json:"verifyIntegrityPending"`
}

// fileEntry is one aria2.getFiles element. Index starts at 1 in aria2 and is exposed 0-based.
type fileEntry struct {
	Index           string `json:"index"`
	Path            string `json:"path"`
	Length          string `json:"length"`
	CompletedLength string `json:"completedLength"`
	Selected        string `json:"selected"`
}

// toTaskInfo normalises one tellStatus result. It never returns an error: an unparsable numeric
// scalar becomes 0 and is logged at debug.
func toTaskInfo(r statusResult) engine.TaskInfo

// toState applies the table of docs/06-download-engines.md §4.6. The checking row is evaluated
// first because it is a key-presence test, not a status value. An unknown status returns
// engine.StateQueued and logs one warning.
func toState(r statusResult) engine.TaskState

// toErrorCode maps an aria2 errorCode string to a tasks.error_code value. "0" and "" return "".
func toErrorCode(code string) string

// toFileEntries converts getFiles elements, subtracting one from every aria2 index and leaving
// Priority nil, because aria2 has no numeric per-file priority.
func toFileEntries(fs []fileEntry) []engine.FileEntry
```

State table, reproduced exactly and evaluated top to bottom:

| Condition | `engine.TaskState` |
|---|---|
| `verifyIntegrityPending` present **or** `verifiedLength` present | `checking` |
| `status == "active"` and `seeder == "true"` | `seeding` |
| `status == "active"` | `downloading` |
| `status == "waiting"` | `queued` |
| `status == "paused"` | `paused` |
| `status == "complete"` | `completed` |
| `status == "error"` | `error` |
| `status == "removed"` | `removed` |
| anything else | `queued`, with one warning log |

Error-code table, reproduced exactly: `3`, `4`, `6` → `broken_link`; `2`, `5` → `timeout`; `8` →
`not_supported_type`; `9` → `disk_full`; `11`, `12` → `torrent_duplicate`; `1`, `7`, `10` and every code
from `13` upward → `unknown`.

## Steps
1. Create `internal/engine/aria2/map.go` with the two wire structs above and no exported symbol beyond
   what a sibling file in the same package needs.
2. Implement a `parseInt64(string) int64` helper and use it for every numeric scalar; treat an absent
   pointer as unknown rather than as zero for `TotalBytes`.
3. Implement `toState` with the `checking` row first, then the `seeder` branch, then the plain `status`
   switch, and the `queued` fallback with a `slog.Warn` carrying the `engine` attribute.
4. Implement `toErrorCode` with the table above; return `""` for `"0"` and for the empty string.
5. Implement `toTaskInfo`, filling `ID` as `"aria2:" + gid`, `Engine` as `engine.NameAria2`, `SaveDir` from
   `dir`, `ContentPath` from the first selected `files[].path`, `InfohashV1` from `infoHash` lowercased,
   `NumSeeds` from `numSeeders` and `NumPeers` from `connections`.
6. Leave `InfohashV2` empty always, and add the comment that aria2 has no BEP 52 support.
7. Implement `toFileEntries` with `Index = aria2 index - 1`, `Selected` from the `"true"` string and
   `Priority = nil`.
8. Record `internal/engine/aria2/testdata/aria2_tellstatus_1.37.0.json` from a live aria2 1.37.0 and note
   the capture command and date in `internal/engine/aria2/testdata/README.md` — that README is created by
   this task inside the `testdata` directory row of the Files table.
9. Create `internal/engine/aria2/map_test.go` with one table row per state row and one per error-code row.
10. Add a golden-file test that decodes the fixture, runs `toTaskInfo` and compares with
    `github.com/google/go-cmp/cmp` behind a `-update` flag.

## Acceptance criteria
- [ ] Every row of the state table and of the error-code table has its own test case.
- [ ] A result carrying `verifyIntegrityPending` maps to `checking` even when `status` is `active`.
- [ ] `status: "active"` with `seeder: "true"` maps to `seeding`.
- [ ] An unknown `status` maps to `queued` and emits exactly one warning.
- [ ] `toFileEntries` reports index `0` for aria2 index `1` and `Priority == nil` for every file.
- [ ] The golden comparison passes without `-update`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/aria2/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/engine/aria2` followed
by its elapsed time, with `TestToState`, `TestToErrorCode` and `TestToTaskInfoGolden` all running. No
`FAIL`, no `[no test files]`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, plus `internal/engine/aria2/testdata/README.md`.
Use `git status`, not `git diff`: a file this task creates is untracked, and `git diff --name-only`
never lists an untracked file.

## Out of scope — do NOT
- Do NOT open a socket or start a daemon; T019 owns the JSON-RPC client.
- Do NOT map qBittorrent states; T029 owns `internal/engine/qbittorrent/map.go`.
- Do NOT add the shared adapter contract suite; T028 owns `internal/engine/enginetest/contract.go`.
- Do NOT write to `tasks`; T017 owns the store.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

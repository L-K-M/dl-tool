# T034 — List, add and remove a task's trackers

| Field | Value |
|---|---|
| **ID** | T034 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T021, T029 |
| **Blocks** | T035 |
| **Parallel-safe** | no — extends `internal/store/tasks.go` and `internal/api/server.go` |
| **Implements** | the tracker half of [FR-018](../02-requirements.md#fr-018-manage-trackers-and-list-peers-for-bittorrent-tasks) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~340 LOC |

## Goal
`GET /api/v1/tasks/{id}/trackers` returns a BitTorrent task's trackers with the engine's own status
string, `POST` adds URLs and `DELETE` removes one. Pseudo-trackers such as DHT, PeX and LSD appear as rows
and cannot be removed.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.9 Trackers and peers](../05-api-contract.md#59-trackers-and-peers)
2. [`docs/06-download-engines.md` §5.7 Files, priorities, trackers, peers, lifecycle](../06-download-engines.md#57-files-priorities-trackers-peers-lifecycle)
3. [`docs/04-data-model.md` §3.3 Tasks](../04-data-model.md#33-tasks)
4. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
5. [`docs/12-security-and-threat-model.md` §2.1 The block list](../12-security-and-threat-model.md#21-the-block-list)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/swarm.go` | create | `Trackers`, `AddTrackers`, `RemoveTrackers`. |
| `internal/api/tasks_swarm.go` | create | The three tracker handlers and the DTO. |
| `internal/api/tasks_swarm_test.go` | create | Cases for list, add, remove and every rejection. |
| `internal/store/tasks.go` | modify | Add `ReplaceTrackers` over `task_trackers`. |
| `internal/api/server.go` | modify | Register `list-task-trackers`, `add-task-trackers`, `remove-task-tracker`. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// TrackerEntry is one row of GET /api/v2/torrents/trackers, normalised onto the wire shape of 05 §5.9.
// Status is the engine's own value rendered as a string and stored verbatim; Seeds and Peers are nil
// when the engine does not report them for that row, which is the case for the DHT, PeX and LSD rows.
type TrackerEntry struct {
	URL                string
	Status             string
	Seeds              *int
	Peers              *int
	Message            string
	UpdateTimerSeconds *int
}

// Trackers lists the trackers of one torrent. id is the engine-namespaced task id.
func (c *Client) Trackers(ctx context.Context, id string) ([]TrackerEntry, error)

// AddTrackers posts torrents/addTrackers with hash and a newline-separated urls field.
func (c *Client) AddTrackers(ctx context.Context, id string, urls []string) error

// RemoveTrackers posts torrents/removeTrackers with hash and a pipe-separated urls field. Removing a
// pseudo-tracker row is refused by the engine and surfaces as ErrNotSupported.
func (c *Client) RemoveTrackers(ctx context.Context, id string, urls []string) error
```

<!-- UNVERIFIED: the per-tracker JSON key names and the JSON type of `status` in the
     GET /api/v2/torrents/trackers response were not read verbatim from release-5.2.3. Capture one real
     response from a live daemon and derive the struct tags from it before writing them; do not guess. -->

```go
package api

// trackerEngine is implemented by an engine that exposes a BitTorrent swarm. Declaring it here, at the
// consumer, keeps the Engine interface of 06 §1 unchanged.
type trackerEngine interface {
	Trackers(ctx context.Context, id string) ([]qbittorrent.TrackerEntry, error)
	AddTrackers(ctx context.Context, id string, urls []string) error
	RemoveTrackers(ctx context.Context, id string, urls []string) error
}

type TrackerDTO struct {
	URL                string  `json:"url"`
	Status             string  `json:"status"`
	Seeds              *int    `json:"seeds"`
	Peers              *int    `json:"peers"`
	Message            string  `json:"message"`
	UpdateTimerSeconds *int    `json:"update_timer_seconds"`
}

type ListTaskTrackersOutput struct {
	Body struct {
		Trackers []TrackerDTO `json:"trackers"`
	}
}

type AddTaskTrackersInput struct {
	ID   string `path:"id"`
	Body struct {
		URLs []string `json:"urls" minItems:"1" maxItems:"100"`
	}
}

type RemoveTaskTrackerInput struct {
	ID  string   `path:"id"`
	URL []string `query:"url"` // repeatable
}
```

## Steps
1. Capture the tracker response from a live 5.2.3 daemon with
   `curl -s "$QBT/api/v2/torrents/trackers?hash=<hash>"`, read the real key names from it, and only then
   write the struct tags in `internal/engine/qbittorrent/swarm.go`. Paste the command and the redacted
   response under `## Evidence`; do not commit it as a fixture.
2. Create `swarm.go` with `TrackerEntry`, `Trackers`, `AddTrackers` and `RemoveTrackers`, calling
   `GET torrents/trackers?hash=`, `POST torrents/addTrackers` and `POST torrents/removeTrackers`.
3. Render `Status` as a string whatever the engine's JSON type is, and leave `Seeds`, `Peers` and
   `UpdateTimerSeconds` nil where the engine reports no value.
4. Detect the pseudo-tracker rows by their bracketed pseudo-URL form and return
   `engine.ErrNotSupported` from `RemoveTrackers` when one is targeted, without issuing the request.
5. Add `ReplaceTrackers(ctx, taskID string, rows []Tracker) error` to `internal/store/tasks.go`: one
   `sqlx.Tx` that deletes the task's rows and re-inserts the listing, keyed on
   `idx_task_trackers_url`.
6. Create `internal/api/tasks_swarm.go` with the three handlers. `GET` reads the engine, calls
   `ReplaceTrackers`, and answers from the engine listing.
7. Validate every added URL: `http`, `https`, `udp` or `ws`/`wss` scheme only, and run each through the
   SSRF guard so a tracker URL cannot address a private host
   ([`12-security-and-threat-model.md`](../12-security-and-threat-model.md)).
8. Map the statuses of 05 §5.9: `200` on `GET`, `201` with the updated list on `POST`, `204` on `DELETE`,
   `404` for an unknown or foreign task, `422` when the task is not a BitTorrent task, `403`
   `/problems/ssrf-blocked` for a blocked tracker host, `503` when the engine is down.
9. Register the three operations in `internal/api/server.go`.
10. Create `internal/api/tasks_swarm_test.go` covering: a two-row listing including a pseudo-tracker with
    null seeds; adding one URL and seeing it in the returned list; removing it; removing a pseudo-tracker
    returning `422`; an `ftp://` tracker URL returning `422`; and an aria2 task returning `422`.

## Acceptance criteria
- [ ] The struct tags in `swarm.go` were derived from the captured response pasted under Evidence, not from the wiki.
- [ ] A pseudo-tracker row is listed with `seeds: null` and cannot be removed.
- [ ] `POST` returns `201` with the full updated list.
- [ ] A tracker URL resolving to a private address is refused with `403` `/problems/ssrf-blocked`.
- [ ] `GET` on a task whose engine lacks the `bittorrent` capability returns `422`.
- [ ] `task_trackers` holds exactly the rows of the last successful listing, with no duplicates.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/engine/qbittorrent`, `.../internal/store` and `.../internal/api`, with
`TestTrackersListPseudoRow`, `TestAddTrackerReturns201`, `TestRemovePseudoTrackerRejected`,
`TestTrackerURLSSRFBlocked` and `TestTrackersOnNonBitTorrentTask` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement peers; T035 adds `sync/torrentPeers` to the same files.
- Do NOT call `torrents/reannounce`, `addPeers`, `editTracker` or any web-seed endpoint.
- Do NOT edit a tracker URL in place; v1 adds and removes only.
- Do NOT expose tracker data for a non-BitTorrent task, not even as an empty list.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

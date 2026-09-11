# T034 — List, add and remove a task's trackers

| Field | Value |
|---|---|
| **ID** | T034 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T021, T029 |
| **Blocks** | T035, T048 |
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
- [x] The struct tags in `swarm.go` were derived from the captured response pasted under Evidence, not from the wiki.
- [x] A pseudo-tracker row is listed with `seeds: null` and cannot be removed.
- [x] `POST` returns `201` with the full updated list.
- [x] A tracker URL resolving to a private address is refused with `403` `/problems/ssrf-blocked`.
- [x] `GET` on a task whose engine lacks the `bittorrent` capability returns `422`.
- [x] `task_trackers` holds exactly the rows of the last successful listing, with no duplicates.

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

### The 5.2.3 capture (step 1)

No Docker daemon exists in this environment, so the live daemon is the official-source static build of the
pinned release — `x86_64-qbittorrent-nox` from `userdocs/qbittorrent-nox-static` tag `release-5.2.3_v2.0.14`,
which builds qBittorrent `release-5.2.3` verbatim — run locally with `--webui-port=8080`, a localhost auth
bypass in `qBittorrent.conf`, and a locally generated one-tracker .torrent
(`udp://tracker.example.org:6969/announce`, infohash `70bd59dc7fc9a42d12074673e54701f2e8867d10`) added
stopped. Nothing was committed as a fixture; the commands and responses follow, session cookie redacted.

```
$ curl -s http://127.0.0.1:8080/api/v2/app/version
v5.2.3

$ curl -s -F "torrents=@fixture.torrent" -F "stopped=true" \
    http://127.0.0.1:8080/api/v2/torrents/add
{"added_torrent_ids":["70bd59dc7fc9a42d12074673e54701f2e8867d10"],"failure_count":0,"pending_count":0,"success_count":1}

$ curl -s "http://127.0.0.1:8080/api/v2/torrents/trackers?hash=70bd59dc7fc9a42d12074673e54701f2e8867d10"
[{"msg":"","num_downloaded":0,"num_leeches":0,"num_peers":0,"num_seeds":0,"status":2,"tier":-1,"url":"** [DHT] **"},
 {"msg":"","num_downloaded":0,"num_leeches":0,"num_peers":0,"num_seeds":0,"status":2,"tier":-1,"url":"** [PeX] **"},
 {"msg":"","num_downloaded":0,"num_leeches":0,"num_peers":0,"num_seeds":0,"status":2,"tier":-1,"url":"** [LSD] **"},
 {"endpoints":[],"min_announce":0,"msg":"","next_announce":0,"num_downloaded":-1,"num_leeches":-1,"num_peers":-1,"num_seeds":-1,"status":1,"tier":0,"updating":false,"url":"udp://tracker.example.org:6969/announce"}]

$ curl -s -d "hash=70bd…7d10" --data-urlencode "urls=http://tracker2.example.org/announce" \
    http://127.0.0.1:8080/api/v2/torrents/addTrackers -o /dev/null -w '%{http_code}\n'
204
(listing then carries the new url as {"endpoints":[],…,"num_downloaded":-1,…,"num_seeds":-1,"status":1,"tier":0,…,"url":"http://tracker2.example.org/announce"})

$ curl -s -d "hash=70bd…7d10" --data-urlencode "urls=** [DHT] **" \
    http://127.0.0.1:8080/api/v2/torrents/removeTrackers -o /dev/null -w '%{http_code}\n'
204        # but the DHT row stays in the next listing: the daemon does NOT refuse it, it no-ops.

$ curl -s -d "hash=70bd…7d10" --data-urlencode "urls=http://tracker2.example.org/announce" \
    http://127.0.0.1:8080/api/v2/torrents/removeTrackers -o /dev/null -w '%{http_code}\n'
204        # and the url is gone from the next listing.
```

Derived from the capture and cross-checked against `release-5.2.3`
`src/webui/api/torrentscontroller.cpp` (`getStickyTrackers`, `getTrackers`) and `src/base/bittorrent/`
(`trackerentry.cpp`, `trackerentrystatus.h`):

- Per-row keys: `url`, `tier`, `status`, `msg`, `num_peers`, `num_seeds`, `num_leeches`, `num_downloaded`,
  plus `updating`, `endpoints`, `next_announce` and `min_announce` on the real tracker rows only — the
  synthetic DHT/PeX/LSD rows carry none of the latter four. `swarm.go` decodes `url`, `status`, `msg`,
  `num_seeds`, `num_peers`; the rest are ignored by the decoder.
- `status` is a **JSON number** — `TrackerEndpointState` (1 not contacted, 2 working, 4 not working,
  5 tracker error, 6 unreachable; the synthetic rows also use 0 disabled) — rendered as its decimal
  string and stored verbatim, exactly what the interface contract's "rendered as a string whatever the
  engine's JSON type is" asked for.
- `-1` counts are the `TrackerEntryStatus` "unknown" defaults, not values: `Seeds`/`Peers` are nil for a
  real row that reports them, and for every synthetic row, whose numbers count peers this swarm
  discovered, not tracker counts.
- No field of the response is an update timer — `next_announce`/`min_announce` are epoch seconds — so
  `UpdateTimerSeconds` has no wire source and stays nil; the column is NULL.
- `addTrackers` `urls` is newline-separated (`parseTrackerEntries` splits on `\n`, an empty line bumps the
  tier); `removeTrackers` `urls` is pipe-separated and each element is percent-decoded by the daemon, so
  the adapter percent-encodes each url before joining.
- The daemon answers `204` to a pseudo-tracker removal and silently keeps the row, which is why step 4's
  local refusal exists: `RemoveTrackers` detects the bracketed form and returns `ErrNotSupported`
  without issuing the request.

### Verification block

`make lint`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
```

`make test PKG=./internal/...`:

```
ok  	github.com/L-K-M/dl-tool/internal/api	78.700s
ok  	github.com/L-K-M/dl-tool/internal/config	1.252s
ok  	github.com/L-K-M/dl-tool/internal/engine	23.236s
ok  	github.com/L-K-M/dl-tool/internal/engine/aria2	3.249s
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	5.298s
ok  	github.com/L-K-M/dl-tool/internal/fsx	1.023s
ok  	github.com/L-K-M/dl-tool/internal/jobs	4.921s
ok  	github.com/L-K-M/dl-tool/internal/obs	1.185s
ok  	github.com/L-K-M/dl-tool/internal/secure	4.167s
ok  	github.com/L-K-M/dl-tool/internal/store	71.977s
ok  	github.com/L-K-M/dl-tool/internal/sync	4.390s
ok  	github.com/L-K-M/dl-tool/internal/uri	1.065s
```

(All output above is the final tree, after the review-round-1 and round-2 fixes listed under Scope.)

(One environmental note from an earlier run: `internal/engine`'s `TestClaimTimeClearDeclinesTheGuardedClaim`
failed once with "temp filesystem has only 1865420800 free bytes; test needs 1992294400 of head-room" —
this sandbox's disk was full from the capture daemon's scratch directory. After removing the scratch the
package passes; no code change was involved. The output above is the clean re-run.)

The five named tests:

```
$ go test ./internal/engine/qbittorrent ./internal/store ./internal/api \
    -run 'TestTrackersListPseudoRow|TestAddTrackerReturns201|TestRemovePseudoTrackerRejected|TestTrackerURLSSRFBlocked|TestTrackersOnNonBitTorrentTask' -v -count=1
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.016s [no tests to run]
ok  	github.com/L-K-M/dl-tool/internal/store	0.010s [no tests to run]
=== RUN   TestTrackersListPseudoRow
--- PASS: TestTrackersListPseudoRow (0.13s)
=== RUN   TestAddTrackerReturns201
--- PASS: TestAddTrackerReturns201 (0.08s)
=== RUN   TestRemovePseudoTrackerRejected
--- PASS: TestRemovePseudoTrackerRejected (0.07s)
=== RUN   TestTrackerURLSSRFBlocked
--- PASS: TestTrackerURLSSRFBlocked (0.07s)
=== RUN   TestTrackersOnNonBitTorrentTask
--- PASS: TestTrackersOnNonBitTorrentTask (0.05s)
ok  	github.com/L-K-M/dl-tool/internal/api	0.428s
```

The named tests live in `internal/api/tasks_swarm_test.go`; the other two packages match none of the
`-run` names and print `[no tests to run]`, which is `ok`, as with T032.

Criterion-to-test mapping: pseudo row `seeds: null` → `TestTrackersListPseudoRow`;
cannot be removed → `TestRemovePseudoTrackerRejected` (422, and no `removeTrackers` request reaches the
fake daemon); `POST` 201 with the full list → `TestAddTrackerReturns201`; private address refused with
403 `/problems/ssrf-blocked` → `TestTrackerURLSSRFBlocked` (TEST-NET-1, RFC 1918, `::1` and an
IPv4-mapped link-local address); non-BitTorrent engine 422 → `TestTrackersOnNonBitTorrentTask`; store
holds exactly the last listing → `TestTrackersListPseudoRow` and `TestTrackersListingReplacesRows`, and
no duplicates → `TestReplaceTrackersDeduplicatesRows`.

### Scope check

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/server.go
internal/api/tasks_swarm.go
internal/api/tasks_swarm_test.go
internal/engine/qbittorrent/swarm.go
internal/store/tasks.go
web/src/api/schema.d.ts
```

Exactly the Files table plus the two generated standing exceptions of docs/13 §7.1 (`api/openapi.json`
and `web/src/api/schema.d.ts`, both `make gen` output). No new dependency, so `go.mod`/`go.sum` are
untouched.

### Review round 1 (PR #121)

Fixed in this PR's scope: `dnsErrorText` now unwraps `net.DNSError` so the warn line omits the queried
host (`TestDNSErrorTextOmitsTheHost`); the file header no longer overstates the SSRF guarantee —
hostname urls are checked best-effort at add time, connect-time enforcement is T123's; `AddTrackers`
refuses empty or line-break-bearing urls before the form is built (`TestAddTrackersRejectsEmbeddedLineBreaks`);
`swarmChangeProblem` no longer maps the remove path's pseudo-tracker detail onto a POST;
`trackerURLsBlocked` resolves each distinct host once; the DELETE url count is capped at 100 like the
add body (`TestRemoveTrackersCapsURLCount`), the `url` query parameter is `required` in the spec, and
the absent-`url` DELETE is tested; POST/DELETE descriptions now state the non-BitTorrent 422; the
wire-fake row splitter uses `encoding/json`; multi-url newline and pipe joins, and the removal hash,
are asserted (`TestAddTrackerReturns201`, `TestRemoveTrackerRoundTrip`).

Not adopted: renaming `remove-task-tracker` to the plural — the singular operation id is named
verbatim by this task's `## Steps`. The remaining findings target `openapi.json` shapes owned by
other tasks (`sort`, `Delta.tasks`, `CreateTasksBody`, `FileSelectionRequest`, `elapsed_ms`, `blob`,
multipart `payload`, `writeOnly`, trailing newline, bulk `delete_data`); touching them would widen
this PR beyond its Files table, so they are left for their owning tasks.

### Review round 2 (PR #121)

Fixed in scope: `dnsErrorText` degrades an empty `net.DNSError.Err` to a static, host-free reason
instead of an empty log field (extended `TestDNSErrorTextOmitsTheHost`).

Not adopted: every remaining round-2 finding targets one shared artifact of the generated spec —
huma renders every Go slice as `["array","null"]` and routes every error through the `default`
response — so `ActionsInputBody.ids`, `PatchTaskFilesInputBody.files`, `CreateTasksBody`, the `Delta`
map, the nullable collections, the explicit 422/403 response entries and the required `url`
parameter's nullable schema are repo-wide conventions owned by the generator and the tasks that
registered those operations (T020, T022, T025, T032). The runtime behaviour each finding worries
about is guarded and tested at the handler level everywhere it applies here (`{"urls":null}` and an
absent `url` DELETE are both 422, `TestTrackersSchemeRejections`), so the gap is contract typing, not
behaviour, and belongs to a task that owns the shared spec generation.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

# 02 — Requirements

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** every task T001–T111, T113 and T115

## Purpose

State every functional requirement (`FR-###`) and non-functional requirement (`NFR-###`) for dl-tool v1 in
EARS notation, each with a stable anchor, a verification statement and the task that covers it. It does not
specify HTTP shapes, DDL, env vars or UI layout — those live in their own documents.

## Scope of this document

- In scope: requirement text, priority, and the task ID expected to satisfy each requirement.
- Out of scope (lives instead in): HTTP request/response shapes → [`05-api-contract.md`](05-api-contract.md) ·
  tables and enums → [`04-data-model.md`](04-data-model.md) · the `Engine` interface →
  [`06-download-engines.md`](06-download-engines.md) · dlsearch YAML schema →
  [`07-search-and-indexers.md`](07-search-and-indexers.md) · RSS rule schema and algorithm →
  [`08-rss-automation.md`](08-rss-automation.md) · grid columns and dialog fields →
  [`09-web-ui-spec.md`](09-web-ui-spec.md) · env vars → [`11-config-reference.md`](11-config-reference.md) ·
  threat model → [`12-security-and-threat-model.md`](12-security-and-threat-model.md) · Definition of Done →
  [`13-testing-and-verification.md`](13-testing-and-verification.md).

## How to read a requirement

| Element | Rule |
|---|---|
| Sentence | One EARS template: `The …` (ubiquitous), `While …` (state), `When …` (event), `Where …` (optional feature), `If …, then …` (unwanted). The system is always written `dl-tool`. |
| `Verify` | The observable check that closes the requirement. |
| `Covered by` | The task ID expected to implement it. Reconciled by the task-writing phase. |
| `Priority` | `must` \| `should` \| `could`. |

**IMPORTANT** v1 ships every requirement marked `must`. A `should` or `could` requirement may be deferred
only by recording the deferral in [`docs/tasks/00-task-index.md`](tasks/00-task-index.md).

Vocabulary used below and owned by the brief: task states are
`queued downloading seeding paused checking extracting moving completed error removed`; error codes are
Download Station's 26 `error_detail` values plus `ssrf_blocked`, `path_rejected`, `quota_exceeded`,
`engine_unavailable`, `unsupported_scheme` and `concurrency_limit`; all rates and sizes are bytes or
bytes/second.

### Permanently unused requirement identifiers

Nothing below is renumbered when a requirement is withdrawn. These identifiers are retired and are never
reassigned:

| Identifier(s) | Was | Why it is unused |
|---|---|---|
| `FR-025` | Bulk-import `.torrent` files and URL lists from a server directory | Withdrawn with the migration subsystem. Uploading `.torrent` and `.txt` files ([FR-005](#fr-005-add-tasks-from-an-uploaded-file)) and the watch folder ([FR-043](#fr-043-import-torrent-files-from-a-watch-folder)) cover the user-facing need. |
| `FR-079` | Import qBittorrent auto-download rules from `rules.json` | Withdrawn with the migration subsystem. |
| `FR-130` – `FR-139` | Compatibility façades | Withdrawn with the façades; dl-tool serves `/api/v1` only. |
| `FR-149` | One-time read-only import from a live Download Station | Withdrawn with the migration subsystem. dl-tool never speaks the Synology Web API. |

dl-tool imports no tasks, feeds or rules from another download product, and never calls another
product's API to fetch them. The one import that survives is the static, never-executed conversion of a
`.dlm` or nova3 `.py` search definition ([FR-053](#fr-053-import-legacy-definitions-by-static-analysis-only)),
which is a definition format, not a migration path. Task identifiers `T112` and `T114` are
retired for the same reason. Task identifier `T102` is retired too: a foreign-task *policy* cannot exist
when there is only one rule, so the ignore behaviour of FR-148 is asserted by T026 and T030 instead.

---

## Task lifecycle (FR-001 – FR-029)

### FR-001 Add tasks from a batch of pasted URIs
When a client submits a newline-separated list of URIs, dl-tool shall create one task per non-empty line and return the created tasks together with a per-line error for any line it rejected.

**Verify:** T020 integration test posts four lines (`https:`, `ftp:`, `magnet:`, garbage) and asserts three tasks created and one per-line rejection.

| Covered by | Priority |
|---|---|
| T020 | must |

### FR-002 Route each URI to an engine by scheme
The dl-tool router shall select the engine for a URI in this evaluation order: BitTorrent (`magnet:`, `.torrent` URL, raw `.torrent` bytes, bare infohash) to qBittorrent; a URI matched by a yt-dlp extractor to yt-dlp; `http` `https` `ftp` `ftps` `sftp` `.metalink` `.meta4` to aria2.

**Verify:** T016 table test drives the routing table in [`06-download-engines.md`](06-download-engines.md) and asserts the chosen engine for each row.

| Covered by | Priority |
|---|---|
| T016 | must |

### FR-003 Decode obfuscated Chinese download-manager schemes
When a submitted URI uses `thunder://`, `flashget://` or `qqdl://`, dl-tool shall Base64-decode the payload, strip the wrapper sentinel (`AA`/`ZZ`, `[FLASHGET]`/`[FLASHGET]`, none respectively) and re-route the recovered inner URI.

**Verify:** T015 unit test decodes `thunder://QUFodHRwOi8vd3d3LmV4YW1wbGUuY29tL2ZpbGUuemlwWlo=` to `http://www.example.com/file.zip` and tolerates missing `=` padding, a trailing `/` and the URL-safe alphabet.

| Covered by | Priority |
|---|---|
| T015 | must |

### FR-004 Reject ed2k links with a clear message
If a submitted URI uses the `ed2k://` scheme, then dl-tool shall reject it with error code `unsupported_scheme` and the message `ed2k is not supported in v1`, and shall not create a task.

**Verify:** T015 unit test asserts the error code and message; T020 asserts HTTP 422 with that code in the RFC 9457 body.

| Covered by | Priority |
|---|---|
| T015 | must |

### FR-005 Add tasks from an uploaded file
When a client uploads a `.torrent` file, dl-tool shall create a BitTorrent task from its bytes; when a client uploads a `.txt` file, dl-tool shall treat each line as a URI and apply FR-001.

**Verify:** T033 posts a fixture `.torrent` from `testdata/` and a two-line `.txt` as multipart parts, asserting one BitTorrent task and two URI tasks; T029 covers the engine half, handing the `.torrent` bytes to qBittorrent.

| Covered by | Priority |
|---|---|
| T033, T029 | must |

### FR-006 Inspect a submission before committing it
When a client submits URIs or a blob to the inspect endpoint, dl-tool shall return a manifest containing `name`, `total_size` and a `files[]` array of `{index, path, size}` without creating any task or writing to disk.

**Verify:** T031 posts a magnet and a `.torrent` blob, asserts the manifest file list matches the fixture and asserts `SELECT count(*) FROM tasks` is unchanged.

| Covered by | Priority |
|---|---|
| T031 | must |

### FR-007 Select and prioritise individual files
Where the target engine declares the `per_file_select` and `per_file_priority` capabilities, dl-tool shall accept a `files:[{index, selected, priority}]` list at creation and on an existing task, mapping priority to the qBittorrent vocabulary `skip=0`, `normal=1`, `high=6`, `maximum=7`, with no distinct `low` level.

**Verify:** T032 creates a multi-file torrent with file 1 deselected, then patches file 2 to `high`, and asserts the engine reports priorities `0` and `6`, and that a request carrying priority `4` is rejected.

| Covered by | Priority |
|---|---|
| T032 | must |

### FR-008 Save selected files in a subfolder named after the list
Where a client sets `create_subfolder` on a multi-file submission, dl-tool shall create the destination subfolder named after the manifest name and place the task content inside it.

**Verify:** T033 creates a task with `create_subfolder=true` and asserts the resolved content path is `<destination>/<manifest name>/`.

| Covered by | Priority |
|---|---|
| T033 | must |

### FR-009 Supply FTP credentials for a single task
Where a submitted URI uses `ftp://` or `ftps://`, dl-tool shall accept per-task `ftp_credentials` (username and password), pass them to aria2 for that task only, and never return the password in any response.

**Verify:** T020 creates an FTP task with credentials, asserts aria2 received `--ftp-user`/`--ftp-passwd` equivalents, and asserts the task JSON contains no password field.

| Covered by | Priority |
|---|---|
| T020 | must |

### FR-010 Recurse an FTP directory when the URI ends in a slash
When a submitted `ftp://` URI ends with `/`, dl-tool shall enumerate the directory and create one child transfer per file and sub-directory it contains.

**Verify:** T020 points at a fixture FTP server directory with two files and asserts two child transfers.

| Covered by | Priority |
|---|---|
| T020 | should |

### FR-011 Maintain the canonical task state machine
The dl-tool store shall hold every task in exactly one of the states `queued`, `downloading`, `seeding`, `paused`, `checking`, `extracting`, `moving`, `completed`, `error`, `removed`, and shall normalise every engine-reported state into that set.

**Verify:** T018 and T029 table tests reproduce the aria2 and qBittorrent normalisation tables from [`06-download-engines.md`](06-download-engines.md) row for row, including the unknown-state fallback to `queued` plus a warning log.

| Covered by | Priority |
|---|---|
| T018 | must |

### FR-012 List and filter tasks
The dl-tool task listing shall support the filters `state`, `category`, `tag`, `owner` and free-text `q`, with `sort`, `limit` and `cursor`, and shall impose no fixed upper bound on the number of stored tasks.

**Verify:** T021 seeds 5 000 tasks, asserts cursor pagination returns every row exactly once and that each filter returns the expected subset.

| Covered by | Priority |
|---|---|
| T021 | must |

### FR-013 Resolve the sidebar filter sets
The dl-tool task listing shall resolve the named filters as `Downloading` = `downloading`, `Completed` = `completed`, `Active` = `downloading ∪ seeding`, `Inactive` = `error ∪ queued ∪ paused`, `Stopped` = `paused`, and `Error` = `error`.

**Verify:** T021 seeds one task in each state and asserts the membership of all six named filters.

| Covered by | Priority |
|---|---|
| T021 | must |

### FR-014 Apply lifecycle and queue actions to a selection
When a client submits a task-action request with a list of task IDs and one of `pause`, `resume`, `remove`, `recheck`, `force_complete`, `queue_top`, `queue_up`, `queue_down` or `queue_bottom`, dl-tool shall apply it to every listed task and return a per-ID result.

**Verify:** T022 pauses three tasks in one call, one of which does not exist, and asserts two successes plus one `not_found` entry.

| Covered by | Priority |
|---|---|
| T022 | must |

### FR-015 Remove a task with or without its data
When a client deletes a task with `delete_data=true`, dl-tool shall remove the task from its engine and delete the downloaded data; with `delete_data=false` it shall leave the data in place.

**Verify:** T023 deletes one task each way and asserts the content path is absent in the first case and present in the second.

| Covered by | Priority |
|---|---|
| T023 | must |

### FR-016 Stream task changes as rid deltas over SSE
While a client holds the events stream open, dl-tool shall emit `event: sync` messages whose `id` is the monotonically increasing `rid` and whose data carries only the fields changed since the client's previous `rid`.

**Verify:** T025 opens the stream, mutates one task and asserts the next event contains that task ID and no unchanged task IDs; see [ADR-0006](decisions/0006-sse-with-rid-deltas.md).

| Covered by | Priority |
|---|---|
| T025 | must |

### FR-017 Serve the identical delta payload by polling
The dl-tool sync endpoint shall return, for a supplied `rid`, a payload byte-identical to the SSE `data` field for the same `rid`, so a client behind a proxy that buffers `text/event-stream` can poll instead.

**Verify:** T025 compares the SSE data field and the polling response for the same `rid` with `go-cmp` and asserts equality.

| Covered by | Priority |
|---|---|
| T025 | must |

### FR-018 Manage trackers and list peers for BitTorrent tasks
Where a task runs on an engine declaring the `bittorrent` capability, dl-tool shall list its trackers and peers, and shall add or remove trackers on request.

**Verify:** T034 adds a tracker URL, asserts it appears in the tracker list, removes it and asserts it is gone; T035 asserts the peer list shape.

| Covered by | Priority |
|---|---|
| T034 | must |

### FR-019 Offer sequential download and share limits
Where the target engine declares `sequential` and `share_limits`, dl-tool shall accept `sequential`, a share-ratio limit and a seeding-time limit per task, and shall stop seeding when **either** limit is reached.

**Verify:** T036 sets ratio 1.0 and seed time 10 minutes, and asserts the task stops on whichever fires first — deliberately OR, unlike Download Station's AND.

| Covered by | Priority |
|---|---|
| T036 | should |

### FR-020 Cap the number of concurrently active tasks
The dl-tool queue admission control shall start no more tasks than `max_active_total` overall and no more than `max_active_per_engine` on any one engine, holding the remainder in state `queued`.

**Verify:** T098 sets `max_active_total=2` and `max_active_per_engine=1`, submits six tasks split across aria2 and qBittorrent, and asserts exactly two are `downloading` with at most one per engine while the rest stay `queued`.

| Covered by | Priority |
|---|---|
| T098 | must |

### FR-021 Exclude seeding tasks from every concurrency limit
The dl-tool queue admission control shall exclude tasks in state `seeding` from `max_active_total`, `max_active_per_engine` and `max_active_per_user`, so a seeding torrent never blocks a queued download.

**Verify:** T098 fills `max_active_total` with tasks in state `seeding`, submits one download and asserts it starts immediately.

| Covered by | Priority |
|---|---|
| T098 | must |

### FR-022 Record both BitTorrent infohash forms
When dl-tool ingests a magnet link, a `.torrent` file or a bare infohash, it shall store `infohash_v1` as 40 lowercase hexadecimal characters and `infohash_v2` as 64 lowercase hexadecimal characters, decoding a 32-character base32 v1 magnet to hexadecimal first.

**Verify:** T100 ingests a v1 magnet, the same magnet in base32 form, a BEP 52 v2 magnet and a hybrid `.torrent`, and asserts the stored columns match the fixtures with the casing and lengths above.

| Covered by | Priority |
|---|---|
| T100 | must |

### FR-023 Reject a duplicate torrent by either infohash
If a submission's `infohash_v1` or `infohash_v2` equals that of an existing task whose state is not `removed`, then dl-tool shall reject the submission with error code `torrent_duplicate` and shall not create a second task.

**Verify:** T100 adds a hybrid torrent by its v1 magnet and then by its v2 magnet and asserts one task exists and the second call returned `torrent_duplicate`.

| Covered by | Priority |
|---|---|
| T100 | must |

### FR-024 Delete downloaded data safely and only on request
When a client deletes a task with `delete_data=true`, dl-tool shall stop the task at its engine first, unlink only the paths recorded in `task_files`, refuse any path that resolves outside the configured roots, and record the removal as a `task_events` row.

**Verify:** T111 deletes a seeding task with `delete_data=true` and asserts the engine was stopped before any unlink, that only `task_files` paths are gone, that a recorded path outside the roots is refused with `path_rejected`, and that a hardlinked copy elsewhere still opens with its original contents — removing dl-tool's link leaves the library copy intact.

| Covered by | Priority |
|---|---|
| T111 | must |

---

## Categories and tags (FR-030 – FR-039)

### FR-030 Manage categories with a save path
The dl-tool category API shall create, rename, delete and list categories, each carrying an optional save path used as the default destination for tasks in that category.

**Verify:** T050 creates category `linux` with save path `/data/linux`, creates a task in that category with no explicit destination, and asserts the destination resolved to `/data/linux`.

| Covered by | Priority |
|---|---|
| T050 | must |

### FR-031 Assign free-form tags and filter by them
The dl-tool task API shall accept a list of tags on creation and on update, and shall filter the task listing by any single tag.

**Verify:** T050 tags two tasks `iso`, asserts the tag filter returns exactly those two.

| Covered by | Priority |
|---|---|
| T050 | must |

### FR-032 Propagate category and tags to capable engines
Where the target engine declares the `categories` or `tags` capability, dl-tool shall mirror the task's category and tags into that engine so an operator inspecting the engine directly sees the same grouping.

**Verify:** T029 creates a categorised task, then reads `torrents/info` from qBittorrent and asserts the `category` field matches.

| Covered by | Priority |
|---|---|
| T029 | should |

### FR-033 List, rename and delete tags
The dl-tool tag API shall list every tag in use with the number of tasks carrying it, rename a tag across every one of those tasks, and delete a tag by detaching it from every task without deleting any task.

**Verify:** T107 tags three tasks, renames the tag and asserts all three carry the new name, then deletes it and asserts the three tasks still exist with no tags.

| Covered by | Priority |
|---|---|
| T107 | must |

---

## Destinations and the filesystem (FR-040 – FR-049)

### FR-040 Browse the server filesystem jailed to configured roots
The dl-tool filesystem API shall list the configured roots, and shall list only the directories beneath a requested path after resolving that path and confirming it lies inside one of those roots.

**Verify:** T046 asserts that `/data/movies` lists, that `/etc` returns 403 and that a symlink inside `/data` pointing at `/etc` is not traversed.

| Covered by | Priority |
|---|---|
| T046 | must |

### FR-041 Create a directory and report free space
The dl-tool filesystem API shall create a directory under a configured root on request, and shall report free and total bytes for any path inside a root.

**Verify:** T047 creates `/data/new`, asserts it exists with the process umask applied, and asserts free-space bytes are non-zero and are plain integers, not KB.

| Covered by | Priority |
|---|---|
| T047 | must |

### FR-042 Reject a destination outside the configured roots
If a task destination resolves outside every configured root, then dl-tool shall reject the request with error code `path_rejected` and shall not create the task.

**Verify:** T046 posts destinations `/etc`, `/data/../etc` and `/data/ok/../../etc` and asserts all three are rejected with `path_rejected`.

| Covered by | Priority |
|---|---|
| T046 | must |

### FR-043 Import torrent files from a watch folder
Where a watch directory is configured, dl-tool shall create a task for each `.torrent` file appearing in it, and shall delete the source file after a successful hand-off when the delete-after-load setting is enabled.

**Verify:** T083 drops a fixture `.torrent` into the watch directory and asserts a task appears within one poll interval and the file is removed.

| Covered by | Priority |
|---|---|
| T083 | must |

### FR-044 Report the effective destination
The dl-tool task record shall expose the destination the server actually resolved, distinct from the destination the client requested, so watch-folder and category defaults are visible rather than silently overridden.

**Verify:** T083 creates a watch-folder task while a category default applies and asserts `requested_destination != effective_destination` with both fields populated.

| Covered by | Priority |
|---|---|
| T083 | must |

### FR-045 Pre-check free space and pause on exhaustion
If the free space at a task's destination falls below the task's remaining bytes, then dl-tool shall pause the task, set error code `disk_full` and emit a `disk space low` notification event.

**Verify:** T076 runs against a small tmpfs destination and asserts the task moves to `paused` with `disk_full` instead of erroring repeatedly.

| Covered by | Priority |
|---|---|
| T076 | should |

### FR-046 Manage watch folders and scan one on demand
The dl-tool watch-folder API shall create, update, delete and list watch folders — each with a directory, an enabled flag, a destination and an optional category — and shall scan a single watch folder immediately on request.

**Verify:** T107 creates a watch folder, drops a fixture `.torrent` into it, triggers a scan and asserts the task appears without waiting for the poll interval; a watch folder outside every configured root is rejected with `path_rejected`.

| Covered by | Priority |
|---|---|
| T107 | must |

### FR-047 Reserve committed-but-unwritten bytes and keep a free-space floor
Before starting a task, dl-tool shall require the destination filesystem to hold that task's remaining bytes plus the sum of `total_bytes - completed_bytes` over every active task sharing the same filesystem plus that root's `min_free_space`, and shall otherwise hold the task in state `queued`.

**Verify:** T099 runs two tasks whose remaining bytes already commit a small tmpfs root, submits a third and asserts it stays `queued` instead of starting and failing, and asserts the default floor is `2147483648` bytes per [`11-config-reference.md`](11-config-reference.md).

| Covered by | Priority |
|---|---|
| T099 | must |

### FR-048 Never destroy partial data when a filesystem fills
If a write fails with `ENOSPC`, then dl-tool shall pause the task with error code `disk_full`, shall leave every partially downloaded file on disk untouched, and shall resume the task once free space is above `min_free_space` again.

**Verify:** T099 fills the destination mid-download, asserts the task is `paused` with `disk_full` and the partial file is byte-for-byte unchanged, then frees space and asserts the task resumes and completes.

| Covered by | Priority |
|---|---|
| T099 | must |

---

## Search and indexers (FR-050 – FR-069)

### FR-050 Query Torznab and Newznab indexers
The dl-tool search client shall issue `t=caps` and `t=search` requests to a configured Torznab or Newznab base URL with an API key, and shall parse `torznab:attr` attributes into the normalised result model.

**Verify:** T054 replays the recorded Torznab response fixture in `testdata/` and asserts `size`, `seeders`, `leechers`, `infohash` and `publish date` are parsed.

| Covered by | Priority |
|---|---|
| T054 | must |

### FR-051 Load declarative dlsearch/v1 engines
The dl-tool search layer shall load `dlsearch/v1` YAML definitions of kind `torznab`, `rss`, `json` and `html`, validate them against the schema, and reject any definition carrying an unknown top-level key.

**Verify:** T056 loads the four bundled definitions and asserts a definition with an unknown key fails validation with a message naming the key.

| Covered by | Priority |
|---|---|
| T056 | must |

### FR-052 Ship four legitimate engines and no piracy indexers
The dl-tool image shall bundle exactly the engines `internet-archive`, `arch-linux`, `academic-torrents` and `linux-distributions`, all enabled by default, and shall contain no other indexer definition in any state.

**Verify:** T057 asserts `definitions/engines/` contains exactly those four files and that a repository-wide grep for the known piracy-tracker names returns nothing.

| Covered by | Priority |
|---|---|
| T057 | must |

### FR-053 Import legacy definitions by static analysis only
Where a user uploads a Synology `.dlm` archive or a qBittorrent nova3 `.py` plugin, dl-tool shall extract and statically analyse it into a `dlsearch/v1` draft, store the original as an inert blob, create the engine **disabled** with its provenance recorded, and shall never execute the uploaded code.

**Verify:** T059 imports the `jackett.dlm` fixture, asserts the resulting engine row is `enabled=0` with `provenance='imported:dlm'`, and asserts no PHP or Python interpreter is invoked; see [ADR-0010](decisions/0010-never-execute-third-party-definitions.md).

| Covered by | Priority |
|---|---|
| T059 | must |

### FR-054 Run a search as an asynchronous job
When a client starts a search, dl-tool shall return a search job ID immediately, shall report `{finished, total, results[]}` on each poll while indexers are still answering, and shall discard the job's results when the client deletes it.

**Verify:** T061 starts a search against two stub indexers with different latencies, asserts the first poll returns `finished=false` with partial results, a later poll returns `finished=true`, and the delete removes the job.

| Covered by | Priority |
|---|---|
| T061 | must |

### FR-055 Surface per-engine status and errors
While a search job is running, dl-tool shall report, per indexer, one of `queued`, `searching`, `done` with a result count, or `error` with the upstream HTTP status and message, and shall never present an all-failed search as an empty result set.

**Verify:** T062 runs a search where one stub indexer returns HTTP 503 and asserts the job payload carries that indexer's `error` state and message alongside the other indexer's results.

| Covered by | Priority |
|---|---|
| T062 | must |

### FR-056 Test an indexer on demand
When an operator tests an indexer, dl-tool shall perform one `t=caps` or definition-equivalent request and return the outcome with the elapsed time and any upstream error text.

**Verify:** T058 tests a reachable and an unreachable indexer and asserts a success payload and an error payload carrying the transport error.

| Covered by | Priority |
|---|---|
| T058 | must |

### FR-057 Save and re-run a search
The dl-tool search API shall persist a named search consisting of a query, an indexer selection and a category selection, and shall re-run it on request.

**Verify:** T064 saves a search, re-runs it and asserts the same indexer selection is used.

| Covered by | Priority |
|---|---|
| T064 | should |

### FR-058 Create a task from a search result in one click
When a client adds a search result, dl-tool shall create a task from that result's download URI or magnet, applying the default destination unless the client supplied one.

**Verify:** T063 Playwright test clicks a result's download button and asserts a task row appears in the grid within five seconds.

| Covered by | Priority |
|---|---|
| T063 | must |

### FR-059 Report unknown result fields as null
The dl-tool result normaliser shall set `seeders`, `leechers`, `grabs` and `published_at` to null when the source does not supply them, and shall never substitute a fabricated value such as `seeders: 1`.

**Verify:** T054 parses the `academic-torrents` fixture, which carries no seeder data, and asserts `seeders` is null.

| Covered by | Priority |
|---|---|
| T054 | must |

---

## RSS automation (FR-070 – FR-089)

### FR-070 Manage feeds and refresh on demand
The dl-tool feed API shall create, update, delete and list feeds, list the items of a feed, and refresh a single feed immediately on request.

**Verify:** T065 adds the Arch Linux releases feed fixture, refreshes it and asserts items are stored with title, published date and download URI.

| Covered by | Priority |
|---|---|
| T065 | must |

### FR-071 Poll feeds politely with conditional GET and backoff
While a feed is enabled, dl-tool shall poll it on the configured interval sending `If-None-Match` and `If-Modified-Since`, shall treat HTTP 304 as "no new items", and shall apply an escalating backoff after consecutive failures.

**Verify:** T066 asserts the second poll of an unchanged fixture feed sends both conditional headers and stores no new items, and that three consecutive 500s lengthen the next scheduled poll.

| Covered by | Priority |
|---|---|
| T066 | must |

### FR-072 Extract a download URI from each item
The dl-tool feed parser shall take the item's download URI from, in order, a BitTorrent `<enclosure>`, a torznab `magneturl` or `infohash` attribute, and finally `<link>`, and shall discard an item that yields no usable URI.

**Verify:** T067 parses fixtures for all three shapes plus a web-page-only item and asserts three extractions and one discard.

| Covered by | Priority |
|---|---|
| T067 | must |

### FR-073 Evaluate rules with the documented algorithm
The dl-tool rule engine shall evaluate enabled rules ordered by `(priority ASC, name ASC)` through the fourteen steps documented in [`08-rss-automation.md`](08-rss-automation.md), rejecting on `none_of` before testing `any_of` and passing an item whose size is unknown rather than rejecting it.

**Verify:** T069 runs the algorithm fixture table and asserts the reason code produced for each step, including the unknown-size pass.

| Covered by | Priority |
|---|---|
| T069 | must |

### FR-074 Reject a malformed episode filter at save time
If a rule's episode filter does not match `^(\d{1,4})x(.*;)$`, then dl-tool shall reject the rule at save time with a validation error naming the field, rather than silently failing to match later.

**Verify:** T068 posts `1x01` (missing the trailing `;`) and asserts HTTP 422 with the field name `episode.filter`.

| Covered by | Priority |
|---|---|
| T068 | must |

### FR-075 Dry-run a rule and explain every item
When a client dry-runs a rule, dl-tool shall return **every** evaluated item — matched and unmatched — each carrying a machine-readable reason code from `excluded`, `no_match`, `size`, `episode_filter`, `unparseable_episode`, `duplicate_episode`, `duplicate_infohash`, `already_have`, `below_minimum_score`, `cooldown`, plus a `reason_detail` naming the pattern index responsible.

**Verify:** T070 dry-runs a rule over a 200-item fixture and asserts `len(results) == evaluated == 200` and that every unmatched entry carries a reason code from that closed set.

| Covered by | Priority |
|---|---|
| T070 | must |

### FR-076 Dry-run reproducibly by ignoring stored state
Where a dry-run request sets `ignore_state`, dl-tool shall ignore the grab and seen-episode tables so the same request returns the same result on every call.

**Verify:** T070 runs the same dry-run twice with `ignore_state=true` around a real grab and asserts both responses are equal under `go-cmp`.

| Covered by | Priority |
|---|---|
| T070 | must |

### FR-077 Run a rule against existing items
When an operator runs a saved rule against existing items, dl-tool shall evaluate the already-stored items of the rule's feeds and create tasks for the matches, reporting how many items were evaluated and how many were grabbed.

**Verify:** T071 stores twenty items, saves a rule matching three, runs it against existing items and asserts three tasks and a report of `evaluated=20, grabbed=3`.

| Covered by | Priority |
|---|---|
| T071 | must |

### FR-078 Route a rule's action to any engine
The dl-tool rule engine shall route each grabbed item through the same normalisation and routing path as a manually added URI, so a single rule can land HTTP, FTP and BitTorrent items.

**Verify:** T069 runs one rule over a feed mixing an `https://` enclosure and a magnet and asserts one aria2 task and one qBittorrent task; see [ADR-0009](decisions/0009-native-cross-protocol-rss-rules.md).

| Covered by | Priority |
|---|---|
| T069 | must |

---

## Bandwidth and scheduling (FR-090 – FR-099)

### FR-090 Enforce global rate limits in bytes per second
The dl-tool bandwidth governor shall accept a global download and upload limit expressed in bytes per second, where `0` means unlimited, and shall push that limit to every configured engine.

**Verify:** T079 sets 1 048 576 B/s and asserts both aria2 and qBittorrent report the equivalent limit; the API never accepts or returns KB/s.

| Covered by | Priority |
|---|---|
| T079 | must |

### FR-091 Apply alternative speeds to every engine
While the schedule's active cell is `alternative`, dl-tool shall apply the alternative download and upload limits to **all** engines and therefore to HTTP, FTP, SFTP, BitTorrent and media-site tasks alike.

**Verify:** T081 activates an alternative cell and asserts the aria2 global limit changed as well as the qBittorrent one — this is the deliberate fix for Download Station's BT-only alternative speed.

| Covered by | Priority |
|---|---|
| T081 | must |

### FR-092 Store and edit a 24×7 schedule grid
The dl-tool schedule shall be a 168-element array of `0` (no download), `1` (default speed) or `2` (alternative speed), indexed by day-of-week then hour, readable and writable as a whole.

**Verify:** T080 writes a grid with all three values present, reads it back and asserts the array is identical and rejects an array of any other length or with a value outside `0..2`.

| Covered by | Priority |
|---|---|
| T080 | must |

### FR-093 Apply the active schedule cell every minute
While the advanced schedule is enabled, dl-tool shall evaluate the active cell once per minute in the configured timezone and shall pause all tasks while that cell is `0`, resuming those it paused when the cell changes.

**Verify:** T081 fakes the clock across a `1 → 0 → 1` boundary and asserts the tasks it paused are the ones it resumes, leaving user-paused tasks untouched.

| Covered by | Priority |
|---|---|
| T081 | must |

### FR-094 Apply per-task limits to already-running tasks
When a client changes a task's download or upload limit, dl-tool shall apply the new limit to that task immediately, including while it is in state `downloading`.

**Verify:** T082 sets a limit on a running HTTP task and asserts the engine reports the new per-task limit without the task restarting — Download Station cannot do this and the test exists to prove dl-tool can.

| Covered by | Priority |
|---|---|
| T082 | must |

### FR-095 Order the queue by creation date or by owner
The dl-tool queue shall support a process order of `by_date_created` or `by_user_round_robin`, the latter starting at most one task per owner in round-robin before starting any owner's second task.

**Verify:** T085 queues three tasks for user A and one for user B under `by_user_round_robin` and asserts B's task starts before A's second.

| Covered by | Priority |
|---|---|
| T085 | should |

### FR-096 Combine schedule, global and per-task limits by minimum
The dl-tool bandwidth governor shall resolve a task's effective rate as the minimum of the active schedule cell's limit, the global limit and the task's own limit, and shall **pause** every task while the active cell is `0` rather than throttling it to a near-zero rate.

**Verify:** T110 sets a global limit of 10 485 760 B/s, a per-task limit of 1 048 576 B/s and an alternative-speed cell of 5 242 880 B/s and asserts the engine receives 1 048 576 B/s; it then sets the cell to `0` and asserts the task is `paused`. The precedence chain is stated once in [`06-download-engines.md`](06-download-engines.md).

| Covered by | Priority |
|---|---|
| T110 | must |

### FR-097 Evaluate the schedule in the container time zone
The dl-tool scheduler shall evaluate the 168-cell grid in the time zone given by the container's `TZ`, applying the repeated cell twice during a daylight-saving fall-back hour and not applying the absent cell during a spring-forward hour, and shall report the active time-zone name together with the grid.

**Verify:** T110 fakes the clock across both daylight-saving transitions in `Europe/Zurich`, asserts the cell is applied twice and then not at all, and asserts the schedule response carries the active time-zone name.

| Covered by | Priority |
|---|---|
| T110 | must |

---

## Post-processing (FR-100 – FR-114)

### FR-100 Auto-extract the supported archive formats
Where auto-extract is enabled, dl-tool shall extract completed archives of type `.zip`, `.tar`, `.gz`, `.tgz`, `.rar` and `.7z`, and shall keep the feature **off** by default.

**Verify:** T074 asserts the default setting is off, then enables it and asserts one archive of each of the six types extracts successfully.

| Covered by | Priority |
|---|---|
| T074 | must |

### FR-101 Try a shared password list and per-task passwords
When extracting an encrypted archive, dl-tool shall try, in order, the task's own extraction password and then each entry of the shared password list, and shall append a per-task password supplied at creation to that list.

**Verify:** T075 creates a task with password `secret`, asserts the archive extracts, asserts `secret` is now in the list, and asserts an archive with no matching password fails with `extract_failed_wrong_password`.

| Covered by | Priority |
|---|---|
| T075 | must |

### FR-102 Report extraction state, progress and failures
While an archive is being extracted, dl-tool shall hold the task in state `extracting` with a 0–100 progress value, and on failure shall set state `error` with one of `extract_failed`, `extract_failed_wrong_password`, `extract_failed_invalid_archive`, `extract_failed_quota_reached` or `extract_failed_disk_full`.

**Verify:** T074 asserts progress advances during extraction and that a truncated archive fixture yields `extract_failed_invalid_archive`.

| Covered by | Priority |
|---|---|
| T074 | must |

### FR-103 Move completed data across filesystems
When a completed task's data must be moved to a path on a different filesystem, dl-tool shall hold the task in state `moving`, copy then delete rather than failing on `EXDEV`, and shall verify the copy before deleting the source.

**Verify:** T076 moves data between two mounts in the test container and asserts the task passes through `moving`, that the destination bytes match and that the source is gone.

| Covered by | Priority |
|---|---|
| T076 | must |

### FR-104 Send notifications and offer a per-channel test
Where a notification channel is configured, dl-tool shall deliver an event to it and shall offer a send-test action that returns the raw upstream HTTP status and body.

**Verify:** T077 configures a webhook against a stub, fires a `task completed` event, asserts the stub received it, and asserts the send-test response contains the stub's status line.

| Covered by | Priority |
|---|---|
| T077 | must |

### FR-105 Run a completion hook only when explicitly enabled
Where a completion hook is enabled in the configuration file, dl-tool shall execute it as an argument vector with a fixed environment and a timeout, and shall not expose the hook command for editing through the HTTP API.

**Verify:** T078 asserts the hook is off by default, that a PATCH attempting to set the hook command returns 422 with the same body shape as any other unknown settings key — the response must not reveal that a hook key is special — and that the executed process receives argv entries rather than a shell string.

| Covered by | Priority |
|---|---|
| T078 | must |

### FR-106 Remove completed tasks automatically
Where auto-remove-on-complete is enabled, dl-tool shall remove a task's row once it reaches `completed` and any configured seeding limits are satisfied, leaving the downloaded data in place.

**Verify:** T074 enables the setting, completes a task and asserts the row is gone while the destination files remain.

| Covered by | Priority |
|---|---|
| T074 | could |

### FR-107 Manage notification channels
The dl-tool notification API shall create, update, delete and list channels of kind `webhook`, `ntfy`, `gotify` or `apprise`, each carrying a name, an enabled flag, a channel configuration, a stored secret that is never returned, and an event mask drawn from the `task_events` code vocabulary.

**Verify:** T106 creates one channel of each kind, asserts no read response contains the stored secret, sets an event mask holding `completed` only, and asserts an `error` event is not delivered to that channel while a `completed` event is.

| Covered by | Priority |
|---|---|
| T106 | must |

---

## Users, authentication and quotas (FR-115 – FR-129)

### FR-115 Complete a first-run setup using a one-time token
While no user exists, dl-tool shall refuse every API call except the setup endpoint, shall accept a one-time setup token of 256 bits (32 bytes) from a cryptographic random source, printed to stdout and written to `<config>/setup-token` with mode `0600`, shall regenerate the unused token on every boot, shall require the first admin password to be at least 12 characters, shall rate-limit setup attempts with the per-source-IP login throttle, and shall delete the token on success.

**Verify:** T009 boots an empty config, asserts every other endpoint returns 401, asserts the token file holds a different value after a restart while unused, drives nine failed setup attempts from one source and asserts the tenth returns `429` with `Retry-After`, advances past the bucket window, then completes setup with the token, asserts the token file is deleted and a second setup attempt returns 409.

| Covered by | Priority |
|---|---|
| T009 | must |

### FR-116 Authenticate with a session cookie or a bearer token
The dl-tool API shall accept either a server-side session cookie or an `Authorization: Bearer <api token>` header on every request, and shall additionally require the `X-DLTOOL-CSRF` header on mutating requests made with cookie authentication.

**Verify:** T008 asserts a cookie-authenticated POST without the CSRF header returns 403, the same POST with it returns 2xx, and a bearer-token POST without the header returns 2xx.

| Covered by | Priority |
|---|---|
| T008 | must |

### FR-117 Issue and revoke API tokens
The dl-tool token API shall create a token whose secret is returned exactly once at creation, shall list tokens by prefix and label only, and shall revoke a token so that its next use returns 401.

**Verify:** T084 creates a token, asserts the list response contains no secret, revokes it and asserts the next request with it returns 401.

| Covered by | Priority |
|---|---|
| T084 | must |

### FR-118 Restrict administrative endpoints to admins
If a non-admin calls a user-management, engine-configuration, indexer-management, settings-write or system-backup endpoint, then dl-tool shall return HTTP 403 and shall not perform the operation.

**Verify:** T084 drives each admin-only path with a non-admin session and asserts 403 for every one.

| Covered by | Priority |
|---|---|
| T084 | must |

### FR-119 Filter tasks by owner for non-admins
While a non-admin session is active, dl-tool shall return only the tasks owned by that user from every task read endpoint and shall reject actions on tasks owned by anyone else with HTTP 404.

**Verify:** T085 creates tasks for two users and asserts each non-admin sees only their own, an admin sees both, and a cross-owner pause returns 404 rather than 403.

| Covered by | Priority |
|---|---|
| T085 | must |

### FR-120 Apply a per-user default destination
Where a user has a default destination configured, dl-tool shall use it for that user's tasks when the request omits a destination, in preference to the global default.

**Verify:** T086 sets user B's default to `/data/b`, creates a task as B with no destination and asserts the effective destination is `/data/b`.

| Covered by | Priority |
|---|---|
| T086 | must |

### FR-121 Enforce the per-user storage quota
If creating a task would take the sum of `total_bytes` over a user's non-`removed` tasks above that user's `quota_bytes`, then dl-tool shall reject the request with error code `quota_exceeded` and shall not create the task.

**Verify:** T085 sets `quota_bytes` to 1 073 741 824, creates a 700 MiB task and asserts a second 700 MiB task is rejected with `quota_exceeded`, and asserts `quota_bytes = 0` means unlimited.

| Covered by | Priority |
|---|---|
| T085 | must |

### FR-122 Re-check the storage quota when metadata resolves
When a task's total size first becomes known after creation, dl-tool shall re-evaluate the owner's `quota_bytes` and, if it is now exceeded, shall pause the task with error code `quota_exceeded` and shall neither delete the task nor its data.

**Verify:** T085 adds a magnet with no size at creation for a user 100 MiB below their quota, resolves metadata to a 4 GiB torrent, and asserts the task is `paused` with `quota_exceeded` and still present after a restart.

| Covered by | Priority |
|---|---|
| T085 | must |

### FR-123 Enforce a per-user concurrency limit
The dl-tool queue admission control shall start no more than `max_active_per_user` tasks for any one user and shall report the reason for holding the remainder as `concurrency_limit`, which is distinct from `quota_exceeded`.

**Verify:** T098 sets `max_active_per_user=1`, queues three tasks for user A and one for user B, and asserts exactly one task per user is active and that every held task carries `concurrency_limit`.

| Covered by | Priority |
|---|---|
| T098 | must |

### FR-124 Jail non-admins to their own destination subtree
While a non-admin session is active, dl-tool shall confine filesystem browsing, free-space queries, directory creation and every task destination to the subtree of that user's `default_destination`, and shall grant admins every configured root.

**Verify:** T109 asserts a non-admin browsing a path outside their subtree receives 403, that a task destination outside it is rejected with `path_rejected`, and that no response lists another user's directory names.

| Covered by | Priority |
|---|---|
| T109 | must |

---

## Settings, backup and restore (FR-140 – FR-149)

### FR-140 Read and update settings without exposing secrets
The dl-tool settings API shall return every user-changeable setting and accept partial updates, and shall replace every secret value in its responses with a redaction marker.

**Verify:** T092 asserts the settings response contains no engine password, no indexer API key and no session key, while a subsequent update using the redaction marker leaves the stored secret unchanged.

| Covered by | Priority |
|---|---|
| T092 | must |

### FR-141 Resolve settings from environment then database
The dl-tool configuration loader shall take infrastructure settings (paths, listen addresses, engine URLs) from the environment at boot and user-preference settings (limits, schedule, poll intervals) from the database, with the environment value winning only for the former.

**Verify:** T005 sets an env override for a path setting and a DB value for a limit setting and asserts each source wins in its own category; the table of settings lives in [`11-config-reference.md`](11-config-reference.md).

| Covered by | Priority |
|---|---|
| T005 | must |

### FR-142 Produce consistent backups
When an operator requests a backup, dl-tool shall produce a consistent copy of the database using `VACUUM INTO`, and shall additionally write such a copy automatically before applying any schema migration.

**Verify:** T091 takes a backup while writes are in flight, opens the copy and asserts `PRAGMA integrity_check` returns `ok`; T006 asserts a pre-migration copy exists after an upgrade.

| Covered by | Priority |
|---|---|
| T091 | must |

### FR-143 List engines and test connectivity
The dl-tool engines endpoint shall list each configured engine with its declared capabilities and last known health, and shall run a connectivity test on request returning the engine's reported version or the transport error.

**Verify:** T027 asserts the aria2 entry reports `aria2.getVersion` output on success and `engine_unavailable` when the sidecar is stopped.

| Covered by | Priority |
|---|---|
| T027 | must |

### FR-144 Persist server-side UI preferences per user
The dl-tool preferences API shall read and replace a per-user preference document holding the task-grid column layout and the UI preferences listed in [`09-web-ui-spec.md`](09-web-ui-spec.md), so one account sees the same grid in every browser.

**Verify:** T107 stores a column order, width set and visibility set, reads it back from a second client authenticated as the same user and asserts equality under `go-cmp`, and asserts a second user's document is unaffected.

| Covered by | Priority |
|---|---|
| T107 | must |

### FR-145 Export and import portable settings
The dl-tool settings API shall export a versioned document containing settings, categories, indexers, feeds, rules, watch folders and the schedule, shall import such a document, and shall exclude sessions, password hashes and API tokens from the export.

**Verify:** T108 exports from a populated instance, greps the document for a session identifier, a password hash and a token prefix and asserts none appear, then imports into an empty instance and asserts the seven collections match the source.

| Covered by | Priority |
|---|---|
| T108 | must |

### FR-146 Restore a backup from the command line
When an operator runs `dl-tool restore --from <file>`, dl-tool shall refuse to proceed while a server process holds the database, shall refuse a backup whose schema version does not match the binary, and shall otherwise replace the database in place.

**Verify:** T108 runs the command against a running instance and asserts a named refusal, stamps a backup with a foreign schema version and asserts a second named refusal, then restores against a stopped instance and asserts the task count matches the source.

| Covered by | Priority |
|---|---|
| T108 | must |

### FR-147 Assert engine conformance at boot
When dl-tool connects to an engine, it shall assert that the engine's own competing automation is off — for qBittorrent `rss_processing_enabled=false`, `scheduler_enabled=false`, `auto_tmm_enabled=false` and no search plugins installed — shall raise a visible warning offering a one-click correction when it is not, and shall never exit because of it.

**Verify:** T101 starts qBittorrent with Automatic Torrent Management enabled, asserts dl-tool boots, asserts the warning names `auto_tmm_enabled`, applies the offered correction and asserts the setting is then false; see [ADR-0017](decisions/0017-exclusive-control-of-engines.md).

| Covered by | Priority |
|---|---|
| T101 | must |

### FR-148 Ignore engine tasks dl-tool did not create
If an engine holds a task dl-tool has no row for, then dl-tool shall ignore it: the task shall not appear in any listing, delta or count, and dl-tool shall neither modify nor delete it. There is no adopt mode and no configurable policy.

**Verify:** T026 asserts it for aria2 and T030 asserts it for qBittorrent: a torrent added directly in the engine never appears in `GET /tasks`, in an SSE delta or in the task counts, and its state at the engine is unchanged after a full poll cycle; see [ADR-0017](decisions/0017-exclusive-control-of-engines.md).

| Covered by | Priority |
|---|---|
| T026, T030 | must |

---

## Observability (FR-150 – FR-159)

### FR-150 Record a per-task event log
The dl-tool task event log shall record, per task, at least the events `created`, `started`, `metadata_received`, `checking`, `error`, `moving`, `extracting`, `completed` and `removed`, each with a timestamp, a level and a message.

**Verify:** T024 completes one task end to end and asserts the event list is non-empty, ordered and contains `created` and `completed`.

| Covered by | Priority |
|---|---|
| T024 | must |

### FR-151 Expose system logs with secrets redacted
The dl-tool system-log endpoint shall return recent structured log records to admins with `Authorization`, `Cookie`, `X-Api-Key`, `apikey`, `token` and `passkey` values replaced by a redaction marker.

**Verify:** T096 makes a request carrying an indexer URL with a `passkey=` query parameter and asserts the stored log line contains the marker and not the passkey.

| Covered by | Priority |
|---|---|
| T096 | must |

### FR-152 Expose health and readiness endpoints
The dl-tool process shall serve `GET /healthz` returning `{"status":"ok"}` as soon as the HTTP listener is up, and `GET /readyz` returning success only once the database is migrated and reachable, both without authentication and both outside `/api/v1`.

**Verify:** T010 asserts `/healthz` returns 200 during migration while `/readyz` returns 503, and both return 200 afterwards.

| Covered by | Priority |
|---|---|
| T010 | must |

### FR-153 Expose Prometheus metrics on a separate listener
The dl-tool process shall serve Prometheus metrics on the dedicated metrics address, bound to loopback by default, and shall not expose `/metrics` on the main HTTP listener.

**Verify:** T010 asserts `/metrics` on the main listener returns 404 and that the metrics listener returns a text exposition body containing the process and task-count metrics.

| Covered by | Priority |
|---|---|
| T010 | must |

---

## Non-functional requirements (NFR-001 – NFR-030)

### NFR-001 Render a 10 000-row grid smoothly
While 10 000 tasks are listed, the dl-tool web UI shall render with row virtualisation at a fixed row height and shall spend no more than 8 ms of scripting per one-second update tick on a mid-range laptop.

**Verify:** T043 Playwright performance test seeds 10 000 rows, records a trace over ten ticks and asserts the 95th-percentile scripting time per tick is under 8 ms.

| Covered by | Priority |
|---|---|
| T043 | must |

### NFR-002 Recover cleanly from a dropped event stream
If the event stream disconnects, then the dl-tool web UI shall reconnect with a 1, 2, 4, 8, 15, 30-second backoff, refetch full state on reconnect rather than replaying deltas, and disable mutating controls while disconnected.

**Verify:** T051 kills the stream mid-session and asserts the backoff sequence, the full refetch request and the disabled controls.

| Covered by | Priority |
|---|---|
| T051 | must |

### NFR-003 Resume every task after a restart
When dl-tool restarts, it shall reconcile its stored tasks against each engine by engine reference, re-attach the tasks the engine still holds, and mark tasks the engine has lost as `error` with `engine_unavailable` rather than losing them.

**Verify:** T026 restarts the container mid-download and asserts every task resumes to its previous state with progress preserved.

| Covered by | Priority |
|---|---|
| T026 | must |

### NFR-004 Shut down gracefully on SIGTERM
When dl-tool receives SIGTERM, it shall stop accepting new work, flush task state, run `PRAGMA wal_checkpoint(TRUNCATE)`, close the database and exit 0 within 20 seconds.

**Verify:** T124 sends SIGTERM under load and asserts exit code 0 within 20 seconds and that no `-wal` file remains.

| Covered by | Priority |
|---|---|
| T124 | must |

### NFR-005 Publish a multi-architecture image
The dl-tool release pipeline shall publish `linux/amd64` and `linux/arm64` images to the configured registry from a single tag, built with `CGO_ENABLED=0`.

**Verify:** T093 asserts `docker buildx imagetools inspect` lists both platforms for the release tag.

| Covered by | Priority |
|---|---|
| T093 | must |

### NFR-006 Work when hosted under a sub-path
Where a base path is configured, dl-tool shall serve every route, asset, event stream and redirect under that prefix, set the session cookie `Path` to it, and return 404 for requests outside it.

**Verify:** T095 runs the Playwright suite against dl-tool behind a reverse proxy at `/dl-tool/` and asserts login, the grid and the event stream all work.

| Covered by | Priority |
|---|---|
| T095 | must |

### NFR-007 Meet WCAG 2.2 AA with a keyboard-navigable grid
The dl-tool web UI shall expose the task list with `role="grid"`, `aria-rowcount` set to the **total** number of rows rather than the number currently in the DOM, `role="row"`/`role="gridcell"` descendants, one roving `tabindex="0"`, arrow-key navigation within the grid and Tab navigation out of it.

**Verify:** T104 runs `@axe-core/playwright` over the five main screens — Tasks, Search, RSS, Settings and the setup wizard — asserting zero serious or critical violations; one keyboard-map test walks the documented shortcuts and tab order with no pointer events; and one assertion compares `aria-rowcount` against the total task count while 10 000 tasks are loaded and fewer than 50 rows are rendered. See [`13-testing-and-verification.md`](13-testing-and-verification.md).

| Covered by | Priority |
|---|---|
| T104 | must |

### NFR-008 Ship translation plumbing with English only
The dl-tool web UI shall route every user-visible string through the translation layer and format all numbers, byte sizes and dates through the `Intl` APIs, shipping a complete `en` catalogue and no other complete locale in v1.

**Verify:** T052 asserts a lint rule finds no bare user-visible string literal in `web/src/` and that the `en` catalogue has no missing keys.

| Covered by | Priority |
|---|---|
| T052 | must |

### NFR-009 Collect and transmit no telemetry
The dl-tool binary and web UI shall make no outbound request that is not the direct result of a user action or a configured feed, indexer, engine or notification target, and shall contain no analytics SDK and no update check.

**Verify:** T094 runs the container with all outbound traffic captured, exercises the UI for five minutes and asserts zero unexpected destinations.

| Covered by | Priority |
|---|---|
| T094 | must |

### NFR-010 Always verify TLS certificates
The dl-tool HTTP client shall verify TLS certificates on every outbound request, and shall expose no configuration option, environment variable or API field that disables that verification.

**Verify:** T095 asserts a fetch from a host with a self-signed certificate fails, and asserts a repository grep finds no `InsecureSkipVerify: true` outside test fixtures — the lesson of CVE-2024-51774.

| Covered by | Priority |
|---|---|
| T095 | must |

### NFR-011 Ship no default credentials
The dl-tool distribution shall contain no built-in account, no default password and no anonymous mode, and shall accept no admin password from an environment variable in the shipped compose file.

**Verify:** T009 greps the image and compose file for any credential literal and asserts none, then asserts a fresh instance rejects every login until setup completes — the lesson of CVE-2023-30801.

| Covered by | Priority |
|---|---|
| T009 | must |

### NFR-012 Protect against CSRF with a synchroniser token
The dl-tool API shall enforce CSRF using a per-session synchroniser token supplied in the `X-DLTOOL-CSRF` header, and shall not rely on the `Referer` or `Origin` header as the sole defence.

**Verify:** T008 asserts a cross-site POST carrying a valid session cookie but no token is rejected, and asserts a forged `Referer` alone never grants access.

| Covered by | Priority |
|---|---|
| T008 | must |

### NFR-013 Reject unexpected Host headers
If an incoming request carries a `Host` header outside the configured allowlist, then dl-tool shall reject it with HTTP 421, with loopback names and literal IP addresses implicitly allowed.

**Verify:** T095 sends `Host: evil.example` and asserts 421, and sends `Host: localhost:8080` and asserts success — the lesson of CVE-2018-5702.

| Covered by | Priority |
|---|---|
| T095 | must |

### NFR-014 Never build a filesystem path from a request parameter
The dl-tool filesystem layer shall construct every path by sanitising each segment and joining it beneath a resolved configured root, and shall refuse absolute paths, `..` segments, symlinked components and paths that escape the root after resolution.

**Verify:** T046 runs the path-safety table from [`12-security-and-threat-model.md`](12-security-and-threat-model.md) and asserts every hostile row is rejected — the lesson of CVE-2017-9031.

| Covered by | Priority |
|---|---|
| T046 | must |

### NFR-015 Never interpolate configuration into a shell
The dl-tool process shall launch every subprocess with an explicit argument vector and shall never construct a shell command string from a configuration value, a task name, a path or any other user-controlled data.

**Verify:** T078 and T089 assert a repository grep finds no `sh -c` construction outside test fixtures, and that a task named `; rm -rf /` runs the extractor and yt-dlp harmlessly — the lesson of CVE-2020-13124.

| Covered by | Priority |
|---|---|
| T078 | must |

### NFR-016 Keep API tokens revocable and out of the logs
The dl-tool token store shall persist only a hash of each token, shall accept tokens solely in the `Authorization` header and never in a query string, and shall exclude token values from every log record and error message.

**Verify:** T084 asserts the database column holds no plaintext token, that a token in a query string is ignored, and that a request log line for an authenticated call contains the redaction marker.

| Covered by | Priority |
|---|---|
| T084 | must |

### NFR-017 Block server-side request forgery
The dl-tool outbound HTTP client shall validate the resolved peer address on every connection and every redirect, deny loopback, link-local, private, CGNAT and IPv6 unique-local targets unless explicitly allowed for that engine, allow only `http` and `https`, follow at most five redirects, and cap response size and total time.

**Verify:** T123 asserts fetches to `127.0.0.1`, `169.254.169.254` and a public host that redirects to `127.0.0.1` are all blocked with `ssrf_blocked`; T122 asserts the same for a URI submitted to `POST /tasks`.

| Covered by | Priority |
|---|---|
| T123, T122 | must |

### NFR-018 Extract archives safely
The dl-tool extractor shall extract into a fresh empty directory on the target filesystem, sanitise every member name, refuse symlinks, hardlinks, device nodes and setuid bits, extract exactly one level without recursing into nested archives, and abort when the total uncompressed size or member count exceeds its configured caps.

**Verify:** T074 runs a zip-slip fixture, a symlink fixture and a compression-bomb fixture and asserts all three abort with no file written outside the target directory.

| Covered by | Priority |
|---|---|
| T074 | must |

### NFR-019 Parse untrusted definitions and regexes defensively
The dl-tool definition loader shall reject any YAML document larger than 512 KiB before parsing, parse with a non-executing loader, validate against the JSON Schema, and run every definition-supplied regular expression under a size limit and a hard deadline.

**Verify:** T056 asserts an oversized document, a document containing a language-specific type tag and a catastrophically backtracking pattern are each rejected without hanging.

| Covered by | Priority |
|---|---|
| T056 | must |

### NFR-020 Execute no third-party code
The dl-tool runtime shall contain no scripting interpreter for user- or definition-supplied code, and shall treat every imported `.dlm`, `.py` or YAML definition as data only.

**Verify:** T059 and T060 assert the process never spawns a PHP or Python interpreter for a definition, and that the image contains no PHP runtime; see [ADR-0010](decisions/0010-never-execute-third-party-definitions.md).

| Covered by | Priority |
|---|---|
| T060 | must |

### NFR-021 Serve strict security headers
The dl-tool HTTP server shall send `Content-Security-Policy` with no `unsafe-inline`, no `unsafe-eval` and no external origin, plus `X-Content-Type-Options`, `Referrer-Policy`, `X-Frame-Options: DENY` and `Permissions-Policy`, and shall send `Strict-Transport-Security` only on requests received over HTTPS.

**Verify:** T095 asserts each header on an HTML response and asserts HSTS is absent over plain HTTP.

| Covered by | Priority |
|---|---|
| T095 | must |

### NFR-022 Load no third-party runtime assets
The dl-tool web UI shall load every script, stylesheet, font and icon from the dl-tool origin, so the interface works with no internet access and leaks nothing to a content delivery network.

**Verify:** T039 asserts the built `index.html` and its bundles reference no absolute external origin.

| Covered by | Priority |
|---|---|
| T039 | must |

### NFR-023 Generate secrets on first run and support file-based secrets
When dl-tool starts with no generated secrets, it shall create the session key, the CSRF key and the setup token from a cryptographic random source, write them with mode `0600`, and shall additionally accept any secret through a `_FILE`-suffixed variable pointing at a mounted file.

**Verify:** T005 asserts a fresh config directory yields distinct secrets on two separate instances and that a `_FILE` variable is honoured in preference to its inline form.

| Covered by | Priority |
|---|---|
| T005 | must |

### NFR-024 Validate login redirects as relative paths
If a login redirect parameter is not a relative path beginning with a single `/`, then dl-tool shall ignore it and redirect to the application root.

**Verify:** T095 asserts `//evil.example` and `https://evil.example` are both ignored — the lesson of CVE-2024-45247.

| Covered by | Priority |
|---|---|
| T095 | must |

### NFR-025 Run unprivileged with the operator's UID and GID
The dl-tool container entrypoint shall apply the configured UID, GID and umask, drop privileges before starting the binary, and shall run the application as a non-root user.

**Verify:** T124 starts the container with a non-default UID and asserts files written to the data mount carry that ownership and that the application process is not root.

| Covered by | Priority |
|---|---|
| T124 | must |

### NFR-026 Store data durably in one SQLite database
The dl-tool store shall use a single SQLite database in the configuration directory with WAL journalling and a busy timeout, shall refuse to start against a database whose schema version is newer than the binary, and shall require no other datastore.

**Verify:** T006 asserts WAL mode is active after boot and that a database stamped with a future schema version causes a refusal to start with a clear message; see [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md).

| Covered by | Priority |
|---|---|
| T006 | must |

### NFR-027 Keep the generated API contract in step with the code
The dl-tool build shall generate the OpenAPI document from the handler definitions and shall fail continuous integration when the committed document differs from the generated one.

**Verify:** T002 asserts the CI job regenerates the document and that a deliberate handler change without regeneration fails the job; see [ADR-0003](decisions/0003-chi-huma-code-first-openapi.md).

| Covered by | Priority |
|---|---|
| T002 | must |

### NFR-028 Harden the release supply chain
The dl-tool release pipeline shall pin base images by digest, pin third-party actions by commit SHA, sign published images with keyless signing and attach a software bill of materials.

**Verify:** T097 asserts the signature verifies with the documented command and that the SBOM attestation is present for the release tag.

| Covered by | Priority |
|---|---|
| T097 | should |

### NFR-029 Ship an installable progressive web app
The dl-tool web UI shall ship a web app manifest with maskable icons, `display: standalone` and a `theme-color` matching the dark theme, together with a service worker whose only jobs are meeting the install criterion and caching static assets; no dl-tool feature works offline.

**Verify:** T103 runs the Lighthouse installability check and asserts the manifest, the maskable icons, `display: standalone`, `theme-color` and a registered service worker, and asserts that cutting the network shows the reconnect banner rather than any offline mode.

| Covered by | Priority |
|---|---|
| T103 | must |

---

## Decisions referenced

| ADR | Decision |
|---|---|
| [0001](decisions/0001-control-plane-over-existing-engines.md) | Build a control plane over existing download engines |
| [0003](decisions/0003-chi-huma-code-first-openapi.md) | chi + Huma with code-first OpenAPI |
| [0004](decisions/0004-sqlite-as-the-only-datastore.md) | SQLite as the only datastore |
| [0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) | aria2, qBittorrent and yt-dlp as the v1 engines |
| [0006](decisions/0006-sse-with-rid-deltas.md) | Server-sent events with rid deltas for live updates |
| [0008](decisions/0008-torznab-first-declarative-yaml-second.md) | Torznab first, declarative YAML engines second |
| [0009](decisions/0009-native-cross-protocol-rss-rules.md) | A native cross-protocol RSS rule engine |
| [0010](decisions/0010-never-execute-third-party-definitions.md) | Never execute third-party definition code |
| [0011](decisions/0011-alpine-runtime-with-puid-pgid.md) | Alpine runtime image with PUID/PGID privilege drop |
| [0013](decisions/0013-mandatory-built-in-authentication.md) | Mandatory built-in authentication |
| [0015](decisions/0015-db-backed-in-process-job-queue.md) | DB-backed in-process job queue |
| [0017](decisions/0017-exclusive-control-of-engines.md) | dl-tool assumes exclusive control of its engines |

## Open questions

- FR-071: the RSS poll-interval option values are not fixed by prior art. Download Station's dropdown values
  are **UNVERIFIED**; the shipped set must be chosen in [`08-rss-automation.md`](08-rss-automation.md).
- FR-002: the cheap yt-dlp extractor pre-check is marked **INFERRED** in the research; the exact matching
  mechanism must be settled in [`06-download-engines.md`](06-download-engines.md).
- FR-050 and FR-055: the Torznab category tree used by the search category filter is **INFERRED** from
  convention; confirm the concrete category IDs in [`07-search-and-indexers.md`](07-search-and-indexers.md).
- (resolved 2026-09-01: the stale FR-121 anchor in [`05-api-contract.md`](05-api-contract.md) is gone; FR-121
  is the storage quota and FR-123 the concurrency limit, as written above.)
- This document exceeds the 700-line budget suggested for it because the mandated coverage list does not fit
  in fewer requirements at the required block format.

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Compatibility façades cut: FR-130 – FR-139 withdrawn and permanently unused, ADR-0014 link removed. Added FR-020 – FR-025, FR-033, FR-046 – FR-048, FR-096, FR-097, FR-107, FR-122 – FR-124, FR-144 – FR-149 and NFR-029. Corrected the FR-007 file-priority vocabulary, the FR-121 quota semantics and the NFR-007 acceptance mechanism; fixed the ADR-0003 and ADR-0010 slugs. |
| 2026-09-01 | Migration subsystem cut: FR-025, FR-079 and FR-149 deleted and their identifiers retired; added the permanently-unused identifier table. FR-148 rewritten as ignore-only — `engines.foreign_task_policy`, the adopt mode and the tasks T112/T114 are gone. Corrected the ADR-0017 filename. |
| 2026-09-01 | M2 task allocation: FR-148 is verified by T026 (aria2) and T030 (qBittorrent); task identifier T102 retired with the foreign-task policy. |
| 2026-09-01 | Consistency review: corrected the ADR-0001, ADR-0005, ADR-0006, ADR-0008, ADR-0009 and ADR-0011 links to the canonical filenames; narrowed "no import path from another product" so it no longer contradicts FR-053's static `.dlm`/nova3 definition conversion. FR-005 is now covered by T033 (the multipart upload path) with T029 as the engine half. |
<<<<<<< HEAD
| 2026-09-01 | FR-115 hardened: the setup token is at least 128 bits from a CSPRNG and setup attempts are rate-limited with the login throttles; the Verify line asserts the `429`. |
| 2026-09-01 | Review pass: FR-115 pins the token at 256 bits (matching §6.4 of the threat model), names the per-source-IP throttle specifically, and its Verify is passable in one run — nine failures, the tenth 429s, the window advances, setup then completes — plus the regenerate-on-restart assertion. |
| 2026-09-01 | Review pass 2: FR-115 gains the regenerate-on-every-boot shall-clause its Verify already asserted, closing the traceability gap. |
=======
| 2026-09-01 | Dropped the resolved FR-121-anchor open question; `05-api-contract.md` no longer carries the stale anchor. |
>>>>>>> origin/main

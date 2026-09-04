# 00 — Task index

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** any task file

## Purpose
Lists every task, its milestone, its dependencies and its status, so an agent can pick the next
unblocked task without reading all of them. It does not restate any task's contents.

## Scope of this document
- In scope: the task roster, milestone grouping, dependency edges, status, and the deferral register.
- Out of scope (lives instead in): each task's goal, files, steps and acceptance criteria → the task file
  itself; the Definition of Done → [`../13-testing-and-verification.md`](../13-testing-and-verification.md);
  requirement text → [`../02-requirements.md`](../02-requirements.md).

## How to use this file
1. Work milestones in order. **M0 is blocking** — nothing else starts until every M0 row is `done`.
2. Within a milestone, take the lowest-numbered row whose `Depends on` tasks are all `done`.
3. Rows marked `Parallel-safe: yes` in their own file may run concurrently with any other parallel-safe
   row in the same milestone.
4. Set the row's `Status` to `done` in the same commit as the work, per rule 9 of the Definition of Done.

`Status` is one of `todo`, `in-progress`, `done`, `deferred`.

## Permanently unused IDs
| ID | Why |
|---|---|
| T102 | Foreign-task policy — withdrawn; dl-tool ignores tasks it did not create ([ADR-0017](../decisions/0017-exclusive-control-of-engines.md)). |
| T112 | Façade authentication mapping — withdrawn with the compatibility façades. |
| T114 | Migration importers — withdrawn with the migration subsystem. |
| T085 | Ownership filtering and the storage quota — withdrawn with the multi-user model ([ADR-0019](../decisions/0019-single-account-no-ownership.md)). |
| T086 | User management and per-user default destinations — withdrawn with the multi-user model. |
| T109 | The per-user root jail — withdrawn with the multi-user model. The `DLTOOL_DATA_ROOTS` check it built on remains, in T046 and T047. |

IDs are never reused. `docs/15-*` and ADR 0014 carry the same permanent gap.

## M0 — Foundations

Repository, CI, configuration, the database and migrations, authentication, health, the SSE skeleton, the
runtime image and the compose stack.
Exit checkpoint: `cp .env.example .env && docker compose -f compose.yaml -f compose.dev.yaml up -d --build`
starts the stack; `curl -s localhost:8091/healthz` returns `{"status":"ok"}`; `curl -s localhost:8091/`
returns the embedded SPA shell; the `lint`, `test`, `gen-drift`, `compose` and `doclint` CI jobs are green.

T124 and T125 build `Dockerfile`, `.dockerignore`, `deploy/entrypoint.sh`, `compose.yaml`,
`compose.dev.yaml` and `.env.example` inside M0, so the brief's `docker compose up -d` checkpoint is
reachable here and the `compose` CI job has input files. Two clauses of the brief's wording still are not
reachable in M0: the rendered **login page** is T040, in M3 — T013 embeds only a placeholder `index.html` —
and the `integration` CI job has no engine adapter to exercise until M1/M2. Both stay in the deferral
register.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T001](T001-makefile-and-doclint.md) | Create the Makefile and the doc-lint script | — | done |
| [T002](T002-ci-workflows-and-openapi-gate.md) | Add the CI workflows and the OpenAPI generation script | T001 | done |
| [T003](T003-web-build-scaffold.md) | Scaffold the Vite SPA build and its toolchain | T001 | done |
| [T004](T004-go-module-and-entrypoint.md) | Bootstrap the Go module, pin every dependency and build the entrypoint | T001 | done |
| [T005](T005-config-loader-and-secrets.md) | Load the DLTOOL_ environment and generate the boot secrets | T004 | done |
| [T006](T006-sqlite-store-and-initial-migration.md) | Open the SQLite store, generate ULIDs and apply the initial migration | T004, T005 | done |
| [T007](T007-http-server-and-openapi.md) | Serve chi and Huma under the base path and commit the OpenAPI document | T005, T006 | done |
| [T008](T008-session-and-csrf-authentication.md) | Authenticate every request with a session cookie or a bearer token | T006, T007 | done |
| [T009](T009-first-run-setup-and-login.md) | Complete the first run with a one-time setup token | T008 | done |
| [T010](T010-health-readiness-and-metrics.md) | Serve health, readiness and Prometheus metrics | T006, T007 | done |
| [T011](T011-sse-hub-and-rid-ring.md) | Build the SSE hub and the rid ring buffer | T004 | done |
| [T012](T012-job-worker-pool.md) | Run the database-backed job worker pool | T006 | done |
| [T013](T013-embed-spa-and-base-path.md) | Embed the built SPA and serve it under the base path | T003, T007 | done |
| [T014](T014-typed-api-client.md) | Generate the typed API client from the committed OpenAPI document | T002, T007, T013 | done |
| [T124](T124-runtime-dockerfile-and-entrypoint.md) | Write the runtime Dockerfile and the entrypoint | T003, T004, T010, T013 | done |
| [T125](T125-compose-and-env-example.md) | Write `compose.yaml` and `.env.example` | T124 | done |

## M1 — Task core and aria2

URI normalisation, the Engine interface, the aria2 adapter, the queue and the event log. Exit checkpoint: A pasted HTTPS URL downloads to `/data`, progress streams over SSE, pause/resume/remove work.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T015](T015-normalise-submitted-uris.md) | Normalise and decode a submitted URI | T004 | done |
| [T016](T016-engine-interface-and-router.md) | Define the Engine interface and the protocol router | T004, T015 | done |
| [T017](T017-task-store-and-state-machine.md) | Persist tasks and enforce the task state machine | T006, T016 | done |
| [T018](T018-aria2-status-mapping.md) | Map aria2 JSON-RPC status to the canonical task state | T016 | done |
| [T019](T019-aria2-adapter.md) | Implement the aria2 adapter and the engine registry | T016, T018 | done |
| [T020](T020-create-tasks-endpoint.md) | Create tasks from submitted URIs | T007, T008, T015, T016, T017, T019 | done |
| [T021](T021-list-and-filter-tasks.md) | List, filter and sort tasks | T017, T020 | done |
| [T022](T022-task-actions-and-patch.md) | Update a task and apply bulk lifecycle actions | T019, T020, T021 | todo |
| [T023](T023-remove-task-and-data.md) | Remove a task with or without its data | T020, T021, T022 | todo |
| [T024](T024-task-event-log.md) | Record and serve the per-task event log | T017, T020, T023 | todo |
| [T025](T025-rid-deltas-over-sse.md) | Compute rid deltas and stream them over SSE | T011, T021, T022, T024 | todo |
| [T026](T026-boot-reconciliation.md) | Reconcile tasks with the engines at boot and on every poll | T019, T024, T025 | todo |
| [T027](T027-list-and-test-engines.md) | List engines and test connectivity | T019, T020 | todo |
| [T098](T098-concurrency-limiter.md) | Admit tasks under the concurrency limits | T017, T019, T020, T024, T026 | todo |
| [T099](T099-disk-space-reservation.md) | Reserve disk space and keep a free-space floor | T020, T024, T098 | todo |

## M2 — BitTorrent

The qBittorrent adapter, `.torrent`/magnet inspection, file selection, trackers and peers. Exit checkpoint: A magnet added with a file-selection step downloads and seeds; per-file priorities apply.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T028](T028-engine-contract-test-suite.md) | Add the shared engine contract test suite | T016, T019 | todo |
| [T029](T029-qbittorrent-session-and-add.md) | Implement the qBittorrent session, state mapping and `torrents/add` | T005, T016, T019, T027 | todo |
| [T030](T030-qbittorrent-maindata-deltas.md) | Track qBittorrent state through `sync/maindata` rid deltas | T026, T029 | todo |
| [T031](T031-metainfo-and-inspect-endpoint.md) | Parse torrent metainfo and serve `POST /tasks/inspect` | T015, T020 | todo |
| [T032](T032-file-selection-and-priorities.md) | Select and prioritise the files of a task | T021, T029, T030 | todo |
| [T033](T033-uploads-subfolder-and-selection.md) | Accept uploaded files, the subfolder option and a create-time file selection | T020, T031, T032 | todo |
| [T034](T034-task-trackers.md) | List, add and remove a task's trackers | T021, T029 | todo |
| [T035](T035-task-peers.md) | List a task's connected peers | T029, T030, T034 | todo |
| [T036](T036-share-limits-and-mutators.md) | Apply share limits, sequential download and the location mutators | T022, T029 | todo |
| [T037](T037-qbittorrent-rate-limits.md) | Apply per-task and global rate limits to running qBittorrent tasks | T022, T029, T030 | todo |
| [T038](T038-magnet-metadata-and-contract-call-site.md) | Resolve magnet metadata and complete the qBittorrent adapter | T028, T029, T030, T031, T032, T036, T037 | todo |
| [T100](T100-infohash-identity-and-duplicates.md) | Record both infohashes and reject duplicate torrents | T017, T020, T029, T030, T031 | todo |
| [T101](T101-engine-conformance-at-boot.md) | Assert and force engine conformance at boot | T019, T027, T028, T029 | todo |

## M3 — Web UI

The application shell, the virtualised grid, the add dialog, the folder browser, the detail pane and settings. Exit checkpoint: The full DS-equivalent screen works in a browser; Playwright E2E green.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T039](T039-ui-stack-and-design-tokens.md) | Install the UI stack and define the design tokens | T003 | todo |
| [T040](T040-router-and-authentication-screens.md) | Mount the router, the providers and the authentication screens | T009, T013, T014, T039 | todo |
| [T041](T041-task-store-and-formatters.md) | Build the live task store and the locale-aware formatters | T014, T025, T039 | todo |
| [T042](T042-virtualised-task-grid.md) | Render the virtualised task grid and the mobile card list | T021, T040, T041 | todo |
| [T043](T043-playwright-harness-and-grid-performance.md) | Stand up the Playwright harness with the setup and grid-performance specs | T040, T042 | todo |
| [T044](T044-sidebar-toolbar-and-status-bar.md) | Build the sidebar tree, the toolbar and the status bar | T022, T023, T041, T042 | todo |
| [T045](T045-column-management-and-ui-prefs.md) | Add column management and the UI preference document | T042, T044 | todo |
| [T046](T046-filesystem-roots-and-browse.md) | Serve the filesystem roots and browse endpoints | T007, T008, T020 | todo |
| [T047](T047-mkdir-free-space-and-folder-browser.md) | Serve mkdir and free space, and build the folder browser dialog | T046, T099 | todo |
| [T048](T048-detail-pane-and-file-tree.md) | Build the detail pane, its tabs and the file tree | T024, T032, T034, T035, T042 | todo |
| [T049](T049-add-task-dialog.md) | Build the add-task dialog and the file-selection step | T020, T031, T033, T044, T047, T048 | todo |
| [T050](T050-categories-and-tags.md) | Serve category CRUD, the tag list and category path resolution | T017, T020, T021 | todo |
| [T051](T051-event-stream-and-reconnect.md) | Connect the event stream, the reconnect ladder and the polling fallback | T011, T025, T041, T044 | todo |
| [T052](T052-i18n-plumbing-and-lint-rule.md) | Complete the i18n plumbing and the string-literal lint rule | T039, T044, T048, T049 | todo |
| [T053](T053-settings-screens.md) | Build the settings screen shell, General and Connection | T027, T045, T047, T050, T052 | todo |
| [T103](T103-progressive-web-app.md) | Ship the web app manifest, maskable icons and the service worker | T039, T040, T043 | todo |
| [T104](T104-accessibility-harness.md) | Add the accessibility harness: axe-core, the keyboard map and aria-rowcount | T042, T043, T044, T045, T048, T049, T051, T053 | todo |

## M4 — Search

The Torznab client, `dlsearch/v1` engines, `.dlm` import and the search screen. Exit checkpoint: Searching the bundled engines returns results; a result becomes a task in one click.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T054](T054-torznab-client.md) | Fetch and parse Torznab caps and search responses | T005, T123 | todo |
| [T055](T055-indexer-store-and-provider-wizard.md) | Store indexers and import a Prowlarr or Jackett instance | T006, T007, T008, T054 | todo |
| [T056](T056-dlsearch-definition-loader.md) | Load and validate `dlsearch/v1` definitions | T004, T005 | todo |
| [T057](T057-bundled-engine-definitions.md) | Bundle the four default engine definitions | T055, T056 | todo |
| [T058](T058-dlsearch-runner-and-probe.md) | Execute `rss` and `json` engines and probe an indexer | T054, T055, T056, T057 | todo |
| [T059](T059-dlm-static-import.md) | Import a Synology `.dlm` module by static analysis | T055, T056 | todo |
| [T060](T060-qbittorrent-plugin-metadata-import.md) | Import a qBittorrent nova3 `.py` plugin's metadata | T055, T056, T059 | todo |
| [T061](T061-async-search-jobs.md) | Run a search as an asynchronous job | T012, T055, T058 | todo |
| [T062](T062-per-engine-status-and-dedup.md) | Report per-engine status and collapse duplicate results | T054, T058, T061 | todo |
| [T063](T063-search-screen.md) | Build the search screen | T014, T041, T042, T044, T061, T062 | todo |
| [T064](T064-saved-searches.md) | Save and re-run a search | T043, T045, T061, T063 | todo |
| [T105](T105-static-linux-distributions-engine.md) | Ship the curated static `linux-distributions` engine | T056, T057, T058, T062 | todo |
| [T116](T116-indexers-settings-section.md) | Build the Indexers settings section | T053, T055, T058 | todo |
| [T122](T122-apply-ssrf-guard-to-user-uris.md) | Apply the SSRF guard to user-submitted URIs | T020, T031, T123 | todo |
| [T123](T123-ssrf-guarded-http-client.md) | Build the SSRF-guarded outbound HTTP client | T005 | todo |

## M5 — RSS

The feed poller, the rule engine, dry-run and the RSS screens. Exit checkpoint: A rule auto-downloads a release feed; the dry-run panel explains every non-match.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T065](T065-feed-store-and-crud.md) | Store RSS feeds and items and serve feed CRUD | T006, T007, T008 | todo |
| [T066](T066-feed-poller-and-backoff.md) | Poll feeds with conditional GET, jitter and the Sonarr backoff ladder | T012, T054, T065 | todo |
| [T067](T067-item-parser-and-uri-extraction.md) | Parse feed items and extract a download URI with the four-tier ladder | T015, T066 | todo |
| [T068](T068-rule-document-and-validator.md) | Validate rule documents and serve rule CRUD | T007, T008, T065 | todo |
| [T069](T069-rule-matching-algorithm.md) | Evaluate rules with the fourteen-step algorithm and the reason-code enum | T015, T016, T067, T068 | todo |
| [T070](T070-rule-dry-run-endpoint.md) | Dry-run a rule and report a reason code for every evaluated item | T068, T069 | todo |
| [T071](T071-commit-grabs-and-run-rule.md) | Commit rule matches as tasks and run a rule against existing items | T020, T024, T066, T069, T070 | todo |
| [T072](T072-rss-feeds-screen.md) | Build the RSS feeds and items screen | T014, T040, T044, T052, T065, T066 | todo |
| [T073](T073-rule-editor-and-live-preview.md) | Build the rule editor with the live dry-run preview | T047, T050, T068, T070, T071, T072 | todo |

## M6 — Post-processing, automation and the account

Extraction, the bandwidth schedule, watch folders, the operator account and its API tokens, notifications, the backup, settings and system-info endpoints, and the settings sections that read them. Exit checkpoint: Auto-extract, the 24×7 grid, watch folders and the account and notification settings sections all work end to end.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T074](T074-auto-extract-archives.md) | Extract completed archives with the safe recipe | T012, T017, T024 | todo |
| [T075](T075-extraction-password-list.md) | Try the per-task and shared extraction passwords | T074 | todo |
| [T076](T076-exdev-aware-move.md) | Move completed data across filesystems with progress | T074, T099 | todo |
| [T077](T077-notification-delivery.md) | Deliver task events to notification channels | T054, T074 | todo |
| [T078](T078-completion-hook.md) | Run the completion hook as an argument vector | T074 | todo |
| [T079](T079-global-bandwidth-governor.md) | Fan global rate limits out to every engine | T016, T019, T027, T037 | todo |
| [T080](T080-schedule-grid-endpoints.md) | Store and serve the 24×7 schedule grid | T027, T079 | todo |
| [T081](T081-schedule-evaluation-and-alternative-speed.md) | Apply the active schedule cell every minute | T066, T079, T080 | todo |
| [T082](T082-live-per-task-rate-limits.md) | Apply a per-task rate limit to a running task | T022, T037, T079 | todo |
| [T083](T083-watch-folder-loader.md) | Load .torrent files dropped into a watch folder | T020, T031, T066, T081 | todo |
| [T084](T084-api-tokens.md) | Issue, list and revoke API tokens | T008, T009 | todo |
| [T091](T091-database-backup-and-retention.md) | Back up the database on demand and prune on a schedule | T006, T012, T066 | todo |
| [T092](T092-settings-and-system-info.md) | Serve settings and system info without leaking secrets | T005, T027, T091 | todo |
| [T106](T106-notification-channel-endpoints.md) | Manage notification channels and test one | T077, T084 | todo |
| [T107](T107-tag-prefs-and-watch-folder-endpoints.md) | Reach the tag, preference and watch-folder tables over HTTP | T050, T083, T084 | todo |
| [T108](T108-settings-export-import-and-restore.md) | Export and import portable settings, and restore from the CLI | T080, T106, T107 | todo |
| [T110](T110-bandwidth-precedence-and-dst.md) | Resolve the bandwidth precedence chain and the schedule time zone | T079, T080, T081, T082 | todo |
| [T111](T111-delete-data-and-hardlink-safety.md) | Delete downloaded data safely and prove hardlink survival | T023, T076 | todo |
| [T117](T117-rss-settings-section.md) | Build the RSS settings section | T053, T065, T066, T068, T092 | todo |
| [T118](T118-bandwidth-settings-and-schedule-grid.md) | Build the Bandwidth settings section and the 24×7 schedule grid | T053, T079, T080, T092, T110 | todo |
| [T119](T119-downloads-and-bittorrent-settings.md) | Build the Downloads and BitTorrent settings sections | T050, T053, T074, T083, T092, T107 | todo |
| [T120](T120-account-and-notifications-settings.md) | Build the Account and Notifications settings sections | T053, T084, T106 | todo |

## M7 — yt-dlp, packaging and release

The yt-dlp engine, the release hardening of the image and the compose stack, the release pipeline, the log viewer and the documentation. Exit checkpoint: Multi-arch image published and signed; the quickstart brings the stack up from scratch.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T087](T087-ytdlp-subprocess-runner.md) | Run yt-dlp as a supervised subprocess | T005, T016 | todo |
| [T088](T088-ytdlp-extractor-cache.md) | Cache the yt-dlp extractor patterns for the router | T016, T087 | deferred |
| [T089](T089-ytdlp-progress-and-exit-codes.md) | Parse yt-dlp progress lines and exit codes | T087 | todo |
| [T090](T090-ytdlp-engine-registration.md) | Register the yt-dlp engine and run the contract suite | T028, T089 | todo |
| [T093](T093-harden-runtime-image.md) | Harden the runtime image for a multi-arch release | T124 | todo |
| [T094](T094-harden-compose-and-release-verification.md) | Harden the compose stack and document release verification | T093, T125 | todo |
| [T095](T095-proxy-hardening-and-headers.md) | Harden the proxied deployment and ship the proxy snippets | T007, T013, T094 | todo |
| [T096](T096-log-redaction-and-system-logs.md) | Redact secrets in logs and serve the system log | T004, T010, T024, T092 | todo |
| [T097](T097-release-pipeline-and-pin-bump.md) | Publish signed multi-arch images and bump the yt-dlp pin weekly | T002, T093, T094 | todo |
| [T113](T113-ytdlp-pin-and-capability-probe.md) | Enforce the yt-dlp pin and probe the runtime at boot | T090, T093, T097 | todo |
| [T115](T115-aria2-image-build-and-publish.md) | Build and publish the aria2 image | T093, T094, T097 | todo |
| [T121](T121-advanced-settings-and-log-viewer.md) | Build the Advanced settings section and the system log viewer | T053, T091, T092, T096, T108 | todo |

## Deferral register
A `should` or `could` requirement may be deferred only by adding a row here, naming the requirement, the
reason and the task that will carry it.

| Requirement | Deferred from | Reason | Carried by |
|---|---|---|---|
| M0 exit: a rendered login page | brief §8 | T013 embeds a placeholder `index.html`; the login screen is a React route. | T040 |
| M0 exit: the `integration` CI job green | brief §8 | `make test-integration` has no adapter to exercise until M1/M2. | T028 |
| Populating row 3 of the routing table, so a yt-dlp URL reaches the media lane | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme) | The mechanism T088 assumed does not exist in yt-dlp, and 284 of its 1702 patterns do not compile with Go `regexp` ([`06-download-engines.md`](../06-download-engines.md#72-routing-check) §7.2). T016 still ships the routing table and its `mediaMatch` hook; only the hook's data source is deferred, and a nil hook routes such a URL to aria2 rather than mis-routing it. Needs an ADR. | T088, after the ADR |
| M3 exit: the eight remaining settings sections | brief §8 | T053 ships the settings shell with General and Connection and lists the rest in `IMPLEMENTED`; each remaining section needs endpoints that do not exist in M3. | T116, T117, T118, T119, T120, T121 |

## A note on identifier order

Task identifiers **T098–T125** are overflow numbers allocated after the original ranges were set. They
belong to earlier milestones than their number suggests — T098 and T099 are M1, T100 and T101 are M2,
T103 and T104 are M3, T105, T116, T122 and T123 are M4, T106–T111 and T117–T120 are M6,
T113, T115 and T121 are M7, and T124 and T125 are M0. A dependency on a numerically higher identifier is
therefore usually not a forward reference: work milestones in order and the dependency is already
satisfied. Do not "fix" these edges.

Two consequences of that overflow numbering are recorded rather than "fixed":

- **T091 and T092 sit in M6, not M7.** The settings sections T117, T118 and T119 need
  `GET`/`PATCH /settings`, so the settings and system-info endpoints — and the backup handler whose file
  they extend — moved forward into M6 with them. M7 keeps the yt-dlp engine, the release hardening and the
  release pipeline.
- **In M4, take T123 before T054.** T054 is the lower-numbered row but depends on T123, which owns the
  SSRF-guarded HTTP client it is built on; T122 depends on T123 for the same reason. Rule 2 handles this
  by itself — T054's dependencies are not `done`, so the next unblocked row is T123 — but do not
  "correct" the edge.

## Roster

### M0

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T001 | Create the Makefile and the doc-lint script | — | no | done | [T001](T001-makefile-and-doclint.md) |
| T002 | Add the CI workflows and the OpenAPI generation script | T001 | yes | done | [T002](T002-ci-workflows-and-openapi-gate.md) |
| T003 | Scaffold the Vite SPA build and its toolchain | T001 | yes | done | [T003](T003-web-build-scaffold.md) |
| T004 | Bootstrap the Go module, pin every dependency and build the entrypoint | T001 | no | done | [T004](T004-go-module-and-entrypoint.md) |
| T005 | Load the DLTOOL_ environment and generate the boot secrets | T004 | no | done | [T005](T005-config-loader-and-secrets.md) |
| T006 | Open the SQLite store, generate ULIDs and apply the initial migration | T004, T005 | yes | done | [T006](T006-sqlite-store-and-initial-migration.md) |
| T007 | Serve chi and Huma under the base path and commit the OpenAPI document | T005, T006 | no | done | [T007](T007-http-server-and-openapi.md) |
| T008 | Authenticate every request with a session cookie or a bearer token | T006, T007 | no | done | [T008](T008-session-and-csrf-authentication.md) |
| T009 | Complete the first run with a one-time setup token | T008 | no | done | [T009](T009-first-run-setup-and-login.md) |
| T010 | Serve health, readiness and Prometheus metrics | T006, T007 | no | done | [T010](T010-health-readiness-and-metrics.md) |
| T011 | Build the SSE hub and the rid ring buffer | T004 | yes | done | [T011](T011-sse-hub-and-rid-ring.md) |
| T012 | Run the database-backed job worker pool | T006 | no | done | [T012](T012-job-worker-pool.md) |
| T013 | Embed the built SPA and serve it under the base path | T003, T007 | no | done | [T013](T013-embed-spa-and-base-path.md) |
| T014 | Generate the typed API client from the committed OpenAPI document | T002, T007, T013 | yes | done | [T014](T014-typed-api-client.md) |
| T124 | Write the runtime Dockerfile and the entrypoint | T003, T004, T010, T013 | no | done | [T124](T124-runtime-dockerfile-and-entrypoint.md) |
| T125 | Write `compose.yaml` and `.env.example` | T124 | yes | done | [T125](T125-compose-and-env-example.md) |

### M1

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T015 | Normalise and decode a submitted URI | T004 | yes | done | [T015](T015-normalise-submitted-uris.md) |
| T016 | Define the Engine interface and the protocol router | T004, T015 | yes | done | [T016](T016-engine-interface-and-router.md) |
| T017 | Persist tasks and enforce the task state machine | T006, T016 | yes | done | [T017](T017-task-store-and-state-machine.md) |
| T018 | Map aria2 JSON-RPC status to the canonical task state | T016 | yes | done | [T018](T018-aria2-status-mapping.md) |
| T019 | Implement the aria2 adapter and the engine registry | T016, T018 | no | done | [T019](T019-aria2-adapter.md) |
| T020 | Create tasks from submitted URIs | T007, T008, T015, T016, T017, T019 | no | done | [T020](T020-create-tasks-endpoint.md) |
| T021 | List, filter and sort tasks | T017, T020 | no | done | [T021](T021-list-and-filter-tasks.md) |
| T022 | Update a task and apply bulk lifecycle actions | T019, T020, T021 | no | todo | [T022](T022-task-actions-and-patch.md) |
| T023 | Remove a task with or without its data | T020, T021, T022 | no | todo | [T023](T023-remove-task-and-data.md) |
| T024 | Record and serve the per-task event log | T017, T020, T023 | no | todo | [T024](T024-task-event-log.md) |
| T025 | Compute rid deltas and stream them over SSE | T011, T021, T022, T024 | no | todo | [T025](T025-rid-deltas-over-sse.md) |
| T026 | Reconcile tasks with the engines at boot and on every poll | T019, T024, T025 | yes | todo | [T026](T026-boot-reconciliation.md) |
| T027 | List engines and test connectivity | T019, T020 | yes | todo | [T027](T027-list-and-test-engines.md) |
| T098 | Admit tasks under the concurrency limits | T017, T019, T020, T024, T026 | yes | todo | [T098](T098-concurrency-limiter.md) |
| T099 | Reserve disk space and keep a free-space floor | T020, T024, T098 | yes | todo | [T099](T099-disk-space-reservation.md) |

### M2

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T028 | Add the shared engine contract test suite | T016, T019 | yes | todo | [T028](T028-engine-contract-test-suite.md) |
| T029 | Implement the qBittorrent session, state mapping and `torrents/add` | T005, T016, T019, T027 | no | todo | [T029](T029-qbittorrent-session-and-add.md) |
| T030 | Track qBittorrent state through `sync/maindata` rid deltas | T026, T029 | no | todo | [T030](T030-qbittorrent-maindata-deltas.md) |
| T031 | Parse torrent metainfo and serve `POST /tasks/inspect` | T015, T020 | no | todo | [T031](T031-metainfo-and-inspect-endpoint.md) |
| T032 | Select and prioritise the files of a task | T021, T029, T030 | no | todo | [T032](T032-file-selection-and-priorities.md) |
| T033 | Accept uploaded files, the subfolder option and a create-time file selection | T020, T031, T032 | no | todo | [T033](T033-uploads-subfolder-and-selection.md) |
| T034 | List, add and remove a task's trackers | T021, T029 | no | todo | [T034](T034-task-trackers.md) |
| T035 | List a task's connected peers | T029, T030, T034 | no | todo | [T035](T035-task-peers.md) |
| T036 | Apply share limits, sequential download and the location mutators | T022, T029 | yes | todo | [T036](T036-share-limits-and-mutators.md) |
| T037 | Apply per-task and global rate limits to running qBittorrent tasks | T022, T029, T030 | yes | todo | [T037](T037-qbittorrent-rate-limits.md) |
| T038 | Resolve magnet metadata and complete the qBittorrent adapter | T028, T029, T030, T031, T032, T036, T037 | no | todo | [T038](T038-magnet-metadata-and-contract-call-site.md) |
| T100 | Record both infohashes and reject duplicate torrents | T017, T020, T029, T030, T031 | no | todo | [T100](T100-infohash-identity-and-duplicates.md) |
| T101 | Assert and force engine conformance at boot | T019, T027, T028, T029 | no | todo | [T101](T101-engine-conformance-at-boot.md) |

### M3

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T039 | Install the UI stack and define the design tokens | T003 | yes | todo | [T039](T039-ui-stack-and-design-tokens.md) |
| T040 | Mount the router, the providers and the authentication screens | T009, T013, T014, T039 | yes | todo | [T040](T040-router-and-authentication-screens.md) |
| T041 | Build the live task store and the locale-aware formatters | T014, T025, T039 | yes | todo | [T041](T041-task-store-and-formatters.md) |
| T042 | Render the virtualised task grid and the mobile card list | T021, T040, T041 | yes | todo | [T042](T042-virtualised-task-grid.md) |
| T043 | Stand up the Playwright harness with the setup and grid-performance specs | T040, T042 | yes | todo | [T043](T043-playwright-harness-and-grid-performance.md) |
| T044 | Build the sidebar tree, the toolbar and the status bar | T022, T023, T041, T042 | yes | todo | [T044](T044-sidebar-toolbar-and-status-bar.md) |
| T045 | Add column management and the UI preference document | T042, T044 | no | todo | [T045](T045-column-management-and-ui-prefs.md) |
| T046 | Serve the filesystem roots and browse endpoints | T007, T008, T020 | no | todo | [T046](T046-filesystem-roots-and-browse.md) |
| T047 | Serve mkdir and free space, and build the folder browser dialog | T046, T099 | no | todo | [T047](T047-mkdir-free-space-and-folder-browser.md) |
| T048 | Build the detail pane, its tabs and the file tree | T024, T032, T034, T035, T042 | yes | todo | [T048](T048-detail-pane-and-file-tree.md) |
| T049 | Build the add-task dialog and the file-selection step | T020, T031, T033, T044, T047, T048 | no | todo | [T049](T049-add-task-dialog.md) |
| T050 | Serve category CRUD, the tag list and category path resolution | T017, T020, T021 | no | todo | [T050](T050-categories-and-tags.md) |
| T051 | Connect the event stream, the reconnect ladder and the polling fallback | T011, T025, T041, T044 | yes | todo | [T051](T051-event-stream-and-reconnect.md) |
| T052 | Complete the i18n plumbing and the string-literal lint rule | T039, T044, T048, T049 | no | todo | [T052](T052-i18n-plumbing-and-lint-rule.md) |
| T053 | Build the settings screen shell, General and Connection | T027, T045, T047, T050, T052 | yes | todo | [T053](T053-settings-screens.md) |
| T103 | Ship the web app manifest, maskable icons and the service worker | T039, T040, T043 | yes | todo | [T103](T103-progressive-web-app.md) |
| T104 | Add the accessibility harness: axe-core, the keyboard map and aria-rowcount | T042, T043, T044, T045, T048, T049, T051, T053 | yes | todo | [T104](T104-accessibility-harness.md) |

### M4

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T054 | Fetch and parse Torznab caps and search responses | T005, T123 | yes | todo | [T054](T054-torznab-client.md) |
| T055 | Store indexers and import a Prowlarr or Jackett instance | T006, T007, T008, T054 | no | todo | [T055](T055-indexer-store-and-provider-wizard.md) |
| T056 | Load and validate `dlsearch/v1` definitions | T004, T005 | yes | todo | [T056](T056-dlsearch-definition-loader.md) |
| T057 | Bundle the four default engine definitions | T055, T056 | no | todo | [T057](T057-bundled-engine-definitions.md) |
| T058 | Execute `rss` and `json` engines and probe an indexer | T054, T055, T056, T057 | no | todo | [T058](T058-dlsearch-runner-and-probe.md) |
| T059 | Import a Synology `.dlm` module by static analysis | T055, T056 | no | todo | [T059](T059-dlm-static-import.md) |
| T060 | Import a qBittorrent nova3 `.py` plugin's metadata | T055, T056, T059 | no | todo | [T060](T060-qbittorrent-plugin-metadata-import.md) |
| T061 | Run a search as an asynchronous job | T012, T055, T058 | no | todo | [T061](T061-async-search-jobs.md) |
| T062 | Report per-engine status and collapse duplicate results | T054, T058, T061 | no | todo | [T062](T062-per-engine-status-and-dedup.md) |
| T063 | Build the search screen | T014, T041, T042, T044, T061, T062 | no | todo | [T063](T063-search-screen.md) |
| T064 | Save and re-run a search | T043, T045, T061, T063 | no | todo | [T064](T064-saved-searches.md) |
| T105 | Ship the curated static `linux-distributions` engine | T056, T057, T058, T062 | no | todo | [T105](T105-static-linux-distributions-engine.md) |
| T116 | Build the Indexers settings section | T053, T055, T058 | no | todo | [T116](T116-indexers-settings-section.md) |
| T122 | Apply the SSRF guard to user-submitted URIs | T020, T031, T123 | no | todo | [T122](T122-apply-ssrf-guard-to-user-uris.md) |
| T123 | Build the SSRF-guarded outbound HTTP client | T005 | yes | todo | [T123](T123-ssrf-guarded-http-client.md) |

### M5

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T065 | Store RSS feeds and items and serve feed CRUD | T006, T007, T008 | yes | todo | [T065](T065-feed-store-and-crud.md) |
| T066 | Poll feeds with conditional GET, jitter and the Sonarr backoff ladder | T012, T054, T065 | no | todo | [T066](T066-feed-poller-and-backoff.md) |
| T067 | Parse feed items and extract a download URI with the four-tier ladder | T015, T066 | yes | todo | [T067](T067-item-parser-and-uri-extraction.md) |
| T068 | Validate rule documents and serve rule CRUD | T007, T008, T065 | no | todo | [T068](T068-rule-document-and-validator.md) |
| T069 | Evaluate rules with the fourteen-step algorithm and the reason-code enum | T015, T016, T067, T068 | yes | todo | [T069](T069-rule-matching-algorithm.md) |
| T070 | Dry-run a rule and report a reason code for every evaluated item | T068, T069 | no | todo | [T070](T070-rule-dry-run-endpoint.md) |
| T071 | Commit rule matches as tasks and run a rule against existing items | T020, T024, T066, T069, T070 | no | todo | [T071](T071-commit-grabs-and-run-rule.md) |
| T072 | Build the RSS feeds and items screen | T014, T040, T044, T052, T065, T066 | no | todo | [T072](T072-rss-feeds-screen.md) |
| T073 | Build the rule editor with the live dry-run preview | T047, T050, T068, T070, T071, T072 | no | todo | [T073](T073-rule-editor-and-live-preview.md) |

### M6

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T074 | Extract completed archives with the safe recipe | T012, T017, T024 | yes | todo | [T074](T074-auto-extract-archives.md) |
| T075 | Try the per-task and shared extraction passwords | T074 | no | todo | [T075](T075-extraction-password-list.md) |
| T076 | Move completed data across filesystems with progress | T074, T099 | yes | todo | [T076](T076-exdev-aware-move.md) |
| T077 | Deliver task events to notification channels | T054, T074 | yes | todo | [T077](T077-notification-delivery.md) |
| T078 | Run the completion hook as an argument vector | T074 | yes | todo | [T078](T078-completion-hook.md) |
| T079 | Fan global rate limits out to every engine | T016, T019, T027, T037 | yes | todo | [T079](T079-global-bandwidth-governor.md) |
| T080 | Store and serve the 24×7 schedule grid | T027, T079 | yes | todo | [T080](T080-schedule-grid-endpoints.md) |
| T081 | Apply the active schedule cell every minute | T066, T079, T080 | no | todo | [T081](T081-schedule-evaluation-and-alternative-speed.md) |
| T082 | Apply a per-task rate limit to a running task | T022, T037, T079 | no | todo | [T082](T082-live-per-task-rate-limits.md) |
| T083 | Load .torrent files dropped into a watch folder | T020, T031, T066, T081 | yes | todo | [T083](T083-watch-folder-loader.md) |
| T084 | Issue, list and revoke API tokens | T008, T009 | yes | todo | [T084](T084-api-tokens.md) |
| T091 | Back up the database on demand and prune on a schedule | T006, T012, T066 | no | todo | [T091](T091-database-backup-and-retention.md) |
| T092 | Serve settings and system info without leaking secrets | T005, T027, T091 | no | todo | [T092](T092-settings-and-system-info.md) |
| T106 | Manage notification channels and test one | T077, T084 | yes | todo | [T106](T106-notification-channel-endpoints.md) |
| T107 | Reach the tag, preference and watch-folder tables over HTTP | T050, T083, T084 | yes | todo | [T107](T107-tag-prefs-and-watch-folder-endpoints.md) |
| T108 | Export and import portable settings, and restore from the CLI | T080, T106, T107 | yes | todo | [T108](T108-settings-export-import-and-restore.md) |
| T110 | Resolve the bandwidth precedence chain and the schedule time zone | T079, T080, T081, T082 | no | todo | [T110](T110-bandwidth-precedence-and-dst.md) |
| T111 | Delete downloaded data safely and prove hardlink survival | T023, T076 | no | todo | [T111](T111-delete-data-and-hardlink-safety.md) |
| T117 | Build the RSS settings section | T053, T065, T066, T068, T092 | no | todo | [T117](T117-rss-settings-section.md) |
| T118 | Build the Bandwidth settings section and the 24×7 schedule grid | T053, T079, T080, T092, T110 | no | todo | [T118](T118-bandwidth-settings-and-schedule-grid.md) |
| T119 | Build the Downloads and BitTorrent settings sections | T050, T053, T074, T083, T092, T107 | no | todo | [T119](T119-downloads-and-bittorrent-settings.md) |
| T120 | Build the Account and Notifications settings sections | T053, T084, T106 | no | todo | [T120](T120-account-and-notifications-settings.md) |

### M7

| ID | Title | Depends on | Parallel | Status | File |
|---|---|---|---|---|---|
| T087 | Run yt-dlp as a supervised subprocess | T005, T016 | yes | todo | [T087](T087-ytdlp-subprocess-runner.md) |
| T088 | Cache the yt-dlp extractor patterns for the router | T016, T087 | no | deferred | [T088](T088-ytdlp-extractor-cache.md) |
| T089 | Parse yt-dlp progress lines and exit codes | T087 | yes | todo | [T089](T089-ytdlp-progress-and-exit-codes.md) |
| T090 | Register the yt-dlp engine and run the contract suite | T028, T089 | no | todo | [T090](T090-ytdlp-engine-registration.md) |
| T093 | Harden the runtime image for a multi-arch release | T124 | yes | todo | [T093](T093-harden-runtime-image.md) |
| T094 | Harden the compose stack and document release verification | T093, T125 | no | todo | [T094](T094-harden-compose-and-release-verification.md) |
| T095 | Harden the proxied deployment and ship the proxy snippets | T007, T013, T094 | no | todo | [T095](T095-proxy-hardening-and-headers.md) |
| T096 | Redact secrets in logs and serve the system log | T004, T010, T024, T092 | no | todo | [T096](T096-log-redaction-and-system-logs.md) |
| T097 | Publish signed multi-arch images and bump the yt-dlp pin weekly | T002, T093, T094 | yes | todo | [T097](T097-release-pipeline-and-pin-bump.md) |
| T113 | Enforce the yt-dlp pin and probe the runtime at boot | T090, T093, T097 | no | todo | [T113](T113-ytdlp-pin-and-capability-probe.md) |
| T115 | Build and publish the aria2 image | T093, T094, T097 | no | todo | [T115](T115-aria2-image-build-and-publish.md) |
| T121 | Build the Advanced settings section and the system log viewer | T053, T091, T092, T096, T108 | no | todo | [T121](T121-advanced-settings-and-log-viewer.md) |

## Decisions referenced
| ADR | Decision |
|---|---|
| [0017](../decisions/0017-exclusive-control-of-engines.md) | dl-tool assumes exclusive control of its engines — why T102 is unused |

## Open questions
- (none)

## Change log
| Date | Change |
|---|---|
| 2026-09-01 | Initial version: 112 tasks across M0–M7, with the permanently unused IDs recorded. |
| 2026-09-01 | Added T116–T125; deleted the "Missing tasks" table; moved the image and compose stack into M0 (T124, T125) and rescoped T093 and T094 to hardening; split the SSRF guard out of T054 into T123. |
| 2026-09-01 | Executability pass: moved T091, T092 and T117 into M6 so no task depends on a later milestone; completed every `Blocks` field against the `Depends on` graph; recorded the M3 settings-section deferral. |
| 2026-09-01 | Final consistency pass: moved `A note on identifier order` and `Roster` above `Decisions referenced` so the section order matches the document template, and corrected the identifier-order note to place T117 in M6 with T118–T120. |
| 2026-09-02 | Multi-user model dropped: T085, T086 and T109 deleted and their identifiers retired; T084 rescoped to API tokens alone and T120 to the account section ([ADR-0019](../decisions/0019-single-account-no-ownership.md)). |

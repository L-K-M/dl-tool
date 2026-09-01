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

IDs are never reused. `docs/15-*` and ADR 0014 carry the same permanent gap.

## M0 — Foundations

Repository, CI, configuration, the database and migrations, authentication, health and the SSE skeleton. Exit checkpoint: `docker compose up -d` serves a login page; `curl /healthz` returns `{"status":"ok"}`; CI green.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T001](T001-makefile-and-doclint.md) | Create the Makefile and the doc-lint script | — | todo |
| [T002](T002-ci-workflows-and-openapi-gate.md) | Add the CI workflows and the OpenAPI generation script | T001 | todo |
| [T003](T003-web-build-scaffold.md) | Scaffold the Vite SPA build and its toolchain | T001 | todo |
| [T004](T004-go-module-and-entrypoint.md) | Bootstrap the Go module, pin every dependency and build the entrypoint | T001 | todo |
| [T005](T005-config-loader-and-secrets.md) | Load the DLTOOL_ environment and generate the boot secrets | T004 | todo |
| [T006](T006-sqlite-store-and-initial-migration.md) | Open the SQLite store, generate ULIDs and apply the initial migration | T004, T005 | todo |
| [T007](T007-http-server-and-openapi.md) | Serve chi and Huma under the base path and commit the OpenAPI document | T005, T006 | todo |
| [T008](T008-session-and-csrf-authentication.md) | Authenticate every request with a session cookie or a bearer token | T006, T007 | todo |
| [T009](T009-first-run-setup-and-login.md) | Complete the first run with a one-time setup token | T008 | todo |
| [T010](T010-health-readiness-and-metrics.md) | Serve health, readiness and Prometheus metrics | T006, T007 | todo |
| [T011](T011-sse-hub-and-rid-ring.md) | Build the SSE hub and the rid ring buffer | T004 | todo |
| [T012](T012-job-worker-pool.md) | Run the database-backed job worker pool | T006 | todo |
| [T013](T013-embed-spa-and-base-path.md) | Embed the built SPA and serve it under the base path | T003, T007 | todo |
| [T014](T014-typed-api-client.md) | Generate the typed API client from the committed OpenAPI document | T002, T007, T013 | todo |

## M1 — Task core and aria2

URI normalisation, the Engine interface, the aria2 adapter, the queue and the event log. Exit checkpoint: A pasted HTTPS URL downloads to `/data`, progress streams over SSE, pause/resume/remove work.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T015](T015-normalise-submitted-uris.md) | Normalise and decode a submitted URI | T004 | todo |
| [T016](T016-engine-interface-and-router.md) | Define the Engine interface and the protocol router | T004, T015 | todo |
| [T017](T017-task-store-and-state-machine.md) | Persist tasks and enforce the task state machine | T006, T016 | todo |
| [T018](T018-aria2-status-mapping.md) | Map aria2 JSON-RPC status to the canonical task state | T016 | todo |
| [T019](T019-aria2-adapter.md) | Implement the aria2 adapter and the engine registry | T016, T018 | todo |
| [T020](T020-create-tasks-endpoint.md) | Create tasks from submitted URIs | T007, T008, T015, T016, T017, T019 | todo |
| [T021](T021-list-and-filter-tasks.md) | List, filter and sort tasks | T017, T020 | todo |
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
| [T054](T054-torznab-client-and-ssrf-guard.md) | Fetch and parse Torznab responses through an SSRF-guarded client | T005 | todo |
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

## M6 — Post-processing and multi-user

Extraction, the bandwidth schedule, watch folders, users and quotas, and notifications. Exit checkpoint: Auto-extract, the 24×7 grid, and per-user destinations and quotas all work end to end.

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
| [T084](T084-api-tokens-and-admin-guard.md) | Issue, list and revoke API tokens | T008, T009 | todo |
| [T085](T085-ownership-filtering-and-quotas.md) | Scope tasks to their owner and enforce the storage quota | T020, T021, T022, T084 | todo |
| [T086](T086-users-and-default-destinations.md) | Manage users, their default destinations and the process order | T084, T085, T098 | todo |
| [T106](T106-notification-channel-endpoints.md) | Manage notification channels and test one | T077, T084 | todo |
| [T107](T107-tag-prefs-and-watch-folder-endpoints.md) | Reach the tag, preference and watch-folder tables over HTTP | T050, T083, T084 | todo |
| [T108](T108-settings-export-import-and-restore.md) | Export and import portable settings, and restore from the CLI | T080, T086, T106, T107 | todo |
| [T109](T109-per-user-root-jail.md) | Jail non-admins to their own destination subtree | T046, T047, T085, T086, T107 | todo |
| [T110](T110-bandwidth-precedence-and-dst.md) | Resolve the bandwidth precedence chain and the schedule time zone | T079, T080, T081, T082 | todo |
| [T111](T111-delete-data-and-hardlink-safety.md) | Delete downloaded data safely and prove hardlink survival | T023, T076, T085, T109 | todo |

## M7 — yt-dlp, packaging and release

The yt-dlp engine, the runtime image, compose, the release pipeline and the documentation. Exit checkpoint: Multi-arch image published and signed; the quickstart brings the stack up from scratch.

| Task | Title | Depends on | Status |
|---|---|---|---|
| [T087](T087-ytdlp-subprocess-runner.md) | Run yt-dlp as a supervised subprocess | T005, T016 | todo |
| [T088](T088-ytdlp-extractor-cache.md) | Cache the yt-dlp extractor patterns for the router | T016, T087 | todo |
| [T089](T089-ytdlp-progress-and-exit-codes.md) | Parse yt-dlp progress lines and exit codes | T087 | todo |
| [T090](T090-ytdlp-engine-registration.md) | Register the yt-dlp engine and run the contract suite | T028, T088, T089 | todo |
| [T091](T091-database-backup-and-retention.md) | Back up the database on demand and prune on a schedule | T006, T012, T066 | todo |
| [T092](T092-settings-and-system-info.md) | Serve settings and system info without leaking secrets | T005, T027, T091 | todo |
| [T093](T093-runtime-image-and-entrypoint.md) | Build the runtime image and the PUID/PGID entrypoint | T003, T004, T013 | todo |
| [T094](T094-compose-stack-and-quickstart.md) | Ship the compose stack, the env template and the quickstart | T093 | todo |
| [T095](T095-proxy-hardening-and-headers.md) | Harden the proxied deployment and ship the proxy snippets | T007, T013, T094 | todo |
| [T096](T096-log-redaction-and-system-logs.md) | Redact secrets in logs and serve the system log | T004, T010, T024, T092 | todo |
| [T097](T097-release-pipeline-and-pin-bump.md) | Publish signed multi-arch images and bump the yt-dlp pin weekly | T002, T093, T094 | todo |
| [T113](T113-ytdlp-pin-and-capability-probe.md) | Enforce the yt-dlp pin and probe the runtime at boot | T090, T093, T097 | todo |
| [T115](T115-aria2-image-build-and-publish.md) | Build and publish the aria2 image | T093, T094, T097 | todo |

## Deferral register
A `should` or `could` requirement may be deferred only by adding a row here, naming the requirement, the
reason and the task that will carry it.

| Requirement | Deferred from | Reason | Carried by |
|---|---|---|---|
| (none) | — | — | — |

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

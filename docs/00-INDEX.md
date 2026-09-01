# 00 — Index

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** anything else in this repository

## Purpose

The map of the plan. It tells you which file answers which question, what order to read them in, and how
to pick the next piece of work. It contains no facts of its own — every row links to the document that
owns the fact.

## Scope of this document

- In scope: the document roster, the fact-ownership table, the reading order, the rule for choosing the
  next task, and the register of permanently unused identifiers.
- Out of scope (lives instead in): the tasks themselves → [`tasks/00-task-index.md`](tasks/00-task-index.md);
  the Definition of Done → [`13-testing-and-verification.md`](13-testing-and-verification.md);
  every *why* → [`decisions/`](decisions/).

## What dl-tool is

A self-hosted replacement for Synology Download Station, deployed with Docker Compose. It is a **control
plane, not a download engine**: it owns the unified queue, users, destinations, search, RSS, scheduling
and the web UI, and delegates transferring to aria2, qBittorrent-nox and yt-dlp.

The product goal, in the repository owner's words, is the acceptance test for every decision:

> "I just want a tool that basically does the same thing from the user's pov and that I can host wherever
> I want using docker compose."

So: **does this make dl-tool feel like Download Station to the person using it, and does it stay a plain
docker compose deployment?** If not, it is out of scope.

## Start here

1. Read [`01-vision-and-scope.md`](01-vision-and-scope.md) — what this is and what it deliberately is not.
2. Read [`13-testing-and-verification.md`](13-testing-and-verification.md) — the **Definition of Done**,
   which every task is measured against.
3. Open [`tasks/00-task-index.md`](tasks/00-task-index.md) and take the next unblocked task.
4. Read only that task file and the documents its **Context you need** section names. Do not explore.

## How to pick the next task

Work milestones in order; **M0 is blocking**. Within a milestone, take the lowest-numbered row in
[`tasks/00-task-index.md`](tasks/00-task-index.md) whose `Depends on` tasks are all `done`. Open that
file and follow it. Do not read other task files.

## Document roster

| # | Document | Answers |
|---|---|---|
| 01 | [Vision and scope](01-vision-and-scope.md) | What is this, who is it for, what does it deliberately not do, and how does it map onto Download Station? |
| 02 | [Requirements](02-requirements.md) | What exactly must it do? `FR-###` and `NFR-###` in EARS notation. |
| 03 | [Architecture](03-architecture.md) | How do the pieces fit together? Context, containers, runtime flows, the task state machine. |
| 04 | [Data model](04-data-model.md) | What is stored? Exact DDL, indices, enums, migration and backup policy. |
| 05 | [API contract](05-api-contract.md) | Every endpoint: request, response, status codes, the SSE stream. |
| 06 | [Download engines](06-download-engines.md) | The `Engine` interface and the aria2, qBittorrent and yt-dlp adapters. |
| 07 | [Search and indexers](07-search-and-indexers.md) | Torznab client, the `dlsearch/v1` YAML format, `.dlm` import, bundled engines. |
| 08 | [RSS automation](08-rss-automation.md) | Feed polling, the rule schema, the matching algorithm, the dry run. |
| 09 | [Web UI specification](09-web-ui-spec.md) | Every screen, dialog, column and interaction. **The centrepiece.** |
| 10 | [Deployment and compose](10-deployment-and-compose.md) | `compose.yaml`, volumes, ports, images, reverse proxy, NAS notes. |
| 11 | [Configuration reference](11-config-reference.md) | Every environment variable and runtime setting. |
| 12 | [Security and threat model](12-security-and-threat-model.md) | SSRF, path safety, extraction, auth, the CVE-derived rules. |
| 13 | [Testing and verification](13-testing-and-verification.md) | Test layout, Makefile targets, the **Definition of Done**. |
| 14 | [Conventions](14-conventions.md) | Layout, naming, error model, logging, commits. |
| 16 | [Prior art and research](16-prior-art-and-research.md) | What already exists, what was rejected, and the evidence behind the plan. |
| 17 | [Operations and runbook](17-operations-and-runbook.md) | Startup, shutdown, backup, upgrade, and what to do when it breaks. |
| — | [Glossary](glossary.md) | Every term of art, one line each. |
| — | [Decisions](decisions/) | One ADR per decision, each with a `Confirmation` section naming an executable check. |
| — | [Task index](tasks/00-task-index.md) | The ordered, dependency-annotated roster of work. |

`15` is **permanently unused** — see below.

## Who owns which fact

Each fact has exactly one home. Everything else links to it. Never copy a table between documents;
duplication is how plans rot.

| Fact | Home |
|---|---|
| Requirement text and IDs | [`02-requirements.md`](02-requirements.md) |
| Table columns, DDL, enum values | [`04-data-model.md`](04-data-model.md) |
| HTTP request and response shapes, status codes | [`05-api-contract.md`](05-api-contract.md) |
| The `Engine` interface and per-engine wire details | [`06-download-engines.md`](06-download-engines.md) |
| The `dlsearch/v1` definition schema | [`07-search-and-indexers.md`](07-search-and-indexers.md) |
| The RSS rule schema and matching algorithm | [`08-rss-automation.md`](08-rss-automation.md) |
| Screens, grid columns, dialog fields | [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| Ports, volumes, compose service names | [`10-deployment-and-compose.md`](10-deployment-and-compose.md) |
| Environment variables and settings | [`11-config-reference.md`](11-config-reference.md) |
| Makefile targets and the Definition of Done | [`13-testing-and-verification.md`](13-testing-and-verification.md) |
| Any *why* | [`decisions/`](decisions/) |

## Permanently unused identifiers

Identifiers are never reused and never renumbered. Two scope cuts left gaps; the gaps stay.

| Identifier | Why |
|---|---|
| `docs/15-*` | Held the compatibility-API document, then the migration document. Both cut. |
| `ADR-0014` | Opt-in qBittorrent and Synology compatibility façades. Withdrawn before it was written. |
| `FR-130`–`FR-139` | The compatibility-façade requirement range. Withdrawn. |
| `T102` | Foreign-task policy. dl-tool always ignores tasks it did not create — [ADR-0017](decisions/0017-exclusive-control-of-engines.md). |
| `T112` | Façade authentication mapping. Withdrawn with the façades. |
| `T114` | Migration importers. Withdrawn with the migration subsystem. |

Two things dl-tool deliberately does **not** do, so nobody re-adds them: it exposes no compatibility
surface (it serves `/api/v1` only and never presents itself as another product's API), and it has no
migration tooling. Note that `/api/v2` paths in [`06-download-engines.md`](06-download-engines.md) and
the qBittorrent task files are qBittorrent's *own* API, which dl-tool calls as a client — those are
correct and must stay.

## Milestones

| Milestone | Theme | Exit checkpoint |
|---|---|---|
| **M0** | Foundations: repo, CI, config, store, auth, health, SSE skeleton, image and compose stack | `docker compose up -d` serves the app on `${DLTOOL_PORT:-8091}`; `/healthz` returns `{"status":"ok"}`; CI green. The rendered login page arrives with M3 |
| **M1** | Task core and the aria2 engine | A pasted HTTPS URL downloads to `/data`, progress streams over SSE, pause/resume/remove work |
| **M2** | BitTorrent via qBittorrent | A magnet added with a file-selection step downloads and seeds; per-file priorities apply |
| **M3** | Web UI | The full Download-Station-equivalent screen works in a browser; Playwright E2E green |
| **M4** | Search | The bundled engines return results; a result becomes a task in one click |
| **M5** | RSS | A rule auto-downloads the Arch Linux release feed; the dry run explains every non-match |
| **M6** | Post-processing, scheduling, multi-user | Auto-extract, the 24×7 grid, and per-user destinations and quotas all work end to end |
| **M7** | Media downloads, packaging, release | yt-dlp works; a signed multi-arch image is published |

## Decisions referenced
| ADR | Decision |
|---|---|
| [0016](decisions/0016-relicense-to-apache-2.md) | The only `proposed` record: `LICENSE` is unchanged until the repository owner decides. |
| [0017](decisions/0017-exclusive-control-of-engines.md) | dl-tool ignores any engine transfer it did not create — why `T102` is unused. |

## Open questions
- (none)

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version. |
| 2026-09-01 | Consistency pass: added the `Decisions referenced` and `Open questions` sections required by the document template. |

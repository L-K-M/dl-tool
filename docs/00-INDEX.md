# 00 — Index

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** every other document, and any task file

## Purpose
Map the document set: what each file owns, the order to read them in, and where to go next. It is the only
place that records which fact lives where; it restates no fact of its own.

## Scope of this document
- In scope: the file map, the single-source table, reading order, the numbering gaps, and the status table.
- Out of scope (lives instead in): every fact — see the single-source table below; the task roster →
  [`tasks/00-task-index.md`](tasks/00-task-index.md); the decision index →
  [`decisions/README.md`](decisions/README.md).

## What dl-tool is
A self-hosted download manager that reproduces Synology Download Station's user experience on hardware the
operator chooses, deployed with plain `docker compose`. It is a control plane over aria2, qBittorrent and
yt-dlp ([ADR-0001](decisions/0001-control-plane-over-existing-engines.md)); it serves `/api/v1` only and
implements no third-party compatibility API.

## The document set

| File | Owns |
|---|---|
| [`01-vision-and-scope.md`](01-vision-and-scope.md) | The problem, the personas, the Download Station parity list, and what is deliberately out of scope. arc42 §1–2. |
| [`02-requirements.md`](02-requirements.md) | Every `FR-###` and `NFR-###` in EARS notation, with a verification statement and a covering task. |
| [`03-architecture.md`](03-architecture.md) | Processes, Go packages, runtime flows and cross-cutting concepts. arc42 §3–8 and §11. |
| [`04-data-model.md`](04-data-model.md) | The SQLite schema: DDL, indices, enum vocabularies, migration policy, backup and retention. |
| [`05-api-contract.md`](05-api-contract.md) | Every HTTP path, query parameter, request body, response body, status code and error slug. |
| [`06-download-engines.md`](06-download-engines.md) | The Go `Engine` interface, URI routing and normalisation, engine ownership and conformance, bandwidth precedence, and each adapter's wire protocol. |
| [`07-search-and-indexers.md`](07-search-and-indexers.md) | The Torznab client, the `dlsearch/v1` YAML schema, the `.dlm` import path, and the bundled engines. |
| [`08-rss-automation.md`](08-rss-automation.md) | Feed polling, URI extraction, the rule document, the matching algorithm, dedup and dry-run. |
| [`09-web-ui-spec.md`](09-web-ui-spec.md) | Every screen, grid column, dialog field and detail tab, plus theming, i18n and accessibility rules. |
| [`10-deployment-and-compose.md`](10-deployment-and-compose.md) | Service names, ports, volumes, compose profiles, image builds, the release pipeline, reverse proxy and VPN. |
| [`11-config-reference.md`](11-config-reference.md) | Every environment variable and database-backed setting, and the boot validation rules. |
| [`12-security-and-threat-model.md`](12-security-and-threat-model.md) | Trust boundaries, SSRF, path safety, extraction safety, untrusted-definition parsing, and the incidents that justify each control. |
| [`13-testing-and-verification.md`](13-testing-and-verification.md) | The test layers, the Makefile targets every task verifies with, and the Definition of Done. |
| [`14-conventions.md`](14-conventions.md) | Repository layout, naming, the error model, logging, and Git conventions. |
| [`16-prior-art-and-research.md`](16-prior-art-and-research.md) | The evidence record: the landscape survey, the parity matrix, and the primary source behind each claim. |
| [`17-operations-and-runbook.md`](17-operations-and-runbook.md) | Boot and shutdown, refusal-to-start conditions, backup and restore, upgrade and rollback, diagnostics, and the symptom table. |
| [`glossary.md`](glossary.md) | Every term of art, defined once. |
| [`decisions/`](decisions/) | Every *why*. MADR-minimal records plus a mandatory `## Confirmation` section. |
| [`tasks/`](tasks/) | One file per implementable task, each self-contained. |

## Single-source rule
Each fact has exactly one home. Every other document links to it and never restates it. A short recap
sentence plus a link is fine; a copied table is a defect.

| Fact | Home |
|---|---|
| Environment variables | [`11-config-reference.md`](11-config-reference.md) |
| HTTP request/response shapes, status codes, error slugs | [`05-api-contract.md`](05-api-contract.md) |
| Table columns, DDL, enums | [`04-data-model.md`](04-data-model.md) |
| Ports, volumes, compose service names | [`10-deployment-and-compose.md`](10-deployment-and-compose.md) |
| The `Engine` Go interface | [`06-download-engines.md`](06-download-engines.md) |
| The `dlsearch/v1` YAML schema | [`07-search-and-indexers.md`](07-search-and-indexers.md) |
| The RSS rule schema and matching algorithm | [`08-rss-automation.md`](08-rss-automation.md) |
| Grid columns, dialog fields | [`09-web-ui-spec.md`](09-web-ui-spec.md) |
| Definition of Done, Makefile targets | [`13-testing-and-verification.md`](13-testing-and-verification.md) |
| Requirement text | [`02-requirements.md`](02-requirements.md) |
| Any *why* | [`decisions/`](decisions/) |
| Term definitions | [`glossary.md`](glossary.md) |

## Reading order

| If you are | Read, in this order |
|---|---|
| New to the plan | `01` → `03` → [`decisions/README.md`](decisions/README.md) → `glossary.md` |
| Implementing a task | The task file, then only the sections it names under `## Context you need` |
| Adding an endpoint | `05` → `04` → `02` → [`14-conventions.md`](14-conventions.md) |
| Adding an engine adapter | `06` → `04` → [`13-testing-and-verification.md`](13-testing-and-verification.md) §4 |
| Building a screen | `09` → `05` → `02` |
| Deploying or operating | `10` → `11` → `17` |
| Reviewing security | `12` → `07` → `06` |

## What to do next
1. Read [`tasks/00-task-index.md`](tasks/00-task-index.md).
2. Take the lowest-numbered task in the earliest milestone whose `Depends on` tasks are all `done`.
   **M0 is blocking** — nothing else starts until every M0 row is `done`.
3. Read only the documents that task's `## Context you need` names. Do not explore the rest of the repo.
4. Close the task against the Definition of Done in
   [`13-testing-and-verification.md`](13-testing-and-verification.md), and set its row in
   [`tasks/00-task-index.md`](tasks/00-task-index.md) to `done` in the same commit.

## Numbering gaps — deliberate and permanent
Identifiers are never reused. Three gaps exist and are expected:

| Gap | Why |
|---|---|
| `docs/15-*.md` | The slot held the compatibility-API document, then the migration document. Both were cut from the product; the number is retired rather than reused. A link to any `docs/15-*.md` is a bug. |
| `decisions/0014-*.md` | Opt-in qBittorrent and Synology compatibility façades — withdrawn before it was written. |
| Tasks `T102`, `T112`, `T114` | Foreign-task policy, façade authentication mapping and the migration importers — all withdrawn. |

There is no `docs/18-*.md`. The operations runbook is `17-operations-and-runbook.md`.

## Status
| Area | File | Status |
|---|---|---|
| Vision and scope | `01` | draft |
| Requirements | `02` | draft |
| Architecture | `03` | draft |
| Data model | `04` | draft |
| API contract | `05` | draft |
| Download engines | `06` | draft |
| Search and indexers | `07` | draft |
| RSS automation | `08` | draft |
| Web UI | `09` | draft |
| Deployment | `10` | draft |
| Configuration | `11` | draft |
| Security | `12` | draft |
| Testing | `13` | draft |
| Conventions | `14` | draft |
| Prior art | `16` | draft |
| Operations | `17` | draft |
| Glossary | `glossary.md` | draft |
| Decisions | `decisions/` | 0001–0013, 0015–0018 accepted; 0016 proposed |
| Tasks | `tasks/` | 112 files, all `todo` |

## Decisions referenced
| ADR | Decision |
|---|---|
| [0001](decisions/0001-control-plane-over-existing-engines.md) | Build a control plane over existing download engines |
| [0016](decisions/0016-relicense-to-apache-2.md) | Relicense from the Unlicense to Apache-2.0 — the only `proposed` record |

## Open questions
- (none)

## Change log
| Date | Change |
|---|---|
| 2026-09-01 | Initial version: document map, single-source table, reading order and the permanent numbering gaps. |
| 2026-09-01 | Consistency review: step 4 of "What to do next" now names `tasks/00-task-index.md` as the file whose row is set to `done`. |

# Decision records

> **Status:** stable
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent

Every architectural *why* lives here. No other document re-argues a decision; it links to the ADR instead.
Format is MADR-minimal with a mandatory `## Confirmation` section — see
[`0000-adr-template.md`](0000-adr-template.md).

## Index

| ID | Decision | Status |
|---|---|---|
| 0001 | [Build a control plane over existing download engines](0001-control-plane-over-existing-engines.md) | accepted |
| 0002 | [Go for the backend](0002-go-for-the-backend.md) | accepted |
| 0003 | [chi + Huma with code-first OpenAPI](0003-chi-huma-code-first-openapi.md) | accepted |
| 0004 | [SQLite as the only datastore](0004-sqlite-as-the-only-datastore.md) | accepted |
| 0005 | [aria2, qBittorrent and yt-dlp as the v1 engines](0005-aria2-qbittorrent-ytdlp-engines.md) | accepted |
| 0006 | [Server-sent events with rid deltas for live updates](0006-sse-with-rid-deltas.md) | accepted |
| 0007 | [React SPA embedded in the Go binary](0007-react-spa-embedded-in-the-binary.md) | accepted |
| 0008 | [Torznab first, declarative YAML engines second](0008-torznab-first-declarative-yaml-second.md) | accepted |
| 0009 | [A native cross-protocol RSS rule engine](0009-native-cross-protocol-rss-rules.md) | accepted |
| 0010 | [Never execute third-party definition code](0010-never-execute-third-party-definitions.md) | accepted |
| 0011 | [Alpine runtime image with PUID/PGID privilege drop](0011-alpine-runtime-with-puid-pgid.md) | accepted |
| 0012 | [A single `/data` mount](0012-single-data-mount.md) | accepted |
| 0013 | [Mandatory built-in authentication](0013-mandatory-built-in-authentication.md) | accepted |
| 0014 | — *never written* | **withdrawn** |
| 0015 | [DB-backed in-process job queue](0015-db-backed-in-process-job-queue.md) | accepted |
| 0016 | [Relicense from the Unlicense to Apache-2.0](0016-relicense-to-apache-2.md) | **proposed** |
| 0017 | [dl-tool assumes exclusive control of its engines](0017-exclusive-control-of-engines.md) | accepted |
| 0018 | [Pin yt-dlp by version and hash; never self-update at runtime](0018-pin-ytdlp-by-version-and-hash.md) | accepted |

## Rules

- **IDs are never reused.** 0014 was withdrawn when the compatibility façades were cut from the product,
  and its number stays permanently unused. dl-tool serves `/api/v1` only; it never imitates another
  product's API.
- Filenames are fixed. They are listed above verbatim; a link that invents a variant slug is a bug.
- Never link to a heading inside an ADR — link to the file.
- 0016 is the only `proposed` record. `LICENSE` is unchanged until the repository owner decides.
- An accepted ADR is not edited to change its outcome. Write a new ADR with the next free ID and set the
  old one to `superseded by [ADR-NNNN](NNNN-slug.md)`.

## How to add an ADR

1. Take the next free ID from the table above and add its row before writing anything else.
2. Copy [`0000-adr-template.md`](0000-adr-template.md). Its headings are mandatory and their order is fixed.
3. Keep the file between 40 and 110 lines, in reference mode: tables, exact identifiers, no hedging.
4. `## Considered Options` must list real alternatives with their real trade-offs, including the evidence
   that points the other way. A strawman option makes the record worthless.
5. `### Confirmation` must name something executable — a `make` target, a named test, a CI job or a `grep`
   with its expected output. "Code review" is not a confirmation.
6. Link the new ADR from every document that depends on it, and add it to that document's
   `## Decisions referenced` table.

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version: index of 0001–0018, 0014 recorded as withdrawn and permanently unused. |

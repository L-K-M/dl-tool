# 0017 - dl-tool assumes exclusive control of its engines

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

dl-tool drives aria2, qBittorrent and yt-dlp as back ends ([ADR-0001](0001-control-plane-over-existing-engines.md)),
but those daemons have their own web UIs, RSS rule engines, bandwidth schedulers and automatic file
relocation. Nothing stops an operator adding a torrent directly in qBittorrent, and nothing stops
Automatic Torrent Management moving files by category behind dl-tool's back. Two questions need one answer:
what happens to a transfer dl-tool did not create, and what happens when the engine's automation competes.

## Decision Drivers

- Every dl-tool guarantee is stated over its own rows: the quota sums the user's tasks, the concurrency
  limits count active tasks, the schedule fans out to tasks. A transfer with no row has no owner and no
  destination policy, so every one of those computations must special-case it. Two RSS engines against one
  feed also grab the same release twice, and two schedulers fight over one rate limit.
- The implementing agent is a weaker model: a one-branch rule is implemented correctly far more often than a
  configurable policy with two sets of semantics. And deleting the wrong thing is unrecoverable — any rule
  letting dl-tool act on a transfer it did not create lets it delete data it never recorded.

## Considered Options

- **Option A** — Exclusive control: a transfer dl-tool did not create is ignored entirely, with no setting.
- **Option B** — Shared and cooperative: foreign transfers are listed read-only, count towards nothing and
  are never modified, so dl-tool coexists with an operator who also uses the qBittorrent web UI.
- **Option C** — Adopt everything: create a dl-tool task for every transfer the engine holds and does not
  know, assigning each to the admin who configured that engine.

## Decision Outcome

Chosen option: **Option A, exclusive control with a hard ignore**, because it is the only rule with a single
branch: a transfer is dl-tool's or it is invisible. That removes the "who owns an adopted task" question
from the quota model, and removes any path in which dl-tool pauses, relocates or deletes something it did
not create. There is **no adopt mode**, no `foreign_task_policy` setting and no column.

Detection is by handle: a transfer is foreign when its `engine_ref` — the aria2 GID, the qBittorrent hash,
the yt-dlp job id — matches no `tasks` row for that engine. The filter runs at `Connect()` and on every
`sync/maindata` full update or full `tellActive`/`tellWaiting`/`tellStopped` sweep; it only filters, never
creates. Behaviour table: [`../06-download-engines.md`](../06-download-engines.md); requirement
[FR-148](../02-requirements.md#fr-148-ignore-engine-tasks-dl-tool-did-not-create).

### Consequences

- Good, because a foreign transfer is absent from `GET /tasks`, every SSE delta and every count, so no
  listing, quota or concurrency computation needs a special case. dl-tool never deletes one and never
  unlinks a file absent from `task_files`, keeping the irreversible operation inside data it owns.
- **Because exclusive control is claimed, it is also enforced.** At `Connect()` every adapter asserts the
  engine's competing automation is off and forces it off where the API allows: qBittorrent
  `rss_processing_enabled=false` (two rule engines double-grab one feed), `scheduler_enabled=false` (two
  schedulers fight over one rate limit), `auto_tmm_enabled=false` (Automatic Torrent Management relocates by
  category and would override `tasks.destination`), and no search plugins installed. Read with
  `GET /api/v2/app/preferences`, fix with `POST /api/v2/app/setPreferences` carrying only the changed keys;
  `torrents/add` also sends `autoTMM=false` so a torrent cannot inherit ATM from a category later.
- Good, because a conformance failure is a visible warning naming the failed key with a "fix it for me"
  action, never a crash ([FR-147](../02-requirements.md#fr-147-assert-engine-conformance-at-boot)).
- Bad, because an operator with an existing qBittorrent sees an empty dl-tool queue and there is no importer
  — they re-add what they want, and the "one queue" claim is true only of tasks dl-tool created.
- Neutral, because search plugins are warned about but never uninstalled: preferences change, software does not.

### Confirmation

Both halves are integration-tested against a real qBittorrent by tasks T101 and T102:

```bash
make test-integration
grep -rn "foreign_task_policy\|adopt" --include='*.go' --include='*.sql' cmd internal
```

Expected: exit 0 from `make test-integration`, whose contract suite adds a torrent directly in qBittorrent
and asserts it never appears in `GET /tasks`, in an SSE delta or in the counts and that its engine state is
unchanged after a full poll cycle; and which starts qBittorrent with ATM enabled, asserts dl-tool boots and
the warning names `auto_tmm_enabled`, applies the correction and asserts the preference is then false.
`grep` prints nothing and exits 1.

## Pros and Cons of the Options

### Option A - exclusive control, foreign transfers ignored

- Good, because it is one rule with no configuration: no settings to test, no way to select the dangerous
  variant, and the engine becomes a private detail whose preferences dl-tool may assert.
- Bad, because "point dl-tool at the qBittorrent you already run" becomes lossy: it works, but existing
  torrents stay invisible while still consuming bandwidth off-book.

### Option B - shared and cooperative, foreign transfers shown read-only

- Good, because it is honest about a shared daemon and matches how an operator with an existing stack works.
- Bad, because "read-only" is a promise every control must keep: every bulk action, select-all and schedule
  fan-out needs the exemption, and one missed check pauses a stranger's torrent.
- Bad, because it cannot answer the quota question: either foreign bytes count against somebody, which is
  arbitrary, or the quota silently under-reports what is on disk.

### Option C - adopt every foreign transfer

- Good, because the queue matches reality and an operator arriving from a bare qBittorrent keeps their work.
- Bad, because ownership is a guess. Assigning to "the admin who configured the engine" charges one user's
  quota for everyone's downloads, and there is no evidence in the engine to do better.
- Bad, because it makes dl-tool responsible for data it never wrote: `delete_data` on an adopted task would
  unlink files no `task_files` row describes.

## More Information

- Research: `engines.md` §2.9, §9.2 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../06-download-engines.md`](../06-download-engines.md),
  [`../03-architecture.md`](../03-architecture.md), [`../01-vision-and-scope.md`](../01-vision-and-scope.md).
- The RSS engine this keeps single is [ADR-0009](0009-native-cross-protocol-rss-rules.md).

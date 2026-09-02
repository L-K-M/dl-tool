# Review of the dl-tool implementation plan

> **Reviewed:** 2026-09-02 · **Reviewing:** the plan at commit `57d37de`, 122 task files (numbered
> non-contiguously to T125; T102, T112 and T114 are permanently retired) and 20 plan documents ·
> **Full findings:** [`PLAN-REVIEW-FINDINGS.md`](PLAN-REVIEW-FINDINGS.md)

## Verdict

The plan is strong in the ways plans usually fail and weak in one way it did not anticipate.

Its form is excellent. 122 task files with a clean dependency graph, no cycles, no forward references
across milestones, every one of the 124 requirements claimed by a task, 2203 internal links resolving,
and its own `make doclint` passing. The research behind it is real: the qBittorrent 5.x
`paused`→`stopped` rename, the stale WebAPI version constant, the CVE-derived extraction rules and the
`aria2.changeOption` restart list are all things most plans get wrong, and this one gets right. Several
of its sharpest calls — sending both spellings of the pause parameter, keying login success on the
`Set-Cookie` header rather than the status code, never executing third-party definition code — I
confirmed against live daemons.

The weakness is that **the plan is not yet executable end to end**. 135 defects (25 critical, 110 high)
survived an adversarial verification pass. They are not scattered: six recurring shapes account for most
of them, and the first three of those trace back to a single root cause — the hard rule that a task may
only touch files in its own `Files` table, with no task owning the seams between them.

Concretely, following the plan literally today:

- `docker compose up -d` from the shipped `.env.example` **exits before any container starts**, and the
  M0 exit checkpoint cannot be reached (F005, F006, F028).
- Every SPA request goes to `/api/v1/api/v1/…` and 404s, while `make typecheck` passes (F001).
- `internal/store` **does not compile** at T017, and `internal/engine` does not compile at T079 —
  duplicate declarations across a task boundary (F013, F020).
- The aria2 client is never constructed, the reconciler is never started, the SSE hub loop never runs,
  the job handlers are never registered, and the search registry is never built (F008, F052, F078,
  F087, F133, F162).
- No legal state transition reaches `completed`, so the M1 exit checkpoint — a pasted HTTPS URL that
  downloads — cannot complete (F004, F096).
- Every qBittorrent task is invisible to dl-tool and gets marked `error` on the first sweep, so the M2
  checkpoint is unreachable (F023).
- Behind the reverse-proxy snippets the plan itself ships, **every request answers 421** — the Host
  allowlist has no configuration source anywhere in the plan (F602, F142).

None of this is fatal to the design. All 135 are fixable in the plan documents, most in a line or two,
and the fixes are listed with each finding. But they should be fixed **before** T001 starts, because the
plan's own process — read only this task file, touch only these files, stop and write `## Blocked`
otherwise — converts each of these into a stalled agent rather than a small correction.

## What I did

| Step | Detail |
|---|---|
| Mechanical checks | Parsed all 122 task files and the index; verified the dependency graph, file ownership, requirement coverage, parallel-safety claims, template conformance; ran the plan's own `scripts/doclint.sh` with `lychee` installed |
| Live verification | Ran the plan's claims against **yt-dlp 2026.08.19**, **qBittorrent 5.2.3 / WebAPI 2.15.1** (`lscr.io/linuxserver/qbittorrent`), **aria2 1.37.0** and **Alpine 7-Zip 26.01** in containers; resolved every Go and npm version pin against its registry; read the pinned Huma, goose and modernc-sqlite sources |
| Review | 24 reviewers, each assigned a lens (one per engine, subsystem, milestone batch, plus completeness and process critics) and told to read its files in full and report only defects that would block or mislead an implementing agent |
| Verification | Every critical and high finding went to an independent verifier whose instruction was to **refute** it; 28 were refuted (mostly duplicates), 135 survived |

Medium and low findings (505) were de-duplicated but not individually re-verified; they are listed
separately as leads rather than established defects.

## The six patterns

These are recurring shapes, not a partition of the 135 findings: each section names the confirmed
findings that illustrate it, and defects that fit no shape are listed only in
[`PLAN-REVIEW-FINDINGS.md`](PLAN-REVIEW-FINDINGS.md), which is the authoritative per-finding list. Where
a fix below is described as fixing "the class", it addresses patterns 1 to 3; patterns 4 to 6 have to be
worked finding by finding.

### 1. Nothing wires the components together

The plan specifies components precisely and then never constructs them. `engine.Registry` is defined
(T019) but no task calls `NewRegistry` or `aria2.New` (F008). `Reconciler.Boot`/`Run` exist but nothing
starts them (F162). `Hub.Loop` is never started and never given a snapshot source (F052). The `rss_poll`
handler is never registered and the `Scheduler` never started (F133). The extract, move and notify job
handlers are never registered on the worker (F087). The search `Registry`, `Runner` and `IndexerStore`
are never built (F078). `SetOwnershipFilter` is never called, so every qBittorrent torrent is invisible
(F023).

This is a direct consequence of hard rule 1. `cmd/dl-tool/main.go` and `internal/api/server.go` are the
composition roots, and a task may only edit them if they are in its own `Files` table — so the tasks
that create a component are structurally unable to plug it in, and no task exists whose job is to do so.

> **Fix the class, not the instances.** Add a mandatory `## Wiring` section to the task template that
> names the composition-root file the task must edit and the exact call it must add, and put that file
> in the `Files` table. Where several tasks in a milestone each need one line in `server.go`, an explicit
> per-milestone wiring task is cleaner than eleven concurrent edits to the same file.

### 2. Generated artefacts are gated in CI but owned by nobody

`api/openapi.json` and `web/src/api/schema.d.ts` are committed, and `gen-drift` is a blocking CI job
(T002). Only T007 and T014 list them in a `Files` table. Every later task that registers a Huma
operation — T009, T020, T021, T022, T023, T024, T027 and roughly thirty more — therefore invalidates the
committed contract, and rule 1 forbids the task from regenerating it (F017, F048, F067). By M3 the
committed `schema.d.ts` still describes only `/system/info`, so `paths['/tasks/{id}']` is a type error
and `make typecheck` cannot pass. An agent that obeys rule 1 is blocked; one that regenerates breaks
rule 1 and Definition-of-Done item 8.

> State once, in doc 13 §7.1 and doc 14 §7, that the two generated files are implicitly part of the
> `Files` table of any task that changes a Huma operation, and add `make gen` to those tasks' Steps.

### 3. The one-file-per-task partition does not check the Go package namespace

`store.ErrNotFound` is declared by T006 in `db.go` and again by T017 in `tasks.go` — same package,
`redeclared in this block`, and T017 may not edit `db.go` to remove the first (F013). `engine.Limits` is
declared by T098 in `admission.go` and again by T079 in `bandwidth.go` (F020). Both are compile errors
that a literal implementer cannot fix without breaking rule 1.

> A cheap guard: before implementation starts, grep every task file's interface contract for `^type ` and
> `^var ` / `^func ` at package scope and assert no identifier is declared twice within one Go package.

### 4. Authoritative documents contradict each other on facts an implementer must act on

The plan's "each fact has exactly one home" rule is well designed but not enforced, and the same fact
ended up in two homes with different values:

| Fact | One document says | The other says |
|---|---|---|
| Non-writable data root (F007, F016) | doc 10 §4 / doc 17 §1.4 / T124: warn and keep running | doc 11 §8 / T005: fatal, exit 1 |
| A task whose engine handle vanished (F010) | doc 17 §1.6: re-submit with resume semantics | T026 / NFR-003: mark `error` |
| `DELETE /tasks/{id}` (F002, F041) | doc 04: `removed` is a retained soft-delete | doc 05 §5.6 / T023: hard-delete the row |
| Post-processing entry state (F004) | doc 03 / T017: from `downloading` | T074 / T076: from `completed` |
| RSS poll interval (F132) | doc 08: default 1800 s, floor 300 s | doc 11 / T092 / T117: 900 s, floor 60 s |
| Feed body cap (F085) | doc 08 / T066: 16 MiB, raw `LimitReader` | doc 12 §2.4 / T123: 8 MiB via `secure.ReadCapped` |

A related sub-pattern: columns the API contract and the tasks both depend on are absent from the DDL,
and no task adds a migration. `tasks.requested_destination` (F044) and
`notification_channels.last_send_at` / `last_error` (F624) are each read by two or more tasks and
returned by documented endpoints, but neither exists in doc 04 §3 — so the queries fail at runtime and
doc 14 §8.3's "a schema change needs a migration plus a doc 04 edit" is unsatisfiable inside the owning
task's `Files` table.

The state machine is the worst case. T017's transition table forbids `downloading → completed`,
`paused → seeding`, `queued → seeding`, `checking → seeding` and `completed → seeding` — every one of
which the engine normalisation tables in doc 06 routinely produce (F096). An aria2 HTTP download that
finishes hits `ErrIllegalTransition` on every poll tick, and there is no path to `completed` at all
(F004). That single table blocks the M1 exit checkpoint.

> Extend the transition table with the engine-driven edges and state that the reconciler may adopt any
> state an engine reports; then re-derive doc 03 §8.1 from it rather than maintaining both.

### 5. The default deployment does not boot

Three independent causes, any one of which is enough:

1. `.env.example` ships `ARIA2_RPC_SECRET=` empty and `compose.yaml` uses the `:?` form on it. Compose
   interpolates the whole file **before** applying profiles, and `:?` fails on empty as well as unset, so
   `docker compose up` aborts at interpolation even though the `aria2` profile is off (F005).
2. Even with that fixed, `DLTOOL_ARIA2_URL` is hard-coded in the always-on service while the secret is
   empty, which doc 11 §8 makes a fatal `config_missing` (F028).
3. The same shape applies to `QBT_PASSWORD` — and here it cannot be pre-filled at all, because
   linuxserver/qbittorrent generates a random temporary password on first start and prints it to its own
   log. No document owns how the operator gets that value into `.env` (F006, F153). I confirmed the
   behaviour live: `A temporary password is provided for this session: …`.

The plan is aware of the tension — T125's own verification exports `ARIA2_RPC_SECRET=checkonly` to get
past it — but the shipped default remains unbootable, and the M0 exit checkpoint is written as
`cp .env.example .env && docker compose … up -d`.

> Make each engine lane follow its profile: `DLTOOL_ARIA2_URL: "${DLTOOL_ARIA2_URL:-}"` (doc 11 already
> says an empty URL disables the lane), and give the qBittorrent credential a documented first-run
> procedure or a generated password in the compose file.

### 6. External-software details that do not survive contact

The engine research is unusually good, so the remaining errors are concentrated where the plan itself
flagged uncertainty — but they are load-bearing. I verified each against the real tool:

| Claim | Reality |
|---|---|
| A yt-dlp flag enumerates extractor URL patterns, so `Accepts()` can be an offline regexp match (doc 06 §7.2, T088) | **No such flag exists.** `--list-extractors` prints names only. T088's design is not implementable as written. |
| The `--progress-template` line in doc 06 §7.3 emits line-delimited JSON (T087, T089) | **Every line is invalid JSON.** Unknown numerics render as the literal `NA`: `"speed":NA,"eta":NA`. 9 of 9 lines failed to parse. Adding `\|null` defaults fixes it — tested, 9 of 9 valid. |
| yt-dlp extractor patterns can be compiled with Go `regexp` (T088) | Python `re` dialect — `(?x)`, lookarounds, conditionals. Go's RE2 rejects them, so the "drop on compile failure" rule silently discards most extractors (F169). |
| `7zz` extracts `.rar` (T074 acceptance criterion) | **Alpine builds 7-Zip without the RAR codec.** RAR is absent from `7zz i`; a RAR header gives `Cannot open the file as archive`. |
| A wrong password is distinguishable from a corrupt archive by `7zz` exit status (T075) | Both exit **2**. Only stderr differs: `Cannot open encrypted archive. Wrong password?` |
| A `.tgz` extracts in one pass (T074) | `7zz x foo.tgz` yields `foo.tar`; a second pass is required (F128). |
| The aria2 compose healthcheck (doc 10 §3) | Posts `getVersion` with empty `params` against a daemon started with `--rpc-secret`: **HTTP 400**, `curl -f` exits 22, container permanently unhealthy. |
| `aria2.remove` then `removeDownloadResult` (T019) | `aria2.remove` **errors** on an already-stopped download (`Active Download not found for GID#…`); the adapter must tolerate that (F164). |
| aria2 JSON-RPC error codes distinguish failures (T019 maps "not found" to `ErrNotFound`) | Every error is `code: 1`; only the message string differs. |
| `testcontainers.DaemonHost` for host access from the container (T028) | Not a function in the pinned API, and the direction is inverted (F061). |
| The image can run yt-dlp merges | No `ffmpeg` in the package list, so media downloads silently cap at pre-merged formats (F167). |

Six further external claims I checked came back **correct** and are worth keeping as written: the
qBittorrent 5.2 login `204`/`401` split, sending both `stopped` and `paused`, `torrents/pause` returning
404 on 5.x, `shareLimitAction` being required on 5.2.3, the `-2` sentinel for "use global", and
`webapiVersion` returning `2.15.1`.

## Two design issues worth deciding, not just fixing

**Rules have no owner, and grabs run as an unspecified admin (F021).** Any authenticated user can create
an RSS rule with any `action.destination`; the `rules` table has no `owner_id`, so the grab must be
attributed to some admin — which bypasses the per-user destination jail (FR-124) and the storage quota
by construction. On the poll path there is no request identity at all, so `IdentityFrom(ctx)` fails and
every automatic grab 401s. This needs a schema column and a decision about whose jail a rule's
destination is validated against, not a patch.

**Admission control is bypassed by design (F003).** T020 hands every new task to its engine at creation
and T022 resumes directly at the engine, while T098 is written as the sole releaser of queued work. As
written, `max_active_*` (FR-020, FR-021, FR-123) is never enforced for newly created tasks, and T098
later calls `Resume` on downloads that were never paused. Making T098 the only caller of `Engine.Add`
and `Engine.Resume` is a small change to T020, T022 and doc 03 §6.1 — but it is a change to the
architecture's control flow, so it should be decided deliberately.

## Recommended order of work

1. **Unblock the boot path** — F005, F006, F028, F007/F016, then F001. Without these the M0 checkpoint
   is unreachable, and every later milestone's verification runs through it.
2. **Fix the two compile errors and the state machine** — F013, F020, F004/F096. These are single-table
   or single-declaration edits that unblock M1 and M6.
3. **Add the wiring** — the pattern-1 defects, ideally by amending the task template so the class cannot
   recur, then sweeping the ten instances.
4. **Settle the generated-artefact rule** — F017, one paragraph in doc 13 plus `make gen` in ~30 tasks'
   Steps.
5. **Resolve the document contradictions** — the table in pattern 4; each has an obvious owner under the
   plan's own fact-ownership rules.
6. **Correct the external-software claims** — the table in pattern 6. Every one has a tested replacement
   in the findings file.
7. **Decide the two design issues** — rule ownership and admission control — as ADRs, since both change
   behaviour the plan states elsewhere.

Items 1 and 2 are eleven edits and would move the plan from "cannot start" to "starts and reaches M1".

## What this review did not cover

- Medium and low findings were not individually verified. 505 of them are listed in part 2 of the
  findings file as leads.
- Reviewer effort was not uniform. M0 and M1 drew the most attention (161 and 119 findings); M7 and the
  M6 user-management tasks the least (21 and 47). A thinner list there means less looking, not a cleaner
  plan.
- The 26 `[NEEDS CLARIFICATION]` markers and ~57 `UNVERIFIED` HTML comments already in the plan were
  treated as known open questions, not reported as findings, except where one blocks a task that is not
  marked blocked.
- No code was written or run against the plan; there is none yet. Every "live" result above came from
  running the real external tools the plan drives.

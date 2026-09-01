# T105 — Ship the curated static `linux-distributions` engine

| Field | Value |
|---|---|
| **ID** | T105 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T056, T057, T058, T062 |
| **Blocks** | — |
| **Parallel-safe** | no — extends T058's `internal/search/dlsearch.go` |
| **Implements** | [FR-052](../02-requirements.md#fr-052-ship-four-legitimate-engines-and-no-piracy-indexers), [FR-051](../02-requirements.md#fr-051-load-declarative-dlsearchv1-engines) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 0 new files, ~220 LOC plus the refreshed definition |

## Goal
`kind: static` engines answer a search from `entries[]` in process, with no HTTP request at search time.
`definitions/engines/linux-distributions.yaml` carries re-probed Ubuntu and Debian torrent URLs, and
`POST /indexers/{id}/test` on it validates the definition and reports each `download` URL that no longer
resolves.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §3.8 Worked example — `kind: static`](../07-search-and-indexers.md#38-worked-example--kind-static)
   — the `entries[]` record table, the 500-entry cap, the probe behaviour and why this engine is not a scraper.
2. [`docs/07-search-and-indexers.md` §3.9 Refreshing the curated list](../07-search-and-indexers.md#39-refreshing-the-curated-list)
   — the four refresh steps and what the fixture test must assert.
3. [`docs/07-search-and-indexers.md` §6.2 The four bundled engines](../07-search-and-indexers.md#62-the-four-bundled-engines)
   — the two directory indexes verified on 2026-09-01 and the `UNVERIFIED` marker on the three URLs.
4. [`docs/tasks/T058-dlsearch-runner-and-probe.md`](T058-dlsearch-runner-and-probe.md) — `Runner.Search`,
   `Runner.Probe` and `ProbeResult`.
5. [`docs/tasks/T056-dlsearch-definition-loader.md`](T056-dlsearch-definition-loader.md) — `Entry` and the
   static validation rules already enforced at load time.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/dlsearch.go` | modify | The `kind: static` search path and the static branch of `Probe`. |
| `internal/search/dlsearch_test.go` | modify | Filtering, the definition fixture test and the probe report. |
| `definitions/engines/linux-distributions.yaml` | modify | Re-probed `entries[]` and a bumped `version`. |

No other file may be modified.

## Interface contract

```go
package search

// searchStatic answers from def.Entries with no HTTP request. Rows are filtered by
// case-insensitive substring of q.Q against Entry.Title, so every static engine is
// browse-style in the UI. A missing size stays 0, which the UI renders as an em dash,
// and Seeders and Leechers are always nil.
func searchStatic(def *Definition, q Query) []SearchResult

// probeStatic validates the definition, then issues one HEAD per distinct download URL
// through the guarded client, with the same 15 s total deadline as a live engine.
// Ok is true only when every URL answered 2xx; Error names each URL that did not, with
// its status. It follows the same redirect and port rules as every other fetch.
func (r *Runner) probeStatic(ctx context.Context, def *Definition) (ProbeResult, error)
```

Every entry maps to `SearchResult` as follows; there are no templates and no transforms in a static
definition:

| `entries[]` field | `SearchResult` field | Rule |
|---|---|---|
| `title` | `Title` | verbatim, and the keyword filter matches against it |
| `download` | `DownloadURL` | `https` only, re-checked against the SSRF rules at grab time |
| `magnet` | `MagnetURI` | `magnet:` scheme only |
| `infohash` | `Infohash` | 40 or 64 lowercase hex |
| `size` | `SizeBytes` | absent or `0` means unknown |
| `category` | `CategoryIDs` | mapped through `caps.categories`; `CategoryDesc` keeps the site value |
| `details` | `DetailsURL` | `https` only |
| `published` | `PublishedAt` | coerced with the declared `format`, else nil |

## Steps
1. This task needs outbound HTTPS to `releases.ubuntu.com` and `cdimage.debian.org`. If the sandbox has no
   network, STOP and write that under `## Blocked`: do **not** invent, guess or reuse a stale URL, and do not
   fall back to whatever T057 shipped. Re-probe the two directory indexes named in doc 07 §6.2 and read the
   current filenames:
   `curl -sS https://releases.ubuntu.com/24.04/ | grep -o '[^"]*\.iso\.torrent'` and
   `curl -sS https://cdimage.debian.org/debian-cd/current/amd64/bt-cd/ | grep -o '[^"]*\.iso\.torrent'`.
2. Fetch each candidate `.torrent` with `curl -sI` and keep only those answering `200` with
   `content-type: application/x-bittorrent`; paste both command outputs under `## Evidence`, which is what
   clears the `UNVERIFIED` marker doc 07 §3.8 carries.
3. Replace `entries[]` in `definitions/engines/linux-distributions.yaml` with the confirmed URLs, keep
   `size: 0`, keep `category: iso`, and bump `version` by one semver minor.
4. Add `searchStatic` to `internal/search/dlsearch.go` and dispatch to it from `Runner.Search` before any
   request is built, so a `static` engine never opens a socket during a search.
5. Map every entry with the table above, defaulting `DownloadVolumeFactor` and `UploadVolumeFactor` to `1.0`,
   then run the rows through `Finalise` so an entry with no acquisition handle is dropped and counted.
6. Add `probeStatic` and dispatch to it from `Runner.Probe`, de-duplicating the URL list first and reporting
   every non-2xx URL with its status in `ProbeResult.Error`.
7. Extend `internal/search/dlsearch_test.go` with `TestStaticSearchMakesNoRequest` (a runner whose client
   fails every dial still returns rows), `TestStaticKeywordFilterIsCaseInsensitive`,
   `TestStaticSeedersAlwaysNull`, `TestStaticProbeReportsDeadURL` against a stub returning `404`, and
   `TestLinuxDistributionsDefinitionIsWellFormed`, which asserts every entry parses, every `download` URL is
   unique, `https` and ends in `.torrent`, and every `category` is a declared `caps.categories` key.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestStaticSearchMakesNoRequest` asserts results are returned with a client that cannot dial.
- [ ] `TestStaticSeedersAlwaysNull` asserts `Seeders` and `Leechers` are nil for every static row.
- [ ] `TestLinuxDistributionsDefinitionIsWellFormed` passes against the refreshed file, and the file's `version` is higher than the one T057 shipped.
- [ ] `TestStaticProbeReportsDeadURL` asserts `ok:false` with the failing URL and its status in `error`.
- [ ] The definition still declares `kind: static` with a `refresh_note`, and contains no `request` or `response` block.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/search/... && echo STATIC_ENGINE_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, every test named in step 7 listed as passing, and
the final line of stdout exactly `STATIC_ENGINE_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT turn this engine into an HTML directory-index scraper; a scraper as a default contradicts the `.dlm`
  post-mortem and is deferred to v2 with `request.paths[]`.
- Do NOT add a scheduled job that refreshes `entries[]`; the refresh is a maintenance step in the repository.
- Do NOT add a fifth bundled definition, and do NOT add a distribution whose `.torrent` URL you could not
  confirm in step 2.
- Do NOT fabricate a size, seeder count or publication date for an entry; unknown stays unknown.
- Do NOT download the `.torrent` bodies at search time; the URL becomes a task through `POST /tasks`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

# T016 — Define the Engine interface and the protocol router

| Field | Value |
|---|---|
| **ID** | T016 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T004, T015 |
| **Blocks** | T017, T018, T019, T020 |
| **Parallel-safe** | yes — touches only `internal/engine/` |
| **Implements** | [FR-002](../02-requirements.md#fr-002-route-each-uri-to-an-engine-by-scheme) |
| **Decisions** | [ADR-0001](../decisions/0001-control-plane-over-existing-engines.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`internal/engine` declares the single `Engine` interface every adapter implements, the capability and state
vocabularies, and a pure `Route` function that maps a `uri.Normalized` to one engine name by walking the
nine-row routing table in order.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
2. [`docs/06-download-engines.md` §1.1 File priority vocabulary](../06-download-engines.md#11-file-priority-vocabulary)
3. [`docs/06-download-engines.md` §2 Routing table](../06-download-engines.md#2-routing-table)
4. [`docs/14-conventions.md` §2.3 Signatures](../14-conventions.md#23-signatures)
5. [`docs/04-data-model.md` §4.1 `tasks.state`](../04-data-model.md#41-tasksstate)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/engine.go` | create | `TaskState`, `Capability`, `AddRequest`, `FileEntry`, `TaskInfo`, `TaskEvent`, `Engine`, the three sentinels. |
| `internal/engine/router.go` | create | `Route` plus the ordered routing table. |
| `internal/engine/router_test.go` | create | One table test per routing row, driven through `uri.Normalize`. |

No other file may be modified.

## Interface contract

Reproduce the `Engine` interface, the `Capability` constants, `AddRequest`, `FileEntry`, `TaskInfo`,
`EventKind`, `TaskEvent` and the three sentinel errors from
[`06-download-engines.md` §1](../06-download-engines.md#1-the-engine-interface) **byte for byte**, including
every comment. Do not rename a field, do not reorder the interface methods, do not add a method.

```go
package engine

var (
	ErrNotSupported = errors.New("engine: capability not supported")
	ErrNotFound     = errors.New("engine: task not found")
	ErrUnavailable  = errors.New("engine: daemon unreachable or session refused")
)

// ErrNoEngine is returned by Route when no row of the routing table matches. Callers map it to the
// tasks.error_code value "unsupported_scheme".
var ErrNoEngine = errors.New("engine: no engine accepts this uri")

// Names of the three v1 engines. These are the values of tasks.engine and of Engine.Name().
const (
	NameAria2       = "aria2"
	NameQBittorrent = "qbittorrent"
	NameYtDlp       = "ytdlp"
)

// Route returns the engine name for an already-normalised submission, evaluating the routing table
// of docs/06-download-engines.md §2 in order and stopping at the first match. mediaMatch reports
// whether a yt-dlp extractor claims the URL; pass nil to skip row 3.
func Route(n uri.Normalized, mediaMatch func(string) bool) (string, error)
```

Routing rows, evaluated in this order and no other:

| # | Input | Result |
|---|---|---|
| 1 | `OriginalScheme` is `thunder`, `flashget` or `qqdl` | already decoded by `uri.Normalize`; continue at row 2 with the recovered URI |
| 2 | `KindMagnet`, `KindTorrent`, or a bare 40-hex or 64-hex infohash | `NameQBittorrent` |
| 3 | `mediaMatch(n.URI)` is true | `NameYtDlp` |
| 4 | `KindHTTP` | `NameAria2` |
| 5 | `KindFTP`, `KindSFTP` | `NameAria2` |
| 6 | `KindMetalink` | `NameAria2` |
| 7 | `ed2k://` | `ErrNoEngine` — already rejected by `uri.ErrUnsupportedScheme` |
| 8 | `.nzb`, `nzb://` | `ErrNoEngine` |
| 9 | anything else | `ErrNoEngine` |

## Steps
1. Create `internal/engine/engine.go` and copy the block from
   [§1](../06-download-engines.md#1-the-engine-interface) verbatim, adding the package clause and the
   `context`, `errors` and `time` imports.
2. Add the doc comment on every exported symbol; state on `SetFiles`, `SetLocation`, `Rename`,
   `SetCategory`, `SetRateLimits` and `SetShareLimits` that an adapter without the backing `Capability`
   returns `ErrNotSupported` and mutates nothing.
3. Record in the `TaskState` doc comment that no adapter ever returns `StateExtracting` or `StateMoving`:
   those two belong to dl-tool's post-processing jobs.
4. Create `internal/engine/router.go` with `ErrNoEngine`, the three name constants and `Route`, implemented
   as a straight-line walk of rows 2 to 9 in the table above.
5. Detect a bare infohash in `Route` by length and alphabet only — exactly 40 or 64 hexadecimal characters,
   lower or upper case — and never by contacting an engine.
6. Create `internal/engine/router_test.go` with one table whose rows are the nine table rows plus a decoded
   `thunder://` HTTPS link, a decoded `flashget://` FTP link, `magnet:?xt=urn:btih:<40 hex>`, a
   `https://example.org/x.torrent` URL, a 40-hex bare infohash, `https://example.org/f.iso`,
   `ftp://example.org/f.iso`, `sftp://example.org/f.iso`, `https://example.org/f.metalink` and
   `https://example.org/f.meta4`.
7. Drive every row through `uri.Normalize` first, so the test proves normalisation and routing agree.
8. Assert `errors.Is(err, engine.ErrNoEngine)` for the `nzb://` row and for a `mailto:` row.
9. Assert that with a `mediaMatch` returning true for one HTTPS host, that host routes to `NameYtDlp` while
   every other HTTPS URL still routes to `NameAria2` — proving row 3 precedes row 4.

## Acceptance criteria
- [ ] `internal/engine/engine.go` compiles and declares every method of the interface in §1, in that order.
- [ ] `ErrNotSupported`, `ErrNotFound` and `ErrUnavailable` exist with the exact texts from §1.
- [ ] `Route` returns `NameQBittorrent` for a magnet, a `.torrent` URL and a bare 40-hex infohash.
- [ ] `Route` returns `NameAria2` for `http`, `https`, `ftp`, `ftps`, `sftp`, `.metalink` and `.meta4`.
- [ ] A `mediaMatch` hit wins over the HTTPS row.
- [ ] `nzb://` and `mailto:` both return `ErrNoEngine`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok  	github.com/L-K-M/dl-tool/internal/engine` followed by its
elapsed time, and `TestRoute` running every row of the table above. No `FAIL`, no `[no test files]`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement any adapter; T018 and T019 own aria2 and T029 owns qBittorrent.
- Do NOT create `internal/engine/registry.go`; T019 adds it with the first adapter.
- Do NOT implement the yt-dlp extractor probe behind `mediaMatch`; T087 owns `internal/engine/ytdlp/`.
- Do NOT add the boot conformance probe; T101 owns it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

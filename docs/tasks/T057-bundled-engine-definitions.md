# T057 — Bundle the four default engine definitions

| Field | Value |
|---|---|
| **ID** | T057 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T055, T056 |
| **Blocks** | T058, T105 |
| **Parallel-safe** | no — wires the registry into `cmd/dl-tool/main.go` |
| **Implements** | [FR-052](../02-requirements.md#fr-052-ship-four-legitimate-engines-and-no-piracy-indexers) |
| **Decisions** | [ADR-0008](../decisions/0008-torznab-first-declarative-yaml-second.md), [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 3 new files plus the four definitions, ~400 lines of YAML and Go |

## Goal
The binary embeds exactly four `dlsearch/v1` definitions — `internet-archive`, `arch-linux`,
`academic-torrents` and `linux-distributions` — validates them at boot, also loads
`/config/engines/*.dlsearch.yaml`, and creates one enabled `indexers` row per bundled definition on first
boot. A user definition whose `id` collides with a bundled one is rejected by name.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §6.2 The four bundled engines](../07-search-and-indexers.md#62-the-four-bundled-engines)
   — the verified endpoints and what each one can and cannot report.
2. [`docs/07-search-and-indexers.md` §3.6 Worked example — `kind: rss`](../07-search-and-indexers.md#36-worked-example--kind-rss),
   [§3.7 `kind: json`](../07-search-and-indexers.md#37-worked-example--kind-json) and
   [§3.8 `kind: static`](../07-search-and-indexers.md#38-worked-example--kind-static) — copy these three
   documents verbatim; they are the specification, not an illustration.
3. [`docs/07-search-and-indexers.md` §6.1 Policy](../07-search-and-indexers.md#61-policy) — zero piracy
   indexers, no marketplace, no `kind: html` in the bundled set.
4. [`docs/07-search-and-indexers.md` §7 Storage, hot reload and provenance](../07-search-and-indexers.md#7-storage-hot-reload-and-provenance)
   — embedded and read-only, user definitions in `/config/engines`, provenance on every row.
5. [`docs/tasks/T056-dlsearch-definition-loader.md`](T056-dlsearch-definition-loader.md) — `LoadDefinition`
   and `DefinitionError`.

## Files
| Path | Action | Purpose |
|---|---|---|
| `definitions/engines/` | create | `internet-archive.yaml`, `arch-linux.yaml`, `academic-torrents.yaml`, `linux-distributions.yaml`. |
| `definitions/embed.go` | create | `package definitions` with `//go:embed engines/*.yaml`, because an `internal/` package cannot embed a parent directory. |
| `internal/search/bundled.go` | create | `Registry`, `Reload`, `SeedIndexers`. |
| `internal/search/bundled_test.go` | create | The bundled-set assertions and the collision case. |
| `cmd/dl-tool/main.go` | edit | Build the registry and call `SeedIndexers` in `OnStart`. |

No other file may be modified.

## Interface contract

```go
package definitions

//go:embed engines/*.yaml
var FS embed.FS
```

```go
package search

// Registry holds every loaded definition, bundled first, then user files. Bundled
// definitions are read-only; a user file whose id collides with one is rejected.
type Registry struct{ /* byID map[string]*Definition, sources map[string]string, log */ }

// BundledIDs is the exact bundled set. FR-052 forbids a fifth entry.
var BundledIDs = []string{"academic-torrents", "arch-linux", "internet-archive", "linux-distributions"}

// NewRegistry loads and validates definitions.FS, then userDir. A definition that fails
// validation is skipped with one warn record naming the file and the DefinitionError;
// it never blocks the others and never stops the process.
func NewRegistry(log *slog.Logger, userDir string) (*Registry, error)

// Reload re-reads and re-validates userDir. The bundled set is unaffected. It is the
// seam a later filesystem watcher calls; boot calls it once through NewRegistry.
func (r *Registry) Reload() error

func (r *Registry) Get(id string) (*Definition, bool)
func (r *Registry) List() []*Definition        // sorted by id
func (r *Registry) Source(id string) string    // "bundled" or the absolute file path
func (r *Registry) Errors() map[string]error   // file path -> DefinitionError, surfaced in the UI

// SeedIndexers creates one indexers row per bundled definition that has none, with
// definition_source='bundled', legal_tier='legitimate', enabled=1, priority=50,
// provenance='shipped with dl-tool' and seeders_unknown taken from caps. It is
// idempotent: the unique index on definition_id makes a second boot a no-op.
func (r *Registry) SeedIndexers(ctx context.Context, s *store.IndexerStore) (created int, err error)
```

`definitions/engines/academic-torrents.yaml`, the one bundled document doc 07 does not print in full,
derived from the feed shape verified in §6.2:

```yaml
dlsearch: 1
id: academic-torrents
name: Academic Torrents
description: "Research datasets and papers shared by universities and researchers, distributed as torrents."
homepage: https://academictorrents.com/
version: "1.0.0"
legal_tier: legitimate
kind: rss
caps:
  modes: {search: [q]}
  categories: {Dataset: 8000, Paper: 7020}
  seeders_unknown: true
request:
  base_url: https://academictorrents.com/
  path: rss.xml
  method: GET
  rate_limit_per_minute: 6
  timeout_seconds: 20
response:
  rows: "rss > channel > item"
  fields:
    title:    {path: "title"}
    _hash:    {path: "infohash"}
    infohash: {path: "infohash"}
    details:  {path: "guid"}
    size:     {path: "size", type: bytes}
    category: {path: "category"}
    download: {template: "https://academictorrents.com/download/{{ .Result._hash }}.torrent"}
  transforms:
    title:
      - {op: trim}
```

## Steps
1. Create `definitions/engines/internet-archive.yaml`, `arch-linux.yaml` and `linux-distributions.yaml` by
   copying doc 07 §3.7, §3.6 and §3.8 character for character, and `academic-torrents.yaml` from the contract
   above.
2. Create `definitions/embed.go` with the embed directive; the package holds no other declaration.
3. Create `internal/search/bundled.go` with `Registry`, `NewRegistry`, `Reload`, `Get`, `List`, `Source`,
   `Errors` and `SeedIndexers`, running every document through `LoadDefinition`.
4. Load bundled definitions first and mark them read-only; then read `userDir` for `*.dlsearch.yaml`, and
   reject a colliding id with the message `id already in use: <id>` recorded in `Errors()`.
5. A user definition that fails validation is skipped with one `warn` record carrying the file path and the
   `DefinitionError`; the process continues and the other engines stay usable.
6. Implement `SeedIndexers` over `store.IndexerStore.Create`, treating the duplicate-`definition_id` sentinel
   as "already seeded" rather than an error, and returning the number of rows actually created.
7. Edit `cmd/dl-tool/main.go` to build the registry with `filepath.Join(cfg.ConfigDir, "engines")` and call
   `SeedIndexers` once in `OnStart`, after the store is open, in the slot T055 step 9 reserves for it, and
   put that same registry into the `api.Deps` value handed to `NewServer`. Build no second registry.
8. Create `internal/search/bundled_test.go` with `TestBundledSetIsExactlyFour` reading `definitions.FS`,
   `TestEveryBundledDefinitionValidates`, `TestNoBundledDefinitionUsesKindHTML`,
   `TestUserDefinitionCollidingIDRejected`, `TestInvalidUserDefinitionDoesNotBlockOthers` and
   `TestSeedIndexersIsIdempotent`.
9. Add `TestNoPiracyIndexerNamesInRepository`, which walks `definitions/` and asserts no file names a
   public tracker; keep the probe list in the test file, not in a definition.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestBundledSetIsExactlyFour` asserts `definitions/engines/` contains exactly the four files of `BundledIDs` and no other entry.
- [ ] Every bundled definition loads with `LoadDefinition` returning no error, and none has `kind: html`.
- [ ] A user file with `id: arch-linux` is refused with `id already in use: arch-linux` and the bundled engine still loads.
- [ ] `SeedIndexers` on a database that already has the four rows creates zero rows and returns no error.
- [ ] Every seeded row has `enabled = 1`, `legal_tier = 'legitimate'` and `definition_source = 'bundled'`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/search/... ./internal/store/..." && echo BUNDLED_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, `ok  	github.com/L-K-M/dl-tool/internal/store`, every
test named in steps 8 and 9 listed as passing, and the final line of stdout exactly `BUNDLED_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add a fifth definition, an engine marketplace, or a link to any third-party definition repository.
- Do NOT add a filesystem watcher; `Reload` is the seam and boot calls it once.
- Do NOT execute a definition or fetch its endpoint here; T058 owns fetching and T105 owns `kind: static`.
- Do NOT auto-download or auto-update a definition from any URL, ever.
- Do NOT re-probe the Ubuntu and Debian URLs in `linux-distributions.yaml`; T105 owns that refresh.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

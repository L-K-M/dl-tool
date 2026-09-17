# T060 — Import a qBittorrent nova3 `.py` plugin's metadata

| Field | Value |
|---|---|
| **ID** | T060 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T055, T056, T059 |
| **Blocks** | — |
| **Parallel-safe** | no — extends T059's `internal/search/dlm_import.go` |
| **Implements** | [NFR-020](../02-requirements.md#nfr-020-execute-no-third-party-code), [FR-053](../02-requirements.md#fr-053-import-legacy-definitions-by-static-analysis-only) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 0 new files, ~240 LOC |

## Goal
Uploading a qBittorrent nova3 `.py` search plugin creates a **disabled** indexer row carrying its name, URL,
version and mapped categories, plus the source stored inert and a message pointing the user at
`dlsearch/v1`. The file is read with a literal-only reader: never `exec`, never `eval`, never imported.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §4.3 qBittorrent nova3 `.py` plugins — metadata only](../07-search-and-indexers.md#43-qbittorrent-nova3-py-plugins--metadata-only)
   — the four extracted values and the exact `#VERSION:` rule.
2. [`docs/07-search-and-indexers.md` §2.3 Standard newznab category IDs](../07-search-and-indexers.md#23-standard-newznab-category-ids)
   — the `jackett.py` friendly-name mapping, reused verbatim.
3. [`docs/07-search-and-indexers.md` §7 Storage, hot reload and provenance](../07-search-and-indexers.md#7-storage-hot-reload-and-provenance).
4. [`docs/tasks/T059-dlm-static-import.md`](T059-dlm-static-import.md) — `ImportResult` and the multipart
   dispatch this task extends.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/dlm_import.go` | modify | Add `ImportNovaPlugin` and the literal-only Python reader. |
| `internal/search/dlm_import_test.go` | modify | Metadata extraction, the version rule and the refusal cases. |
| `internal/search/testdata/` | modify | Add `legacy_plugin.py` and `hostile_plugin.py` (an `import os` plus a call at module scope). |
| `internal/api/search.go` | modify | Dispatch the `.py` extension to `ImportNovaPlugin`. |

No other file may be modified.

## Interface contract

```go
package search

// ImportNovaPlugin extracts metadata from a qBittorrent nova3 plugin. It never runs the
// file. The result always has Converted=false and Definition=nil: a nova3 plugin is
// procedural code and no mechanical conversion to dlsearch/v1 exists.
//
// Provenance is "imported:qbt-py"; Origin is the uploaded file name, whose stem must
// equal the class name qBittorrent would resolve with getattr(module, module_name).
func ImportNovaPlugin(data []byte, filename string) (ImportResult, error)

// NovaCategories maps the nine friendly names of a supported_categories dict onto
// newznab ids, exactly as doc 07 section 2.3 gives the jackett.py mapping. "all" means
// no category filter, and "pictures" has no documented newznab id: it is imported as a
// declared site value with no mapping and raises a warning.
var NovaCategories = map[string][]int{
	"all":      nil,
	"anime":    {5070},
	"books":    {8000},
	"games":    {1000, 4000},
	"movies":   {2000},
	"music":    {3000},
	"pictures": nil,
	"software": {4000},
	"tv":       {5000},
}

// parsePluginVersion implements doc 07 section 4.3 exactly: scan every line, strip ALL
// spaces, take the first line that starts with "#VERSION:" case-insensitively and read
// the remainder after 9 characters. qBittorrent reads only 16 bytes per line, so a
// longer line counts as absent and this returns "".
func parsePluginVersion(src []byte) string

// pyLiterals reads class-scope assignments of string, integer and dict literals and
// nothing else. Any other construct on the right-hand side is ignored, never evaluated.
// Returns the values of name, url and supported_categories when present.
func pyLiterals(src []byte) (name, url string, categories map[string]string, err error)
```

## Steps
1. Add `parsePluginVersion` to `internal/search/dlm_import.go` following the rule character for character,
   including the 16-byte line cap.
2. Add `pyLiterals`: scan lines, accept `name = "…"`, `url = "…"`, `supported_categories = { … }` and
   integer assignments at class scope; parse the dict with a small literal reader that accepts only quoted
   keys and quoted values. Reject nothing — unknown constructs are skipped — but never evaluate one.
3. Add `ImportNovaPlugin`: require the file to be valid UTF-8 and at most 512 KiB; extract the version, name,
   URL and categories; require a class whose name equals the file stem and record a warning when it differs.
4. Map every `supported_categories` key through `NovaCategories`; add the warning
   `category "pictures" has no newznab equivalent and is imported unmapped` when present.
5. Always return `Converted: false`, `Kind: "dlsearch"`, `Provenance: "imported:qbt-py"` and the warning
   `dl-tool does not run Python search plugins. Re-express this plugin as a dl-tool YAML engine.`
6. Extend the multipart dispatch in `internal/api/search.go` with the `.py` extension, creating the row with
   `enabled = false` and the inert source in `settings_json`, exactly like the `.dlm` path.
7. Add to `internal/search/dlm_import_test.go`: `TestPluginVersionRule` (a normal line, a spaced line, a
   17-byte line and an absent line), `TestPluginMetadataExtracted`, `TestPluginClassNameMismatchWarns`,
   `TestPicturesCategoryWarns`, `TestHostilePluginIsNotExecuted` (asserting the side effect the file would
   have produced never happens) and `TestPluginImportedDisabled`.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestPluginVersionRule` asserts `# VERSION: 1.42` and `#VERSION:1.42` both yield `1.42`, and a line longer than 16 bytes yields the empty string.
- [x] `TestPluginMetadataExtracted` asserts `name`, `url` and all nine category keys are read from `legacy_plugin.py`.
- [x] `TestHostilePluginIsNotExecuted` asserts no file, process or environment side effect occurs during import.
- [x] Every `.py` import produces `enabled = 0`, `provenance = 'imported:qbt-py'` and a warning naming `dlsearch/v1`.
- [ ] `go list -deps ./internal/search` contains no `os/exec` and no `plugin`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/search/... ./internal/api/..." && echo PY_IMPORT_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, `ok  	github.com/L-K-M/dl-tool/internal/api`, every
test named in step 7 listed as passing, and the final line of stdout exactly `PY_IMPORT_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT attempt to convert a nova3 plugin into a `dlsearch/v1` engine; the shapes do not correspond.
- Do NOT add a Python parser dependency, a sandbox, a WASM runtime or a subprocess.
- Do NOT install, download or list qBittorrent's own search plugins; dl-tool never calls its `search/*` API.
- Do NOT enable an imported plugin row under any circumstance.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

```
$ make lint && make test PKG="./internal/search/... ./internal/api/..." && echo PY_IMPORT_OK
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/search/... ./internal/api/...
ok  	github.com/L-K-M/dl-tool/internal/search	6.788s
ok  	github.com/L-K-M/dl-tool/internal/api	119.074s
PY_IMPORT_OK
```

The step-7 test set, run with `-v`:

```
--- PASS: TestPluginVersionRule (0.00s)
    --- PASS: TestPluginVersionRule/normal (0.00s)
    --- PASS: TestPluginVersionRule/spaced (0.00s)
    --- PASS: TestPluginVersionRule/mixed_case (0.00s)
    --- PASS: TestPluginVersionRule/over_16_bytes (0.00s)
    --- PASS: TestPluginVersionRule/absent (0.00s)
--- PASS: TestPluginMetadataExtracted (0.00s)
--- PASS: TestPluginClassNameMismatchWarns (0.00s)
--- PASS: TestPicturesCategoryWarns (0.00s)
--- PASS: TestHostilePluginIsNotExecuted (0.00s)
--- PASS: TestPluginImportedDisabled (0.09s)
--- PASS: TestPluginImportRefusals (0.05s)
ok  	github.com/L-K-M/dl-tool/internal/search	0.185s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/search.go
internal/search/dlm_import.go
internal/search/dlm_import_test.go
internal/search/testdata/README.md
internal/search/testdata/hostile_plugin.py
internal/search/testdata/legacy_plugin.py
web/src/api/schema.d.ts
```

Exactly the Files table plus the two doc 13 §7.1 standing-exception paths
(`api/openapi.json`, `web/src/api/schema.d.ts` — the import operation's
multipart description changed; `git diff` on both shows only that text).

## Blocked

The implementation is complete and the `## Verification` block above passes, but
acceptance criterion 5 cannot pass as written, and the repair is a plan decision,
not an implementation one.

Criterion: "`go list -deps ./internal/search` contains no `os/exec` and no `plugin`."

On the base commit (ffcf7fe, before any T060 change) the command already lists
`os/exec`:

```
$ go list -deps ./internal/search | grep -nE 'os/exec|^plugin$'
235:os/exec
```

The path is `internal/search/bundled.go` (T057) → `internal/store` →
`modernc.org/sqlite` → `modernc.org/libc` → `os/exec` — the pure-Go SQLite
driver's C runtime imports `os/exec` for its own internals. The same result
holds with `CGO_ENABLED=0` and `CGO_ENABLED=1`. No file in this task's
`## Files` table can remove that edge, and the importer itself adds only
`slices` and `sort` to the package's imports. `plugin` is absent.

The guarantee the criterion encodes — the .py importer can never spawn or load
code — is enforced at source level by `TestNoInterpreterIsSpawned` (merged in
T059), which parses every Go file of package `internal/search` and asserts none
imports `os/exec`, `plugin`, or a PHP/Python runtime; and by the literal-only
reader this task adds, which consumes quoted strings, integers and dict literals
and nothing else.

Suggested repair — pick one:

a. Scope the check to the package's direct imports:
   `go list -f '{{join .Imports " "}}' ./internal/search` contains neither
   `os/exec` nor `plugin` (verified on this tree).
b. Point the criterion at `TestNoInterpreterIsSpawned`, the enforcement the tree
   already runs — the same shape T059's own criterion 5 used ("asserts the
   package imports no os/exec, no plugin…").
c. Keep `-deps` but whitelist the `modernc.org/libc` chain explicitly.

The row stays `todo`; the PR is not to be merged until the criterion is repaired.

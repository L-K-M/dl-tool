# T059 — Import a Synology `.dlm` module by static analysis

| Field | Value |
|---|---|
| **ID** | T059 |
| **Milestone** | M4 |
| **Status** | todo |
| **Depends on** | T055, T056 |
| **Blocks** | T060 |
| **Parallel-safe** | no — extends T055's `internal/api/search.go` |
| **Implements** | [FR-053](../02-requirements.md#fr-053-import-legacy-definitions-by-static-analysis-only), [NFR-020](../02-requirements.md#nfr-020-execute-no-third-party-code) |
| **Decisions** | [ADR-0010](../decisions/0010-never-execute-third-party-definitions.md) |
| **Est. size** | 2 new files, ~420 LOC |

## Goal
`POST /indexers/import` accepts a `.dlm` archive or a `.dlsearch.yaml` file, reads it entirely in memory,
converts the two mechanically convertible `.dlm` shapes into a `dlsearch/v1` draft, and creates a **disabled**
indexer row carrying its provenance and the original module source as an inert blob. Nothing is written to
disk, nothing is extracted, and no interpreter is ever started.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/07-search-and-indexers.md` §4.1 `.dlm` — Synology Download Station search module](../07-search-and-indexers.md#41-dlm--synology-download-station-search-module)
   — the `INFO` keys, the member-validation table and the two convertible shapes.
2. [`docs/07-search-and-indexers.md` §4.2 `.dlm` `addResult` → Torznab → dl-tool field mapping](../07-search-and-indexers.md#42-dlm-addresult--torznab--dl-tool-field-mapping)
   — the positional nine-tuple and its target fields.
3. [`docs/12-security-and-threat-model.md` §5.3 `.dlm` tar-member validation](../12-security-and-threat-model.md#53-dlm-tar-member-validation)
   — the same posture with looser numbers; apply the stricter table, which is doc 07 §4.1.
4. [`docs/05-api-contract.md` §9.1 Indexer CRUD](../05-api-contract.md#91-indexer-crud) — the import request,
   the `201` body and the `warnings[]` array.
5. [`docs/07-search-and-indexers.md` §7 Storage, hot reload and provenance](../07-search-and-indexers.md#7-storage-hot-reload-and-provenance)
   — imported engines are always created disabled and always show where they came from.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/search/dlm_import.go` | create | Archive validation, `INFO` parsing, static analysis and conversion. |
| `internal/search/dlm_import_test.go` | create | The two convertible shapes, the metadata-only path and every rejection. |
| `internal/search/testdata/` | modify | Add `jackett.dlm`, `rssmodule.dlm` and `hostile.dlm` (symlink member, `..` name, 5 MiB member). |
| `internal/api/search.go` | modify | Add the multipart branch of `POST /indexers/import`. |

No other file may be modified.

## Interface contract

```go
package search

// ImportResult is what every importer produces. Converted is false for the
// metadata-only path, and Definition is then nil.
type ImportResult struct {
	Definition *Definition
	Name       string // INFO.displayname, or INFO.name
	Kind       string // dlsearch | torznab
	Provenance string // "imported:dlm"
	Origin     string // the uploaded file name, stored in settings_json.origin
	Source     []byte // the original module, stored inert for the "view source" pane
	Converted  bool
	Warnings   []string
}

// Archive limits, doc 07 section 4.1. They are stricter than doc 12 section 5.3, so
// satisfying these satisfies both.
const (
	MaxDLMCompressedBytes   = 1 << 20 // 1 MiB uploaded
	MaxDLMMembers           = 16
	MaxDLMMemberBytes       = 1 << 20 // 1 MiB per member, uncompressed
	MaxDLMUncompressedBytes = 4 << 20 // 4 MiB total, enforced with io.LimitReader
)

// DLMInfo is the INFO member, doc 07 section 4.1.
type DLMInfo struct {
	Name            string `json:"name"`
	DisplayName     string `json:"displayname"`
	Description     string `json:"description"`
	Version         string `json:"version"`
	Site            string `json:"site"`
	Module          string `json:"module"`
	Type            string `json:"type"` // only "search" is supported
	Class           string `json:"class"`
	AccountSupport  bool   `json:"accountsupport"` // observed in third-party modules only
}

// ImportDLM validates the archive, reads exactly INFO and the member INFO.module, and
// statically analyses the module. It never writes a file, never extracts and never
// executes. Every rejection names the rule it hit.
func ImportDLM(data []byte, filename string) (ImportResult, error)

// ImportDefinitionFile accepts an uploaded .dlsearch.yaml, validates it with
// LoadDefinition and returns it with Provenance "imported:file".
func ImportDefinitionFile(data []byte, filename string) (ImportResult, error)

// analyseModule detects the convertible shapes of doc 07 section 4.1:
//   addRSSResults  -> kind: rss,     base_url from the single http string literal
//   torznab proxy  -> kind: torznab, <host> and <apikey> mapped to the two settings
// Anything else returns converted=false and the metadata-only result.
func analyseModule(php []byte, info DLMInfo) (*Definition, bool, []string)
```

T055 already declares `ImportIndexerInput` and `ImportOutput` and registers the operation. This task adds
the `multipart/form-data` branch to the existing `ImportIndexer` handler; the uploaded part is named `file`
and its file name decides the importer.

## Steps
1. Create `internal/search/dlm_import.go` with the constants, `DLMInfo`, `ImportResult`, `ImportDLM`,
   `ImportDefinitionFile` and `analyseModule`.
2. Reject an upload over `MaxDLMCompressedBytes` before decompressing; wrap the gzip stream in an
   `io.LimitReader` of `MaxDLMUncompressedBytes` so a bomb aborts mid-stream.
3. Walk the tar with `archive/tar` applying doc 07 §4.1 row for row: at most 16 members; regular files only,
   rejecting symlink, hardlink, device, FIFO and directory entries; names that are relative, free of `..`,
   free of `/` and `\`, valid UTF-8 and at most 255 bytes; each member at most 1 MiB.
4. Read `INFO`, decode it as a JSON object, require `name`, `version`, `module`, `class` and
   `type == "search"`, then read the member named by `INFO.module`; ignore every other member.
5. Detect the `addRSSResults` shape: the module calls `addRSSResults` and carries exactly one string literal
   containing `http`. Emit `kind: rss` with that literal split into `request.base_url` and `request.path`, the
   concatenated query parameter replaced by `{{ .Keywords }}`, and the default RSS field mapping of doc 07
   §4.2.
6. Detect the Torznab-proxy shape: the module contains both `simplexml_load_string` and the literal
   `torznab:attr`. Emit `kind: torznab` with `settings` `base_url` (text) and `api_key` (password), and add
   the warning `Download Station stored the Jackett host as the username and the API key as the password;
   they are now base_url and api_key`.
7. Everything else returns `Converted: false` with the `INFO` metadata only, and the UI message of doc 07
   §4.1 recorded as a warning.
8. Extend `internal/api/search.go`: switch on `Content-Type`, parse `multipart/form-data` with a 1 MiB
   `ParseMultipartForm` cap, dispatch on the file-name extension `.dlm` or `.dlsearch.yaml`, and create the
   row with `enabled = false`, `definition_source = 'imported'`, `legal_tier = 'user-supplied'` and
   `provenance = 'imported:dlm'` or `'imported:file'`; store the inert source and `Origin` in
   `settings_json`.
9. Return `413` `/problems/payload-too-large` above the cap, `415` `/problems/unsupported-media-type` for any
   other content type, and `422` `/problems/validation-failed` naming the violated rule for a bad archive.
10. Create `internal/search/dlm_import_test.go` with `TestImportJackettDLMBecomesTorznab`,
    `TestImportRSSModuleBecomesRSSDefinition`, `TestUnconvertibleModuleImportsDisabledMetadataOnly`,
    `TestRejectsSymlinkMember`, `TestRejectsTraversalName`, `TestRejectsOversizeMember`,
    `TestRejectsMissingModuleMember` and `TestNoInterpreterIsSpawned`, the last asserting the package imports
    neither `os/exec` nor `plugin`.
11. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] `TestImportJackettDLMBecomesTorznab` asserts `kind == "torznab"`, the two settings exist and the host/API-key warning is present.
- [x] Every import path produces a row with `enabled = 0` and `provenance = 'imported:dlm'` for a `.dlm`.
- [x] `hostile.dlm` is rejected three ways — symlink member, `..` in a name, 5 MiB member — each naming the rule.
- [x] Nothing is written outside the process: the test asserts the temporary directory is empty after every import.
- [x] `TestNoInterpreterIsSpawned` asserts `internal/search` imports no `os/exec`, no `plugin` and no PHP or Python runtime.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG="./internal/search/... ./internal/api/..." && echo DLM_IMPORT_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/search`, `ok  	github.com/L-K-M/dl-tool/internal/api`, every
test named in step 10 listed as passing, and the final line of stdout exactly `DLM_IMPORT_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `.dlm` export; the format is import-only.
- Do NOT implement the qBittorrent `.py` importer; T060 owns it.
- Do NOT execute, transpile, sandbox or partially interpret PHP. There is no fallback path.
- Do NOT enable an imported engine, and do NOT infer `legal_tier: legitimate` for one.
- Do NOT write the uploaded archive or any member to `/config` or `/data`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

```
$ make lint && make test PKG="./internal/search/... ./internal/api/..." && echo DLM_IMPORT_OK
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
ok  	github.com/L-K-M/dl-tool/internal/search	6.050s
ok  	github.com/L-K-M/dl-tool/internal/api	118.625s
DLM_IMPORT_OK
```

The step-10 test set plus the endpoint and regression additions in
`internal/search/dlm_import_test.go`, run with `-v`:

```
--- PASS: TestImportJackettDLMBecomesTorznab (0.01s)
--- PASS: TestImportRSSModuleBecomesRSSDefinition (0.01s)
--- PASS: TestRSSModuleDecodesStaticQueryPairs (0.01s)
--- PASS: TestRSSModuleNeedsKeywordParam (0.03s)
    --- PASS: TestRSSModuleNeedsKeywordParam/no_query (0.01s)
    --- PASS: TestRSSModuleNeedsKeywordParam/trailing_& (0.01s)
    --- PASS: TestRSSModuleNeedsKeywordParam/value-only (0.00s)
    --- PASS: TestRSSModuleNeedsKeywordParam/path-appended (0.01s)
--- PASS: TestRSSModuleIgnoresCommentURLs (0.01s)
--- PASS: TestImportDefinitionFileTorznabKind (0.00s)
--- PASS: TestUnconvertibleModuleImportsDisabledMetadataOnly (0.42s)
--- PASS: TestRejectsSymlinkMember (0.00s)
--- PASS: TestRejectsTraversalName (0.00s)
--- PASS: TestRejectsOversizeMember (0.19s)
--- PASS: TestHostileFixtureRejected (0.09s)
--- PASS: TestRejectsDuplicateMember (0.00s)
--- PASS: TestRejectsMissingModuleMember (0.00s)
--- PASS: TestRejectsMalformedArchives (0.03s)
--- PASS: TestNoInterpreterIsSpawned (0.00s)
--- PASS: TestImportEndpointCreatesDisabledRows (0.80s)
    --- PASS: TestImportEndpointCreatesDisabledRows/dlm (0.42s)
    --- PASS: TestImportEndpointCreatesDisabledRows/dlsearch.yaml (0.38s)
--- PASS: TestImportEndpointSettingsJSON (0.38s)
--- PASS: TestImportEndpointCapsOrigin (0.03s)
--- PASS: TestImportEndpointRejections (0.50s)
--- PASS: TestImportEndpointDuplicateConflicts (0.38s)
ok  	github.com/L-K-M/dl-tool/internal/search	3.969s
```

Scope check:

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
api/openapi.json
internal/api/search.go
internal/search/dlm_import.go
internal/search/dlm_import_test.go
internal/search/testdata/README.md
internal/search/testdata/hostile.dlm
internal/search/testdata/jackett.dlm
internal/search/testdata/rssmodule.dlm
web/src/api/schema.d.ts
```

Exactly the Files table plus the two doc 13 §7.1 standing-exception paths
(`api/openapi.json`, `web/src/api/schema.d.ts` — the import operation's
request-body description changed; `git diff` on both shows only that
description text). `make ci` (lint vet typecheck test compose-check doclint)
green on this tree. `TestNoInterpreterIsSpawned` walks the package's own
imports and asserts none of `os/exec`, `plugin`, or a PHP/Python runtime
appears in `internal/search`.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

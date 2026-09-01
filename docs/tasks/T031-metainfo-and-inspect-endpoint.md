# T031 — Parse torrent metainfo and serve `POST /tasks/inspect`

| Field | Value |
|---|---|
| **ID** | T031 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T015, T020 |
| **Blocks** | T033, T038, T100 |
| **Parallel-safe** | no — extends `internal/api/tasks_test.go` and `internal/api/server.go` |
| **Implements** | [FR-006](../02-requirements.md#fr-006-inspect-a-submission-before-committing-it); the parsing half of [FR-022](../02-requirements.md#fr-022-record-both-bittorrent-infohash-forms) |
| **Decisions** | [ADR-0003](../decisions/0003-chi-huma-code-first-openapi.md), [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new files, ~420 LOC |

## Goal
`uri.InspectTorrent` turns raw `.torrent` bytes into a manifest with both BEP 52 infohashes, and
`POST /api/v1/tasks/inspect` returns one manifest per submission without creating a task, without writing
to disk and without inserting a `tasks` row.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §5.3 `POST /tasks/inspect`](../05-api-contract.md#53-post-tasksinspect)
2. [`docs/06-download-engines.md` §3.4 Bencode and the v1 infohash](../06-download-engines.md#34-bencode-and-the-v1-infohash-metainfogo)
3. [`docs/06-download-engines.md` §3.5 BitTorrent v2 (BEP 52) identity](../06-download-engines.md#35-bittorrent-v2-bep-52-identity)
4. [`docs/06-download-engines.md` §3.3 Magnet URIs](../06-download-engines.md#33-magnet-uris-magnetgo)
5. [`docs/12-security-and-threat-model.md` §3.2 `sanitiseSegment`](../12-security-and-threat-model.md#32-sanitisesegments-string-string)
6. [`docs/05-api-contract.md` §1.3 Errors](../05-api-contract.md#13-errors--rfc-9457-applicationproblemjson)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/uri/metainfo.go` | create | `Manifest`, `ManifestFile`, `InspectTorrent`, `infoDictBytes`. |
| `internal/api/tasks_inspect.go` | create | The `POST /tasks/inspect` handler and its Huma structs. |
| `internal/api/tasks_inspect_test.go` | create | `humatest` cases for every branch and status code. |
| `internal/uri/normalize_test.go` | modify | Add the bencode, v1, v2 and hybrid parsing cases. |
| `internal/api/server.go` | modify | Register `inspect-tasks`. |

No other file may be modified.

## Interface contract

```go
package uri

// ErrNotTorrent is returned when the bytes are not a bencoded metainfo file.
var ErrNotTorrent = errors.New("uri: not a torrent file")

// Manifest is the inspect-before-commit result for one submission. Producing it never touches disk and
// never creates an engine task.
type Manifest struct {
	Name       string
	TotalSize  int64
	Files      []ManifestFile
	InfohashV1 string // 40 lowercase hex, "" when the torrent has no v1 hash
	InfohashV2 string // 64 lowercase hex, "" when the torrent has no v2 hash
	Private    *bool  // nil when unknown
}

type ManifestFile struct {
	Index int
	Path  string // relative, cleaned; never absolute, never containing ".."
	Size  int64
}

// InspectTorrent parses raw .torrent bytes and computes both infohashes. It must not touch disk and
// must not contact any engine.
func InspectTorrent(b []byte) (Manifest, error)

// infoDictBytes returns the raw bencoded bytes of the top-level "info" value exactly as they appear in
// b. The hashes are computed over these bytes: re-encoding is forbidden, because unknown keys and
// non-canonical ordering in the wild make a re-encode differ from the original.
func infoDictBytes(b []byte) ([]byte, error)
```

Hash rules, exactly these:

| Torrent kind | `InfohashV1` | `InfohashV2` |
|---|---|---|
| v1-only (no `meta version`) | `sha1(infoDictBytes)` hex | `""` |
| v2-only (`meta version` = 2, no `pieces`) | `""` | `sha256(infoDictBytes)` hex |
| hybrid (`meta version` = 2 **and** `pieces`) | `sha1(...)` hex | `sha256(...)` hex |

```go
package api

type InspectTasksInput struct {
	Body struct {
		URIs     []string `json:"uris"               maxItems:"50"`
		Blob     string   `json:"blob,omitempty"`     // base64, 10 MiB decoded maximum
		Filename string   `json:"filename,omitempty"`
	}
}

type ManifestFileDTO struct {
	Index int    `json:"index"`
	Path  string `json:"path"`
	Size  *int64 `json:"size"`
}

type ManifestDTO struct {
	SourceURI       string            `json:"source_uri"`
	Kind            string            `json:"kind"` // the source_kind vocabulary of 04 §3.3
	Name            string            `json:"name"`
	TotalSize       *int64            `json:"total_size"`
	FileCount       *int              `json:"file_count"`
	MetadataPending bool              `json:"metadata_pending"`
	InfohashV1      *string           `json:"infohash_v1"`
	InfohashV2      *string           `json:"infohash_v2"`
	Files           []ManifestFileDTO `json:"files"`
}

type InspectTasksOutput struct {
	Body struct {
		Manifests []ManifestDTO `json:"manifests"`
		Rejected  []RejectedURI `json:"rejected"`
	}
}

// magnetInspector is implemented by an engine that can resolve magnet metadata without creating a task.
// The qBittorrent implementation lands in T038; while no registered engine implements it, a magnet
// submission returns metadata_pending true with files null.
type magnetInspector interface {
	InspectMagnet(ctx context.Context, magnet string) (uri.Manifest, error)
}
```

## Steps
1. Create `internal/uri/metainfo.go`. Implement `infoDictBytes` as a bencode scanner that walks the
   top-level dictionary, recognises the key `4:info`, and returns the exact byte slice of its value.
2. Decode the structure of that slice with the pinned `github.com/anacrolix/torrent/metainfo` module and
   read `name`, `piece length`, `pieces`, `length`, `files`, `meta version`, `file tree` and `private`.
3. Compute `InfohashV1` as SHA-1 and `InfohashV2` as SHA-256 over `infoDictBytes`, applying the table
   above; lowercase-hex both and never re-encode the dictionary.
4. Build `Files`: `length` alone yields one entry named `name`; a `files` list yields one entry per
   member joining its `path` segments; a v2 `file tree` is walked depth-first in key order. Index from 0,
   run every segment through `sanitiseSegment`, and reject an entry that escapes with `ErrNotTorrent`.
5. Cap parsing defensively: reject input over 10 MiB, over 100 000 files, or with a nesting depth above
   16, and return `ErrNotTorrent` wrapped with what failed.
6. Add the cases to `internal/uri/normalize_test.go`: a hand-written single-file v1 torrent built as a
   bencode string literal in the test, its known SHA-1, a hybrid torrent, a v2-only torrent, a truncated
   file, and a file whose `info` value carries an unknown key that a re-encode would reorder.
7. Create `internal/api/tasks_inspect.go` with the handler: route each entry with `uri.Normalize` and the
   engine router, then produce one `ManifestDTO` per accepted submission.
8. Branch by kind: `torrent` blob → `uri.InspectTorrent`; `magnet` → the registered `magnetInspector`
   under a 60 s deadline, falling back to `metadata_pending: true` with `files: null` on timeout or when
   no engine implements it; `http`, `ftp`, `sftp` and `media` → one file entry whose `size` may be null
   and both infohashes null.
9. Return the rejected entries in the `{uri, type, detail}` shape T020 already defines, and map the status
   codes of 05 §5.3 — `413` above the cap, `422` for validation or an unsupported scheme, `503` when the
   magnet path needs an engine that is down.
10. Register `inspect-tasks` in `internal/api/server.go` as `POST /tasks/inspect`, admin and non-admin
    alike, with the same authentication and CSRF requirements as `POST /tasks`.
11. Create `internal/api/tasks_inspect_test.go` with `humatest` cases for: a `.torrent` blob manifest, a
    magnet resolved through a fake `magnetInspector`, a magnet timing out into `metadata_pending`, an
    HTTP URL manifest, an `ed2k:` rejection, an oversized blob, and an assertion that
    `SELECT count(*) FROM tasks` is unchanged after every case.

## Acceptance criteria
- [ ] `InspectTorrent` computes the v1 hash over the original `info` bytes, proved by the unknown-key case.
- [ ] A hybrid torrent yields both hashes; a v2-only torrent yields `InfohashV2` only.
- [ ] Every `ManifestFile.Path` is relative and free of `..` after `sanitiseSegment`.
- [ ] `POST /tasks/inspect` inserts no `tasks` row and writes no file, asserted by a count before and after.
- [ ] A magnet with no available inspector returns `200` with `metadata_pending: true` and `files: null`.
- [ ] A blob above 10 MiB decoded returns `413` `/problems/payload-too-large`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/...
```
Expected: `make lint` prints nothing, then `ok` for
`github.com/L-K-M/dl-tool/internal/uri` and `github.com/L-K-M/dl-tool/internal/api`, with
`TestInspectTorrentV1`, `TestInspectTorrentHybrid`, `TestInfoBytesAreNotReencoded`,
`TestInspectCreatesNoTask` and `TestInspectMagnetPending` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git diff --name-only | sort
```
Expected: exactly the paths in the Files table.

## Out of scope — do NOT
- Do NOT create a task, write to a destination, or call `Engine.Add` from this endpoint.
- Do NOT implement `InspectMagnet` on the qBittorrent client; T038 owns it.
- Do NOT store an infohash or check for duplicates; T100 owns the columns, the lookup and
  `torrent_duplicate`.
- Do NOT accept `multipart/form-data` here; T033 adds the shared form parser to both endpoints.
- Do NOT run a BitTorrent engine in-process. Parsing only.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

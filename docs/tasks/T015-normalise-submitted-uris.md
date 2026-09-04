# T015 — Normalise and decode a submitted URI

| Field | Value |
|---|---|
| **ID** | T015 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T004 |
| **Blocks** | T016, T020, T031, T067, T069 |
| **Parallel-safe** | yes — touches only `internal/uri/` |
| **Implements** | [FR-003](../02-requirements.md#fr-003-decode-obfuscated-chinese-download-manager-schemes), [FR-004](../02-requirements.md#fr-004-reject-ed2k-links-with-a-clear-message) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md) |
| **Est. size** | 3 new source files, 1 test file, ~360 LOC |

## Goal
`internal/uri` turns any string a user pastes into a `Normalized` value carrying a canonical URI, a `Kind`
and, for magnets, the display name, trackers and lowercase hexadecimal infohashes. `thunder://`,
`flashget://` and `qqdl://` decode to their inner URL; `ed2k://` parses for display and returns
`ErrUnsupportedScheme`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §3 URI normalisation](../06-download-engines.md#3-uri-normalisation-internaluri)
2. [`docs/06-download-engines.md` §3.1 Obfuscated schemes](../06-download-engines.md#31-obfuscated-schemes-obfuscatedgo)
3. [`docs/06-download-engines.md` §3.2 ed2k](../06-download-engines.md#32-ed2k--parsed-never-downloaded)
4. [`docs/06-download-engines.md` §3.3 Magnet URIs](../06-download-engines.md#33-magnet-uris-magnetgo)
5. [`docs/14-conventions.md` §2.2 Error model](../14-conventions.md#22-error-model)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/uri/normalize.go` | create | `Kind`, `Normalized`, `Normalize`, `ParseED2K`, scheme classification. |
| `internal/uri/obfuscated.go` | create | `DecodeObfuscated` for the three Base64 wrapper schemes. |
| `internal/uri/magnet.go` | create | `ParseMagnet`: `xt`, `dn`, `tr`, `x.pe`, base32 to hex, `btmh` v2. |
| `internal/uri/normalize_test.go` | create | Table tests for all three files, including the fixtures below. |

No other file may be modified.

## Interface contract

```go
package uri

// Kind mirrors the tasks.source_kind enum in docs/04-data-model.md.
type Kind string

const (
	KindHTTP     Kind = "http"
	KindFTP      Kind = "ftp"
	KindSFTP     Kind = "sftp"
	KindMagnet   Kind = "magnet"
	KindTorrent  Kind = "torrent"
	KindMetalink Kind = "metalink"
	KindMedia    Kind = "media"
)

// ErrUnsupportedScheme is returned for ed2k:// and nzb sources.
var ErrUnsupportedScheme = errors.New("uri: unsupported scheme")

// Normalized is one classified, canonical submission.
type Normalized struct {
	Kind           Kind
	URI            string // the plain, canonical URI handed to the engine
	OriginalScheme string // "thunder" | "flashget" | "qqdl" | "" — provenance for the UI
	DisplayName    string // magnet dn, or ""
	InfohashV1     string // lowercase hex, 40 chars
	InfohashV2     string // lowercase hex, 64 chars
	Trackers       []string
	PeerHints      []string // magnet x.pe values, "host:port"
}

// Normalize decodes obfuscated schemes, lowercases infohashes and classifies the URI.
func Normalize(raw string) (Normalized, error)

// DecodeObfuscated returns the plain URL behind a thunder://, flashget:// or qqdl:// link.
// ok is false when the scheme is not one of the three or the payload does not decode to a URL.
func DecodeObfuscated(raw string) (plain string, ok bool)

// ParseMagnet extracts the BEP 9 parameters of a magnet: URI. Every repeatable key is read
// with url.Values, never with a single-valued map.
func ParseMagnet(raw string) (Normalized, error)

// ED2K is a parsed ed2k:// link, kept for display only. dl-tool never downloads one.
type ED2K struct {
	Filename string
	SizeBytes int64
	Hash     string // 32 hex characters, an MD4 root hash
}

// ParseED2K parses ed2k://|file|<name>|<size>|<hash>|/ . Callers reject the submission with
// ErrUnsupportedScheme and the message "ed2k is not supported in v1".
func ParseED2K(raw string) (ED2K, error)
```

## Steps
1. In `internal/uri/normalize.go` declare `Kind`, its seven constants, `Normalized` and
   `ErrUnsupportedScheme` exactly as above, each exported symbol with a doc comment.
2. In `internal/uri/obfuscated.go` implement `DecodeObfuscated` in the seven ordered steps of
   [§3.1](../06-download-engines.md#31-obfuscated-schemes-obfuscatedgo): split on the first `://`; cut from
   the first `&`; strip a trailing `/`; re-pad to a multiple of four with `=`; decode with the standard
   alphabet then retry with the URL-safe alphabet; strip the sentinel prefix and suffix when present; accept
   only a result beginning `http://`, `https://`, `ftp://`, `ftps://`, `sftp://` or `ed2k://`,
   case-insensitively.
3. In `internal/uri/magnet.go` implement `ParseMagnet` with `url.Values`, reading every `xt`, `tr`, `x.pe`
   and `ws` occurrence. Lowercase a 40-character `urn:btih` value; base32-decode a 32-character one to 20
   bytes and hex-encode it; take the 64 hexadecimal digits after the `1220` prefix of `urn:btmh` into
   `InfohashV2`.
4. In `internal/uri/normalize.go` implement `ParseED2K` as a `strings.Split(s, "|")` whose `parts[1]` must
   equal `file`; return `ErrUnsupportedScheme` from `Normalize` for that scheme.
5. Implement `Normalize`: call `DecodeObfuscated` first and record `OriginalScheme` when it succeeds, then
   classify the recovered URI — `magnet:` and a path ending `.torrent` to `KindTorrent`/`KindMagnet`,
   `.metalink`/`.meta4` to `KindMetalink`, `http`/`https` to `KindHTTP`, `ftp`/`ftps` to `KindFTP`, `sftp`
   to `KindSFTP` — and return `ErrUnsupportedScheme` for anything else.
6. Strip userinfo from the returned `URI` so a password never reaches storage or a log.
7. In `internal/uri/normalize_test.go` add the three worked round-trips of §3.1 as table rows, plus the
   independent fixture `thunder://QUFodHRwOi8vd3d3LmZyZWUtei5uZXQvMS5yYXJaWg==` →
   `http://www.free-z.net/1.rar`, and the FR-003 fixture
   `thunder://QUFodHRwOi8vd3d3LmV4YW1wbGUuY29tL2ZpbGUuemlwWlo=` → `http://www.example.com/file.zip`.
8. Add negative rows for missing `=` padding, a trailing `/`, the URL-safe alphabet and a `&freeznet`
   suffix; each must still decode.
9. Add the verbatim ed2k fixture
   `ed2k://|file|The_Two_Towers-The_Purist_Edit-Trailer.avi|14997504|965c013e991ee246d63d45ea71954c4d|/`
   and assert `ErrUnsupportedScheme` with `errors.Is`.
10. Add magnet rows for a 40-hex `btih`, the same hash in 32-character base32, a `btmh` v2 magnet and a
    hybrid magnet carrying both; assert lowercase hexadecimal of length 40 and 64.

## Acceptance criteria
- [ ] `Normalize` returns `KindHTTP` and `OriginalScheme: "thunder"` for the two thunder fixtures.
- [ ] `DecodeObfuscated` returns `ok == false` for a payload that decodes to a non-URL.
- [ ] `errors.Is(err, uri.ErrUnsupportedScheme)` is true for the ed2k fixture and for `nzb://x`.
- [ ] A base32 `btih` magnet and its hexadecimal twin produce the identical `InfohashV1`.
- [ ] A hybrid magnet fills both `InfohashV1` (40 chars) and `InfohashV2` (64 chars).
- [ ] Every exported symbol carries a doc comment starting with its own name.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/uri/...
```
Expected: `make lint` prints nothing, then one line
`ok  	github.com/L-K-M/dl-tool/internal/uri` followed by its elapsed time. No `FAIL`, no
`[no test files]`, and `TestDecodeObfuscated`, `TestNormalizeClassifies`, `TestParseMagnet` and
`TestParseED2K` all run.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT parse bencode or `.torrent` bytes; `internal/uri/metainfo.go` and `InspectTorrent` belong to T031.
- Do NOT write the routing table or any engine selection; T016 owns `internal/engine/router.go`.
- Do NOT store an infohash or touch `internal/store`; T017 owns the `tasks` columns and T100 owns
  duplicate detection across `infohash_v1` and `infohash_v2`.
- Do NOT add SSRF checks on the recovered URL; T054 owns `internal/secure/ssrf.go`.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
`make lint && make test PKG=./internal/uri/...`:

```
$ make lint
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!

$ make test PKG=./internal/uri/...
go test -race -count=1 ./internal/uri/...
ok  	github.com/L-K-M/dl-tool/internal/uri	1.033s
```

`make lint` printed no findings (`0 issues.`, eslint and prettier silent). One `ok` line, no `FAIL`, no
`[no test files]`. With `-v`, `TestDecodeObfuscated`, `TestNormalizeClassifies`, `TestParseMagnet` and
`TestParseED2K` all run and pass (11 + 14 + 10 + 6 subtests).

Scope check (files staged for the task commit):

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
internal/uri/magnet.go
internal/uri/normalize.go
internal/uri/normalize_test.go
internal/uri/obfuscated.go
```

Exactly the Files table, nothing else.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

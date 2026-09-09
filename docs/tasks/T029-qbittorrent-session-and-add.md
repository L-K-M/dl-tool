# T029 — Implement the qBittorrent session, state mapping and `torrents/add`

| Field | Value |
|---|---|
| **ID** | T029 |
| **Milestone** | M2 |
| **Status** | done |
| **Depends on** | T005, T016, T019, T027 |
| **Blocks** | T030, T032, T034, T035, T036, T037, T038, T100, T101 |
| **Parallel-safe** | yes — every file it touches is new |
| **Implements** | the engine half of [FR-005](../02-requirements.md#fr-005-add-tasks-from-an-uploaded-file), [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine), [FR-032](../02-requirements.md#fr-032-propagate-category-and-tags-to-capable-engines) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~430 LOC |

## Goal
`qbittorrent.Client` logs in, holds its session cookie in a cookie jar, probes the version, normalises every qBittorrent
state, and adds a torrent from a magnet, a `.torrent` URL or raw `.torrent` bytes. It is registered in the
engine registry under `qbittorrent`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §5 qBittorrent adapter](../06-download-engines.md#5-qbittorrent-adapter-internalengineqbittorrent)
2. [`docs/06-download-engines.md` §5.1 Version probe](../06-download-engines.md#51-version-probe)
3. [`docs/06-download-engines.md` §5.2 Login and the session cookie](../06-download-engines.md#52-login-and-the-session-cookie)
4. [`docs/06-download-engines.md` §5.3 `torrents/add`](../06-download-engines.md#53-torrentsadd)
5. [`docs/06-download-engines.md` §5.5 `torrents/info`](../06-download-engines.md#55-torrentsinfo)
6. [`docs/06-download-engines.md` §5.6 State normalisation](../06-download-engines.md#56-state-normalisation--reproduce-exactly-accept-both-spellings)
7. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
8. [`docs/11-config-reference.md` §2 `DLTOOL_` variables](../11-config-reference.md#2-dltool_-variables-application)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/qbittorrent/client.go` | create | Transport, login, version probe, `Add`, `Pause`, `Resume`, `Remove`. |
| `internal/engine/qbittorrent/map.go` | create | `normaliseState`, `torrentJSON`, `toTaskInfo`. |
| `internal/engine/qbittorrent/client_test.go` | create | `httptest` cases plus the state-normalisation table test. |

No other file may be modified.

## Interface contract

```go
package qbittorrent

// Config is the adapter's construction input, from DLTOOL_QBITTORRENT_URL, _USERNAME and _PASSWORD.
type Config struct {
	BaseURL  string        // e.g. http://qbittorrent:8080 — no trailing slash, no /api/v2 suffix
	Username string
	Password string
	Timeout  time.Duration // per-call deadline
}

// Client implements engine.Engine over the qBittorrent WebAPI v2 (target 5.2.3).
type Client struct{ /* unexported */ }

// New returns a Client. It performs no I/O; call Connect first.
func New(cfg Config, hc *http.Client) (*Client, error)

func (c *Client) Name() string                       // engine.NameQBittorrent
func (c *Client) Capabilities() []engine.Capability
func (c *Client) Accepts(uriStr string) bool
func (c *Client) Connect(ctx context.Context) error  // login, then app/version and app/webapiVersion
func (c *Client) Close() error
func (c *Client) Health(ctx context.Context) (string, error) // "v5.2.3"; ErrUnavailable when down
func (c *Client) Add(ctx context.Context, req engine.AddRequest) (string, error)
func (c *Client) Pause(ctx context.Context, id string) error
func (c *Client) Resume(ctx context.Context, id string) error
func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error

// login posts username and password as application/x-www-form-urlencoded to auth/login with a Referer
// matching the request's own Host. The cookie jar captures the response cookie; never parse, rename or
// copy it by hand — 5.2.3 names it QBT_SID_<WebUI port>, not SID, and the name is not part of the
// contract. Login has succeeded only once the jar holds a cookie AND an authenticated GET app/version
// returns 200; neither the login status nor its body proves the session works (06 §5.2).
func (c *Client) login(ctx context.Context) error

// do performs one authenticated call, POST when mutating and GET otherwise. It re-logs in and retries
// exactly once on 401 or 403, and logs the response body on 401 so a narrowed ServerDomains or a
// mismatched Host port is diagnosable.
func (c *Client) do(ctx context.Context, method, apiPath string, form url.Values) ([]byte, error)
```

```go
package qbittorrent

// normaliseState maps a qBittorrent state onto the canonical TaskState of 06 §5.6. progress is needed
// because pausedUP/stoppedUP is completed at progress == 1 and paused otherwise. An unrecognised state
// returns engine.StateQueued and logs one warning; it never returns an error and never panics.
func normaliseState(state string, progress float64) engine.TaskState

// torrentJSON is one element of torrents/info and one value of the sync/maindata torrents object.
type torrentJSON struct {
	Hash          string   `json:"hash"`
	InfohashV1    string   `json:"infohash_v1"`
	InfohashV2    string   `json:"infohash_v2"`
	HasMetadata   bool     `json:"has_metadata"`
	Name          string   `json:"name"`
	State         string   `json:"state"`
	Progress      float64  `json:"progress"`
	Size          int64    `json:"size"`        // selected files only
	TotalSize     int64    `json:"total_size"`  // including unselected
	Completed     int64    `json:"completed"`
	Uploaded      int64    `json:"uploaded"`
	DlSpeed       int64    `json:"dlspeed"`
	UpSpeed       int64    `json:"upspeed"`
	ETA           int64    `json:"eta"`
	Ratio         float64  `json:"ratio"`
	SavePath      string   `json:"save_path"`
	ContentPath   string   `json:"content_path"`
	Category      string   `json:"category"`
	Tags          string   `json:"tags"`        // comma-concatenated
	NumSeeds      int      `json:"num_seeds"`
	NumComplete   int      `json:"num_complete"`
	NumLeechs     int      `json:"num_leechs"`
	NumIncomplete int      `json:"num_incomplete"`
	AddedOn       int64    `json:"added_on"`
	CompletionOn  int64    `json:"completion_on"`
	DlLimit       int64    `json:"dl_limit"`
	UpLimit       int64    `json:"up_limit"`
	SeqDl         bool     `json:"seq_dl"`
	AutoTMM       bool     `json:"auto_tmm"`
	Private       *bool    `json:"private"`     // tri-state: null until metadata arrives
}

// toTaskInfo projects one torrentJSON onto engine.TaskInfo. ID is "qbittorrent:" + Hash, TotalBytes is
// nil while HasMetadata is false, and InfohashV1/InfohashV2 come from the infohash_v1/infohash_v2 keys,
// never from Hash.
func toTaskInfo(t torrentJSON) engine.TaskInfo
```

Declared capabilities, exactly this set and no other: `bittorrent`, `magnet`, `bt_v2`,
`per_file_select`, `per_file_priority`, `categories`, `tags`, `sequential`, `set_location`, `rename`,
`share_limits`.

## Steps
1. Create `internal/engine/qbittorrent/client.go` with `Config`, `Client` and `New`, storing the base URL,
   the credentials, an injected `*http.Client` carrying a `cookiejar.Jar`, and a mutex-guarded cache of the
   resolved `app/version`.
2. Implement `login` and `do` exactly as in the Interface contract. Every path is `"/api/v2/" + apiPath`;
   send `Referer` equal to `scheme://host[:port]` of the request's own URL on every call.
3. Implement `Connect` as `login` followed by `GET app/version` and `GET app/webapiVersion`, storing both;
   `Health` returns the cached version, re-probing when it is empty, and returns `engine.ErrUnavailable`
   for any transport failure.
4. Implement `Accepts`: `magnet:` URIs, a URL whose path ends `.torrent`, and a bare 40-hex or 64-hex
   infohash — the row 2 vocabulary of [`06` §2](../06-download-engines.md#2-routing-table).
5. Create `map.go` with `normaliseState` covering every row of 06 §5.6, both the `paused*` and the
   `stopped*` spellings, and the `unknown`/unrecognised fallback to `queued` with one `slog.Warn`.
6. Add `toTaskInfo` in `map.go`, converting `added_on` and `completion_on` seconds to `*time.Time`, `eta`
   to `*int64` (nil when it is qBittorrent's 8 640 000 sentinel), and the comma-joined `tags` to a slice.
7. Implement `Add` as `POST torrents/add` with `Content-Type: multipart/form-data`: `urls` for
   `req.URIs` newline-separated, one `torrents` part per `req.Blob` with
   `Content-Type: application/x-bittorrent`, `savepath`, `category`, comma-separated `tags`,
   `sequentialDownload`, and `autoTMM=false` explicitly.
8. Send **both** spellings of every renamed parameter with the same value — `stopped` and `paused` from
   `req.StartPaused`, `skip_checking` and `seedMode`, `contentLayout` and `root_folder` — because unknown
   parameters are ignored. Map the four documented statuses of 06 §5.3: `200` and `202` are decoded as the
   JSON result, `409` is a wrapped error saying every submission failed, `415` a wrapped error naming the
   invalid torrent.
9. Resolve the id from the `added_torrent_ids` of that result (06 §5.3), not from the torrent list. For an
   immediate single add require exactly one returned id and assert it equals the identity resolved before
   the add; for a `202` pending add retain the expected identity and let T030 reconcile it when it appears.
   Return `"qbittorrent:" + hash`. Malformed JSON, inconsistent counts or an unexpected id is a protocol
   error — never infer success from a 2xx status, and never recover identity by diffing `torrents/info`.
10. Implement `Pause`, `Resume` and `Remove` on `hashes=`: probe once with the 5.x pair
    `torrents/stop` and `torrents/start`, retry with `torrents/pause` and `torrents/resume` on `404`, and
    cache which pair the daemon answers. `Remove` sends `deleteFiles`.
11. Create `client_test.go` with an `httptest.Server` covering: login succeeding on `204` whose
    `Set-Cookie` names the cookie `QBT_SID_8080`, login failing on `401`, a `204` that sets no cookie
    being treated as a failure, one `401` mid-session triggering exactly one re-login and retry, the
    `Referer` header being present, `Add` sending both `stopped` and `paused`, `Add` returning the id from
    `added_torrent_ids` and rejecting a body whose counts disagree, the `404` fallback from
    `torrents/stop` to `torrents/pause`, and a table test driving every state of 06 §5.6.
12. Register the client nowhere yet: `*Client` does not satisfy `engine.Engine` until T038 adds the last
    method, so a `Register` call here cannot compile. T038 owns the `internal/api/server.go` edit.

## Acceptance criteria
- [x] Login succeeds only when the jar captured a cookie and an authenticated `GET app/version` returned
      200; the adapter never names the cookie itself.
- [x] `Add` takes its engine reference from `added_torrent_ids` and never from a `torrents/info` diff.
- [x] A single `401` mid-session causes exactly one re-login and one retry, never a loop.
- [x] `Add` sends `stopped` and `paused` with the same value, and `autoTMM=false`.
- [x] `normaliseState` returns the documented value for all 21 spellings and `queued` plus one warning for
      an unknown one.
- [x] `Capabilities()` returns exactly the eleven names listed above, sorted and stable.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/qbittorrent/...
```
Expected: `make lint` prints nothing, then
`ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent` with `TestLoginAcceptsNoContent`,
`TestRetriesOnceOn401`, `TestAddSendsBothPausedSpellings`, `TestPauseFallsBackTo4x` and
`TestNormaliseState` all `PASS`. No `FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT implement `List`, `Get` or `Events`; T030 owns `sync/maindata` and the delta cache.
- Do NOT implement `Files` or `SetFiles`; T032 owns per-file selection and priorities.
- Do NOT implement trackers or peers; T034 and T035 own them.
- Do NOT implement `SetLocation`, `Rename`, `SetCategory` or `SetShareLimits`; T036 owns the mutators, and
  T037 owns `SetRateLimits`. Leave them absent until then — do not add a stub that returns nil.
- Do NOT call any `search/*` or `rss/*` endpoint, ever.
- Do NOT read or write `app/preferences`; T101 owns boot conformance.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

`make lint && make test PKG=./internal/engine/qbittorrent/...`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint

> lint
> eslint .

cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	1.166s
```

All 17 tests `PASS`, including the five the block names: `TestLoginAcceptsNoContent`,
`TestRetriesOnceOn401`, `TestAddSendsBothPausedSpellings`, `TestPauseFallsBackTo4x`,
`TestNormaliseState` (`go test -count=1 -v` printed 17 `--- PASS` lines; no `FAIL`).
Full `make test` green (all 12 Go packages `ok`, web suite 13/13), `make vet` clean.

Scope check, `git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort`:

```
go.mod
go.sum
internal/engine/qbittorrent/client.go
internal/engine/qbittorrent/client_test.go
internal/engine/qbittorrent/map.go
```

Exactly the Files table, plus `go.mod`/`go.sum` under the standing exception for the
first import of an already-pinned dependency. `go mod tidy` did three things, verified
with `go mod tidy -diff` (empty), `go mod verify` (all modules verified) and `go mod why -m`:
`github.com/anacrolix/torrent v1.61.0` became direct (first import); ten further modules
were reclassified from `// indirect` to direct with **no version change** — huma/v2, chi/v5,
sqlx, tint, ulid/v2, goose/v3, cobra, x/crypto, x/sys, modernc.org/sqlite — all stale
annotations for packages earlier tasks already import; and tidy pruned T004's transitive
pins of the full anacrolix closure plus other unneeded requirements (pion/*, gofeed,
zombiezen/go/sqlite, go-llsqlite/*, …), all of which `go mod why -m` reports the main
module does not need. No module version changed and no module was added.

Note for T100 and docs/06 §3.5: `expectedTorrentID` keys a hybrid torrent on the 40-hex truncation of
its v2 hash, not on `infohash_v1` as docs/06 §3.5's table says — verified against
libtorrent RC_2_0 `info_hash_t::get_best()` and release-5.2.3 `InfoHash::toTorrentID()`;
see the comment on `expectedTorrentID` and the PR description.

### Repair: immediate single add must return exactly one id (post-merge)

The verifier of 2026-09-08 found `decodeAddResult` accepted a single-magnet `200` body of
`success_count=0, pending_count=0, failure_count=1, added_torrent_ids=[]` and returned the expected id
with a nil error. Reproduced first (`TestAddResolvesIDAndRejectsBadCounts/immediate_add_reports_failure`
failed with "An error is expected but got nil"), then fixed: a reply whose every outcome is a failure is a refusal on either status and any submission
count — one URI or several, or a blob+URI add whose expected id is the blob's hash — and a single
submission never names more than one added id, while a `200` reply — at least one immediate success,
nothing pending (06 §5.3) — must carry exactly one. The id-count guard runs before the success-count
check so both violations report the sharper message, and `single submission names two ids` fails if its
clause is removed.

`make lint && make test PKG=./internal/engine/qbittorrent/...` on `fix/t029-single-add-id`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint
> lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	1.192s
```

### Repair: a pending .torrent URL add retains its identity (post-merge)

The verifier's earlier finding — that a pending `.torrent` URL add must retain an
identity — was first declined claiming only the UNVERIFIED T038-owned
`fetchMetadata`/`parseMetadata` endpoints could resolve it. That decline was wrong:
06 §5.3 names the local §3.4 parser as an equal path, and it is pinned and already
used for blobs. Reproduced first: `TestAddPendingTorrentURLRetainsIdentity` failed
with "pending add of a uri whose identity is not locally resolvable" and the
daemon-id disagreement subtest expectedly returned nil, because no fetch was ever
attempted.

Fixed: `expectedTorrentID` became a client method; for one `.torrent` URL it now
fetches the bytes through the injected `*http.Client` under the per-call timeout,
caps the body like every other reply, and hashes it with `blobTorrentID` — identity
resolved before the submission, so a `202` pending add retains it and an immediate
add's daemon id is verified against it. A fetch that fails leaves the identity
honestly unresolved (one `slog.Warn`, no URL in the log: it can carry a tracker
token): the daemon fetches `urls` itself, so the submission proceeds and only a
pending outcome without a retained id reports the explicit error, as before.
Identity is never guessed and `torrents/info` is never diffed.

`make lint && make test PKG=./internal/engine/qbittorrent/...` on `fix/t029-pending-url-identity`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint
> lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	4.551s
```

Full `go test -mod=readonly -count=1 -race ./...`: all 12 packages `ok`, no `FAIL`.
`make vet` clean. The independent audit harness
(`/tmp/dltool-legacy-audit.CCQHQq/audit_pending_test.go`, added as
`zz_audit_pending_test.go` through a `-overlay` mapping) now passes:
`TestAuditPendingTorrentURLRetainsIdentity --- PASS`, returning
`qbittorrent:b2ff1d0b915849a3afe5f6de49ae4828299ddc8b` with `error=<nil>`.

### Repair: a failed pre-resolution aborts the add before submission (post-merge)

The audit of 2026-09-08 (recovery gate, `recovery/dltool-13`) found the previous
repair incomplete: `urlTorrentID` still discarded a failed prefetch with one
`slog.Warn` and let the submission proceed, so a daemon-accepted `202` add returned
`id=""` and `identity is not locally resolvable` — an accepted task nobody could
reference, violating 06 §5.3's resolve-before-add and T029 step 9. It also found the
warning's wrapped error carried the `*url.Error` message, whose URL embeds the query,
leaking a tracker passkey — the previous section's `no URL in the log` claim was wrong
for transport failures; that description is superseded by this repair.

Reproduced first: `TestAddTorrentURLPrefetchFailureAbortsBeforeSubmission` (metadata
fixture 503 to the client, valid bytes to the daemon, daemon set to accept `202`)
failed with the daemon recording one `torrents/add` and `Add` returning
`identity is not locally resolvable`;
`TestAddTorrentURLFetchFailureRedactsQuerySecret` failed with the passkey present in
the captured log; `TestAddTorrentURLPrefetchNotFoundAborts` replaced
`TestAddPendingURLHasNoIdentity`, which had codified the rejected
submit-then-decode-error behavior, and failed likewise.

Fixed: `urlTorrentID` now returns the fetch error and `Add` aborts before the
submission — the daemon never receives an add whose identity is unknown, so a
pre-resolution failure cannot lose an accepted task; callers retry the whole add.
`redactURL` rebuilds the `*url.Error` with doc 11's `__redacted__` placeholder in
place of the URL, so the returned error keeps the type, the cause and `net.Error`'s
Timeout/Temporary delegation but never the URL (docs/14 §3.3). URIs that are not
http(s) `.torrent` URLs still resolve to `""` without an error: 06 §2's routing
table sends every other http(s) URL to aria2, so they are outside this engine's
lane; if one is forced through anyway, the unresolved-pending error in
`decodeAddResult` remains for the shapes that reach it (a multi-URI pending add,
an xt-less magnet). Known tradeoff, accepted per 06 §5.3's resolve-before-add: a
metadata server hostile to the prefetch (UA filter, one-time token) now fails the
add up front instead of leaving an unreferencable pending task.

`make lint && make test PKG=./internal/engine/qbittorrent/...` on `fix/t029-prefetch-abort`:

```
test -z "$(gofmt -l cmd internal)"
golangci-lint run ./...
0 issues.
cd web && npm run lint
> lint
> eslint .
cd web && npx prettier --check .
Checking formatting...
All matched files use Prettier code style!
go test -race -count=1 ./internal/engine/qbittorrent/...
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	4.709s
```

Full `go test -mod=readonly -race -count=1 ./...`: all 12 packages `ok`, no `FAIL`.
`make vet` and `make doclint` clean. The audit's control harness
(`audit_pending_test.go` through a `-overlay` mapping) still passes:
`TestAuditPendingTorrentURLRetainsIdentity --- PASS`,
`id="qbittorrent:b2ff1d0b915849a3afe5f6de49ae4828299ddc8b"`, `error=<nil>`.

Review round 1 (GLM 5.3) on the first push: its Major — extension-less http URLs
still submit unresolved — was declined with 06 §2's routing table as evidence: row 2
is the only lane that reaches `qbittorrent.Add`, and it admits `.torrent` paths
alone; every other http(s) URL is row 4, aria2's lane. The misleading doc-comment
clause it quoted was corrected instead. Its Minor was accepted: `redactURL` now
preserves the `*url.Error` type with doc 11's `__redacted__` placeholder, keeping
`net.Error` timeout classification for callers (`TestRedactURL`). Its Info is
recorded above as the known tradeoff.

### Repair: a redirect's passkey never reaches the returned error (post-merge)

The verification at 097b14a (recovery gate, `recovery/dltool-15`) found one more leak
path: `redactURL` blanked the outer `*url.Error` URL but returned `ue.Err` untouched,
and when a .torrent URL redirects to a malformed `Location` net/http fails the fetch
with an inner error whose cached text quotes the raw Location — passkey included —
twice (`failed to parse Location header "/dl%zz.torrent?passkey=…": parse …`). Both
the returned error and any upstream `slog` line carried the secret, violating
docs/14 §§2.2,3.3 and docs/11 §6 and contradicting the section above.

Reproduced first: `TestAddTorrentURLRedirectFailureRedactsQuerySecret` (metadata
fixture answers `302` with `Location: /invalid%zz.torrent?passkey=t029-secret-token`,
daemon set to accept `202`) failed on `main` with
`should not contain "passkey="` — the full token was in the returned error — while
issuing zero `torrents/add` requests.

Fixed: `redactURL` now rebuilds the chain below the wrapper through `redactSecrets`.
A `*url.Error` node keeps its type with doc 11's `__redacted__` in place of its URL
(so `net.Error`'s Timeout/Temporary delegation and `errors.As` keep working); a node
whose rendered text already matches no secret query parameter is kept untouched —
a wrapper's cached message inlines its children, so a clean parent implies a clean
subtree and `errors.Is` to sentinel causes like `os.ErrDeadlineExceeded` survives
(`TestRedactURL`); a leaking node is replaced by a copy whose every
`apikey|token|passkey` value is the placeholder — the parameter name stays, the same
shape `redactedRequestURI` logs — because no structural edit can clean text already
formatted into a cached message.

`go test -mod=readonly -count=1 -run '^TestAddTorrentURLRedirectFailureRedactsQuerySecret$' ./internal/engine/qbittorrent`:

```
=== RUN   TestAddTorrentURLRedirectFailureRedactsQuerySecret
--- PASS: TestAddTorrentURLRedirectFailureRedactsQuerySecret (0.00s)
PASS
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	0.013s
```

`go test -mod=readonly -race -count=1 ./internal/engine/qbittorrent/...`:

```
ok  	github.com/L-K-M/dl-tool/internal/engine/qbittorrent	4.685s
```

Full `go test -mod=readonly -race -count=1 ./...`: all 12 packages `ok`, no `FAIL`.
`test -z "$(gofmt -l cmd internal)"`, `golangci-lint run
./internal/engine/qbittorrent/...` (`0 issues.`), `go vet ./...` and `make doclint`
(2381 total, 0 errors) clean; web lint not runnable locally (no eslint) — unchanged
files, CI covers it.

Review round 1 (GLM 5.3) suggested one change, accepted: the non-`*url.Error`
fallback in `redactURL` now routes through `redactSecrets` too, so a dirty chain
that never carried a URL node is sanitized instead of passed through — clean
chains still return the identical error (`TestRedactURL`'s `require.Same`).

### Repair: encoded passkey spellings and URL passwords (post-merge)

The verification at 126c55e (recovery gate, `recovery/dltool-17`, probe
`TestAuditTorrentRedirectFailureRedactsSecrets`) found the literal pattern
`apikey|token|passkey=` still leaked two secret shapes a malformed redirect
Location can carry: a percent-encoded key (`?%70asskey=…` for `passkey`) and a
URL userinfo password (`http://alice:…@host`), both quoted verbatim inside
`failed to parse Location header` and its nested `parse` error, in Add's
returned error and any upstream `slog` line.

Reproduced first: `TestAddTorrentURLRedirectFailureRedactsEncodedPasskey` and
`TestAddTorrentURLRedirectFailureRedactsUserinfoPassword` (metadata fixture
answers `302` with the secret-bearing Location; daemon set to accept `202`)
failed on `main` with `should not contain "t029-secret-token"` /
`should not contain "t029-secret-password"` while issuing zero `torrents/add`
requests.

Fixed: the literal-name pattern is replaced by `sanitizeSecretText`
(commit `00a5f90`, with the review round-1 additions below on the same
branch). `queryPairPattern` captures each rendered `key=value` pair (its key
class forbids URL structural bytes, so a candidate never spans scheme or host)
and `isSecretParamKey` compares the percent-decoded whole key with the doc 11
§6 names case-insensitively — the same whole-key rule internal/api's
`isSecretQueryParameter` applies, so `x-apikey` is deliberately untouched —
keeping the rendered spelling and substituting `__redacted__` for the value,
the same shape `redactedRequestURI` logs. The userinfo pass runs first
(review round 3 below: a rendered password may itself carry `&` and `=`);
`userinfoSpanPattern` finds
each URL's userinfo — schemed and RFC 3986 network-path (`//user@host`)
alike — up to its last `@` (the split net/url itself makes, so a password
containing `@` cannot survive in the span tail), its class stopping at `/`,
`?` and `#` so it never runs past an authority into path or query; and
`redactUserinfoPassword` substitutes the doc 11 placeholder for the password,
keeping the user name; a span without a password passes through untouched.
A leaking node is now replaced by a `redactedError` that renders the
sanitized text but still answers `errors.Is` for sentinel causes wrapped
under it, so clean and leaking nodes alike keep timeout/cancellation
classification (`TestRedactURL`, `TestRedactSecretsKeepsSentinelMatching`).

`go test -mod=readonly -count=1 -run '^TestAddTorrentURLRedirectFailureRedacts(EncodedPasskey|UserinfoPassword)$' ./internal/engine/qbittorrent` before the fix:

```
--- FAIL: TestAddTorrentURLRedirectFailureRedactsEncodedPasskey (0.00s)
        Error: "qbittorrent: fetch torrent url: Get \"__redacted__\": failed to parse Location header \"/invalid%zz.torrent?%70asskey=t029-secret-token\": parse \"/invalid%zz.torrent?%70asskey=t029-secret-token\": invalid URL escape \"%zz\"" should not contain "t029-secret-token"
--- FAIL: TestAddTorrentURLRedirectFailureRedactsUserinfoPassword (0.00s)
        Error: "qbittorrent: fetch torrent url: Get \"__redacted__\": failed to parse Location header \"http://alice:t029-secret-password@127.0.0.1:1/invalid%zz.torrent\": parse \"http://alice:t029-secret-password@127.0.0.1:1/invalid%zz.torrent\": invalid URL escape \"%zz\"" should not contain "t029-secret-password"
FAIL
```

After the fix, `go test -mod=readonly -race -count=1 ./...`:

```
ok  github.com/L-K-M/dl-tool/internal/api        48.950s
ok  github.com/L-K-M/dl-tool/internal/config      1.120s
ok  github.com/L-K-M/dl-tool/internal/engine      21.120s
ok  github.com/L-K-M/dl-tool/internal/engine/aria2        3.195s
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent 4.584s
ok  github.com/L-K-M/dl-tool/internal/fsx      1.031s
ok  github.com/L-K-M/dl-tool/internal/jobs      4.475s
ok  github.com/L-K-M/dl-tool/internal/obs      1.175s
ok  github.com/L-K-M/dl-tool/internal/secure    4.096s
ok  github.com/L-K-M/dl-tool/internal/store     66.845s
ok  github.com/L-K-M/dl-tool/internal/sync      4.382s
ok  github.com/L-K-M/dl-tool/internal/uri       1.037s
```

`test -z "$(gofmt -l cmd internal)"` clean, `golangci-lint run
./internal/engine/qbittorrent/...` (`0 issues.`), `go vet ./...` and `make doclint`
(2381 total, 0 errors) clean; web lint not runnable locally (no eslint) — unchanged
files, CI covers it.

Review round 1 (GLM 5.3) found the scheme-relative gap before merge: a
`Location: //alice:…@host/…` (RFC 3986 network-path reference, no scheme)
quoted its password verbatim in the same parse failure, because the span
pattern anchored on `scheme://`. Reproduced on the round-1 commit with
`TestAddTorrentURLRedirectFailureRedactsSchemeRelativeUserinfo`, then fixed
by matching both shapes. Also accepted: the span class now stops at `?` and
`#` so a path-less URL's query cannot be swallowed into a fabricated
user:password pair (`TestSanitizeSecretTextLeavesInnocentURLs`); leaking
nodes keep `errors.Is` sentinel matching through `redactedError`
(`TestRedactSecretsKeepsSentinelMatching`); whole-key matching intent locked
(`TestSanitizeSecretTextWholeKeyMatching`); combined credentials + query
secret on one Location covered
(`TestAddTorrentURLRedirectFailureRedactsUserinfoAndQueryTogether`); the four
redirect-failure tests share one `requireRedirectLocationRedacted` helper.
Declined: a guard for a missing `"://"` in `redactUserinfoPassword` — the
span pattern guarantees every match begins `//` or `scheme://`, so the
authority offset is well defined by construction.

`go test -mod=readonly -race -count=1 ./internal/engine/...` after round 1:

```
ok  github.com/L-K-M/dl-tool/internal/engine              22.055s
ok  github.com/L-K-M/dl-tool/internal/engine/aria2        3.205s
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent 4.743s
```

`golangci-lint run ./internal/engine/qbittorrent/...` (`0 issues.`) and
`go vet ./...` clean.

Review round 2 (GLM 5.3, minor-only) accepted one fix: the
`requireRedirectLocationRedacted` helper now captures slog records at
`Level: slog.LevelDebug`, so its guard really holds for any future log
line the add path grows — a default-level handler would drop a secret
leaked at debug verbosity and pass vacuously (6176cbe). Declined with
evidence: `Timeout()`/`errors.As` forwarding on `redactedError` (no
caller classifies an Add error by timeout interface; `*url.Error` nodes
are rebuilt structurally and never wrapped) and a broader post-round-1
test command (every helper and test lives in
`internal/engine/qbittorrent`, which the recorded command runs).

Review round 3 (GLM 5.3) found a real pass-order leak: `net/url` renders
a password raw except `@ / ? : #`, so a password may itself carry `&`
and `=`. With the query pass first, `user:hunter2&token=x@host` had its
embedded `token=x` pair replaced, destroying the `@` the userinfo pass
needs and leaking the `hunter2&` fragment. Reproduced with
`TestSanitizeSecretTextRedactsUserinfoBeforeQueryPairs` (fails on 048daf7),
fixed by running the userinfo pass first (26c4ff6) — the span class stops
at `/ ? #` so it can never consume a query pair, and the password
redaction subsumes any secret-looking pair it swallows. Declined as
false: the reported `0-+` reversed character class — the pattern reads
`[a-zA-Z0-9+.-]` and the package compiles.

`go test -mod=readonly -race -count=1 ./internal/engine/...` after round 3:

```
ok  github.com/L-K-M/dl-tool/internal/engine              20.349s
ok  github.com/L-K-M/dl-tool/internal/engine/aria2        3.193s
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent 4.753s
```

`golangci-lint run ./internal/engine/qbittorrent/...` (`0 issues.`),
`go vet ./internal/...` and `make doclint` (2381 total, 0 errors) clean.

### Repair: apostrophes and escaped quotes are value bytes, not delimiters (post-merge)

The verification at a3e492c (recovery gate, `recovery/dltool-19`) found one
more delimiter flaw: both character classes excluded `'`, but an apostrophe
is an RFC 3986 sub-delim that neither net/url nor %q quoting escapes. A
Location like `http://al'ice:t029-'pw@host/…%zz…` matched no userinfo span
(whole password leaked), and `?passkey=prefix'audit-synthetic-passkey`
redacted only up to the apostrophe, leaving the `audit-synthetic-passkey`
suffix in the returned error and any upstream log (docs/14 §§2.2,3.3,
docs/11 §6).

Reproduced first: `TestAddTorrentURLRedirectFailureRedactsApostropheUserinfo`,
`…ApostrophePasskeyValue` and `…QuotedSecretBytes` fail on a3e492c — the
passkey case rendered `passkey=__redacted__'audit-synthetic-passkey`, the
suffix after the apostrophe surviving.

Fixed by making both value classes hold every byte a rendered value can
carry: `'` is ordinary, and a literal `"` inside %q-quoted text — the one
form a stop byte takes inside a value — is consumed by an escape
alternative listed first, so greedy matching prefers it. Only `&`,
whitespace and a closing `"` end a value; the userinfo span keeps its
`/ ? #` stops, so it still cannot run past an authority.
`TestSanitizeSecretTextTreatsApostrophesAndEscapesAsValueBytes` pins the
rendering-level rule; the innocent-URL tests are unchanged and still pass.

`go test -mod=readonly -race -count=1 ./internal/engine/qbittorrent`:

```
ok  github.com/L-K-M/dl-tool/internal/engine/qbittorrent 4.813s
```

`go test -mod=readonly -count=1 ./internal/...` all `ok`; `go vet` and
`golangci-lint run ./internal/engine/qbittorrent/...` (`0 issues.`) clean.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

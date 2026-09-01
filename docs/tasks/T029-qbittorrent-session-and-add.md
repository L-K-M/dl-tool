# T029 — Implement the qBittorrent session, state mapping and `torrents/add`

| Field | Value |
|---|---|
| **ID** | T029 |
| **Milestone** | M2 |
| **Status** | todo |
| **Depends on** | T005, T016, T019, T027 |
| **Blocks** | T030, T032, T034, T035, T036, T037, T038, T100, T101 |
| **Parallel-safe** | no — registers the engine in `internal/api/server.go` |
| **Implements** | the engine half of [FR-005](../02-requirements.md#fr-005-add-tasks-from-an-uploaded-file), [FR-011](../02-requirements.md#fr-011-maintain-the-canonical-task-state-machine), [FR-032](../02-requirements.md#fr-032-propagate-category-and-tags-to-capable-engines) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~430 LOC |

## Goal
`qbittorrent.Client` logs in, holds the `SID` cookie, probes the version, normalises every qBittorrent
state, and adds a torrent from a magnet, a `.torrent` URL or raw `.torrent` bytes. It is registered in the
engine registry under `qbittorrent`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §5 qBittorrent adapter](../06-download-engines.md#5-qbittorrent-adapter-internalengineqbittorrent)
2. [`docs/06-download-engines.md` §5.1 Version probe](../06-download-engines.md#51-version-probe)
3. [`docs/06-download-engines.md` §5.2 Login and the SID cookie](../06-download-engines.md#52-login-and-the-sid-cookie)
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
| `internal/api/server.go` | modify | Construct the client from config and register it in the engine registry. |

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
// matching the request's own Host. Success is the presence of a Set-Cookie: SID= header, never the
// status code: 5.2.x answers 204 and 4.x answers 200 with the body "Ok.".
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
   parameters are ignored. Map `415` to a wrapped error naming the invalid torrent.
9. Resolve the id after adding, because `torrents/add` returns no hash: when the submission carries a
   known infohash use it directly, otherwise diff the `torrents/info` hash set captured immediately before
   the add, polling every 200 ms for at most 5 s. Return `"qbittorrent:" + hash`.
10. Implement `Pause`, `Resume` and `Remove` on `hashes=`: probe once with the 5.x pair
    `torrents/stop` and `torrents/start`, retry with `torrents/pause` and `torrents/resume` on `404`, and
    cache which pair the daemon answers. `Remove` sends `deleteFiles`.
11. Create `client_test.go` with an `httptest.Server` covering: login succeeding on `204` with
    `Set-Cookie: SID=`, login failing on `401`, one `401` mid-session triggering exactly one re-login and
    retry, the `Referer` header being present, `Add` sending both `stopped` and `paused`, the `404`
    fallback from `torrents/stop` to `torrents/pause`, and a table test driving every state of 06 §5.6.
12. Edit `internal/api/server.go` to build the client from `cfg.QBittorrentURL`, `_USERNAME` and
    `_PASSWORD` and `Register` it beside aria2; skip registration when the URL is empty.

## Acceptance criteria
- [ ] Login is judged only by the `Set-Cookie: SID=` header; a `204` response is a success.
- [ ] A single `401` mid-session causes exactly one re-login and one retry, never a loop.
- [ ] `Add` sends `stopped` and `paused` with the same value, and `autoTMM=false`.
- [ ] `normaliseState` returns the documented value for all 21 spellings and `queued` plus one warning for
      an unknown one.
- [ ] `Capabilities()` returns exactly the eleven names listed above, sorted and stable.
- [ ] `Registry.Get("qbittorrent")` returns the client after `NewServer` with a configured URL.

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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

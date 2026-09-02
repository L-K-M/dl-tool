# T019 — Implement the aria2 adapter and the engine registry

| Field | Value |
|---|---|
| **ID** | T019 |
| **Milestone** | M1 |
| **Status** | todo |
| **Depends on** | T016, T018 |
| **Blocks** | T020, T022, T026, T027, T028, T029, T079, T098, T101 |
| **Parallel-safe** | no — adds `internal/engine/registry.go` beside T016's files |
| **Implements** | infrastructure for [FR-001](../02-requirements.md#fr-001-add-tasks-from-a-batch-of-pasted-uris) and [FR-014](../02-requirements.md#fr-014-apply-lifecycle-and-queue-actions-to-a-selection) |
| **Decisions** | [ADR-0005](../decisions/0005-aria2-qbittorrent-ytdlp-engines.md), [ADR-0017](../decisions/0017-exclusive-control-of-engines.md) |
| **Est. size** | 3 new files, ~400 LOC |

## Goal
`aria2.Client` implements `engine.Engine` over JSON-RPC 2.0 for HTTP, FTP, SFTP and Metalink transfers, and
`engine.Registry` holds it under the name `aria2`. Every call sends the secret as the first positional
parameter `token:<secret>`.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/06-download-engines.md` §1 The Engine interface](../06-download-engines.md#1-the-engine-interface)
2. [`docs/06-download-engines.md` §4.2 Endpoint and the `token:` convention](../06-download-engines.md#42-endpoint-and-the-token-convention)
3. [`docs/06-download-engines.md` §4.3 Methods dl-tool calls](../06-download-engines.md#43-methods-dl-tool-calls)
4. [`docs/06-download-engines.md` §4.5 WebSocket notifications](../06-download-engines.md#45-websocket-notifications)
5. [`docs/06-download-engines.md` §8 Engine ownership](../06-download-engines.md#8-engine-ownership)
6. [`docs/11-config-reference.md` §2 `DLTOOL_` variables](../11-config-reference.md#2-dltool_-variables-application)

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/engine/aria2/client.go` | create | JSON-RPC transport and the `engine.Engine` implementation. |
| `internal/engine/aria2/client_test.go` | create | Unit tests against an `httptest` JSON-RPC server. |
| `internal/engine/registry.go` | create | `Registry`, `Register`, `Get`, `Names`. |

No other file may be modified.

## Interface contract

```go
package aria2

// Config is the adapter's construction input. URL is DLTOOL_ARIA2_URL, Secret is DLTOOL_ARIA2_SECRET.
type Config struct {
	URL     string        // e.g. http://aria2:6800/jsonrpc
	Secret  string        // sent as the first positional parameter, "token:" + Secret
	Timeout time.Duration // per-call deadline
}

// Client implements engine.Engine over aria2's JSON-RPC 2.0 endpoint.
type Client struct{ /* unexported */ }

// New returns a Client. It performs no I/O; call Connect first.
func New(cfg Config, hc *http.Client) (*Client, error)

func (c *Client) Name() string
func (c *Client) Capabilities() []engine.Capability
func (c *Client) Accepts(uriStr string) bool
func (c *Client) Connect(ctx context.Context) error
func (c *Client) Close() error
func (c *Client) Health(ctx context.Context) (string, error)
func (c *Client) Add(ctx context.Context, req engine.AddRequest) (string, error)
func (c *Client) List(ctx context.Context) ([]engine.TaskInfo, error)
func (c *Client) Get(ctx context.Context, id string) (engine.TaskInfo, error)
func (c *Client) Files(ctx context.Context, id string) ([]engine.FileEntry, error)
func (c *Client) Pause(ctx context.Context, id string) error
func (c *Client) Resume(ctx context.Context, id string) error
func (c *Client) Remove(ctx context.Context, id string, deleteData bool) error
func (c *Client) SetFiles(ctx context.Context, id string, selected []int, priorities map[int]int) error
func (c *Client) SetLocation(ctx context.Context, id, path string) error
func (c *Client) SetRateLimits(ctx context.Context, id string, down, up *int64) error
func (c *Client) Events(ctx context.Context) (<-chan engine.TaskEvent, error)
```

```go
package engine

// Registry holds one Engine per Name(). Adding an engine is one Register call.
type Registry struct{ /* unexported */ }

func NewRegistry() *Registry
func (r *Registry) Register(e Engine)
func (r *Registry) Get(name string) (Engine, bool)
func (r *Registry) Names() []string
```

Declared capabilities, exactly this set and no other:
`http`, `ftp`, `sftp`, `metalink`, `per_file_select`, `set_location`, `push_events`.

`CapRename` is **not** declared. aria2 can only name an output at add time through the `out` option, and
the interface's `Rename` renames a running transfer; declaring the capability while `Rename` always
returns `ErrNotSupported` is exactly what T028's `UnsupportedCapabilityReturnsErrNotSupported` subtest
fails on. `AddRequest.Filename` still maps to `out`.

Method mapping, exactly this and no other:

| `Engine` method | aria2 call |
|---|---|
| `Health` | `aria2.getVersion` |
| `Add` with URIs | `aria2.addUri([secret], uris, options)` — returns one GID string |
| `Add` with a Metalink blob | `aria2.addMetalink([secret], base64, options)` — returns an array of GIDs; record the first |
| `List` | one JSON-RPC batch of `aria2.tellActive`, `aria2.tellWaiting`, `aria2.tellStopped` |
| `Get` | `aria2.tellStatus([secret], gid, keys)` |
| `Files` | `aria2.getFiles([secret], gid)` |
| `Pause` | `aria2.pause([secret], gid)` |
| `Resume` | `aria2.unpause([secret], gid)` |
| `Remove` | `aria2.remove` then `aria2.removeDownloadResult` |
| `SetFiles` | `aria2.changeOption([secret], gid, {"select-file": "1,3,5"})` |
| `SetLocation` | `aria2.changeOption([secret], gid, {"dir": path})` |
| `SetRateLimits` per task | `aria2.changeOption([secret], gid, {"max-download-limit": …, "max-upload-limit": …})` |
| `SetRateLimits` global | `aria2.changeGlobalOption([secret], {"max-overall-download-limit": …, "max-overall-upload-limit": …})` |

## Steps
1. Create `internal/engine/registry.go` with `Registry`, `NewRegistry`, `Register`, `Get` and `Names`,
   guarded by a mutex and keyed on `Engine.Name()`.
2. Create `internal/engine/aria2/client.go` with `Config`, `Client` and `New`; store the base URL, the
   secret and an injected `*http.Client`.
3. Implement one private `call(ctx, method string, params ...any) (json.RawMessage, error)` that builds
   `{"jsonrpc":"2.0","id":…,"method":…,"params":…}`, prepends `"token:"+secret` as the first positional
   parameter, and maps a JSON-RPC error object to a wrapped error. `system.multicall` is never used.
4. Encode every option value as a **string** with the long option name and no leading `--`, for example
   `{"dir":"/data/iso","out":"file.iso","pause":"true"}`.
5. Implement `Health` over `aria2.getVersion`, returning `engine.ErrUnavailable` for a transport failure.
6. Implement `Add`: `addUri` for `req.URIs`, `addMetalink` for a `metalink` blob, `dir` from
   `req.SaveDir`, `out` from `req.Filename`, `pause` from `req.StartPaused`, `select-file` from
   `req.SelectFiles`. Return `engine.ErrNotSupported` for a `torrent` blob.
7. Implement `List` as one JSON-RPC batch array of the three `tell*` calls, decoding each result with
   `toTaskInfo` from T018, and return **every** GID the daemon reports. The adapter has no access to the
   `tasks` table, so it cannot tell a foreign transfer from one of ours; the foreign-transfer filter of
   [§8](../06-download-engines.md#8-engine-ownership) is applied by the reconciler (T026), which holds the
   `engine_ref` → task-id map. Do not add a store dependency to this package.
8. Implement `Get`, `Files`, `Pause`, `Resume` and `Remove`; map an aria2 "not found" error to
   `engine.ErrNotFound`.
9. Return `engine.ErrNotSupported` unchanged from `Rename`, `SetCategory`, `SetShareLimits`, and from
   `SetFiles` whenever its `priorities` map is non-nil.
10. Implement `Events` as a 1 Hz `aria2.tellActive` poll emitting `engine.TaskEvent` values; add the
    comment that the WebSocket notification transport is added by T026.
11. Create `internal/engine/aria2/client_test.go` with an `httptest.Server` that asserts the first
    positional parameter of every request is `token:s3cret`, then covers `Add`, `Get`, `Pause`, `Remove`,
    `SetFiles` with priorities, and a `503` mapping to `engine.ErrUnavailable`.

## Acceptance criteria
- [ ] Every request body carries `token:s3cret` as `params[0]`.
- [ ] `Add` with two URIs sends one `aria2.addUri` call and returns the GID string.
- [ ] `Capabilities()` returns exactly the seven names listed above, sorted and stable, and does **not**
      include `rename`.
- [ ] `SetFiles` with a non-nil `priorities` map returns `engine.ErrNotSupported` and sends no request.
- [ ] A transport failure returns `engine.ErrUnavailable`; an unknown GID returns `engine.ErrNotFound`.
- [ ] `Registry.Get("aria2")` returns the registered client and `Names()` includes `aria2`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make lint && make test PKG=./internal/engine/...
```
Expected: `make lint` prints nothing, then `ok` lines for
`github.com/L-K-M/dl-tool/internal/engine` and `github.com/L-K-M/dl-tool/internal/engine/aria2`, with
`TestCallSendsToken`, `TestAddURI`, `TestSetFilesRejectsPriorities` and `TestRegistry` all running. No
`FAIL`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT create `internal/engine/enginetest/contract.go` or `internal/engine/aria2/contract_test.go`; T028
  owns the shared adapter contract suite and the aria2 call site, and T038 adds the qBittorrent one.
- Do NOT add the boot conformance probe or raise `max-concurrent-downloads`; T101 owns conformance.
- Do NOT wire the client into the HTTP server; T027 step 8 owns building the registry in `NewServer`,
  constructing this client from `cfg.Aria2URL`, calling `Connect` and registering it.
- Do NOT add the WebSocket notification transport; T026 owns it.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

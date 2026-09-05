# T005 — Load the DLTOOL_ environment and generate the boot secrets

| Field | Value |
|---|---|
| **ID** | T005 |
| **Milestone** | M0 |
| **Status** | done |
| **Depends on** | T004 |
| **Blocks** | T006, T007, T029, T054, T056, T087, T092, T123 |
| **Parallel-safe** | no — it edits `cmd/dl-tool/main.go` |
| **Implements** | [FR-141](../02-requirements.md#fr-141-resolve-settings-from-environment-then-database), [NFR-023](../02-requirements.md#nfr-023-generate-secrets-on-first-run-and-support-file-based-secrets) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md), [ADR-0012](../decisions/0012-single-data-mount.md) |
| **Est. size** | 5 source files, ~1,050 added LOC |

## Goal
`config.Load` turns the `DLTOOL_*` environment into one validated struct, dies with a named `err_code` on
every fatal condition in [`11-config-reference.md`](../11-config-reference.md#8-boot-validation), and
generates `<CONFIG_DIR>/secrets.env` mode `0600` when a key is missing. Secret values never render in a log
line or an error string.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/11-config-reference.md` §2 `DLTOOL_` variables](../11-config-reference.md#2-dltool_-variables-application)
   — the authoritative name, type, default and category of every variable.
2. [`docs/11-config-reference.md` §1 Precedence rule](../11-config-reference.md#1-precedence-rule) —
   infrastructure reads the environment, preference reads the database.
3. [`docs/11-config-reference.md` §6 Secrets](../11-config-reference.md#6-secrets) — the `_FILE` convention
   and first-run secret generation.
4. [`docs/11-config-reference.md` §8 Boot validation](../11-config-reference.md#8-boot-validation) — the
   fatal and warn table with its `err_code` values.
5. [`docs/14-conventions.md` §2.2 Error model](../14-conventions.md#22-error-model).

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/config/config.go` | create | The `Config` struct, `Load`, and secrets-file generation. |
| `internal/config/env.go` | create | Typed readers for each variable and the validation table. |
| `internal/secure/secret.go` | create | `Secret`, a string that cannot be printed or marshalled. |
| `internal/config/config_test.go` | create | Table tests for defaults, `_FILE`, fatals and redaction. |
| `cmd/dl-tool/main.go` | edit | Call `config.Load` and feed `LogLevel`/`LogFormat` into `obs.NewLogger`. |

No other file may be modified.

## Interface contract

```go
package secure

// Secret is a string whose value never reaches a log, an error or an API response.
type Secret string

func (s Secret) String() string                { return "[REDACTED]" }
func (s Secret) Format(f fmt.State, verb rune) { io.WriteString(f, "[REDACTED]") }
func (s Secret) MarshalJSON() ([]byte, error)  { return []byte(`"[REDACTED]"`), nil }
func (s Secret) Reveal() string                { return string(s) }
```

```go
package config

type Config struct {
	HTTPAddr           string        // DLTOOL_HTTP_ADDR,           default ":8080"
	BasePath           string        // DLTOOL_BASE_PATH,           default ""
	AllowedHosts       []string      // DLTOOL_ALLOWED_HOSTS,       default empty, split on ","
	ConfigLock         bool          // DLTOOL_CONFIG_LOCK,         default false
	ConfigDir          string        // DLTOOL_CONFIG_DIR,          default "/config"
	DataRoots          []string      // DLTOOL_DATA_ROOTS,          default ["/data"], split on ":"
	DBPath             string        // DLTOOL_DB_PATH,             default "/config/dl-tool.db"
	LogLevel           string        // DLTOOL_LOG_LEVEL,           default "info"
	LogFormat          string        // DLTOOL_LOG_FORMAT,          default "json"
	TrustedProxies     []netip.Prefix// DLTOOL_TRUSTED_PROXIES,     default empty, split on ","
	SessionTTL         time.Duration // DLTOOL_SESSION_TTL,         default 720h
	MetricsAddr        string        // DLTOOL_METRICS_ADDR,        default "127.0.0.1:9090", "off" disables
	Aria2URL           string        // DLTOOL_ARIA2_URL
	Aria2Secret        secure.Secret // DLTOOL_ARIA2_SECRET
	QBittorrentURL     string        // DLTOOL_QBITTORRENT_URL
	QBittorrentUser    string        // DLTOOL_QBITTORRENT_USERNAME
	QBittorrentPass    secure.Secret // DLTOOL_QBITTORRENT_PASSWORD
	YtdlpPath          string        // DLTOOL_YTDLP_PATH,          default "/usr/local/bin/yt-dlp"
	JSRuntimePath      string        // DLTOOL_JS_RUNTIME_PATH,     default "/usr/bin/node"
	SevenzipPath       string        // DLTOOL_SEVENZIP_PATH,       default "/usr/bin/7zz"
	SSRFAllowPrivate   bool          // DLTOOL_SSRF_ALLOW_PRIVATE,  default false
	WatchDir           string        // DLTOOL_WATCH_DIR,           preference seed
	NotifyURL          string        // DLTOOL_NOTIFY_URL,          preference seed
	SecretKey          secure.Secret // secrets.env DLTOOL_SECRET_KEY, the at-rest encryption key
}

// Load reads the environment, validates it and generates any missing secret.
// It performs no network I/O. Every returned error wraps one FatalError.
func Load(ctx context.Context) (*Config, error)

// FatalError carries the err_code from 11-config-reference.md section 8.
type FatalError struct {
	Code     string // config_missing | config_conflict | config_secret_unreadable |
	                // config_malformed | config_path_unwritable | config_network_fs
	Variable string
	Detail   string
}

func (e *FatalError) Error() string
```

## Steps
1. Create `internal/secure/secret.go` exactly as above.
2. Create `internal/config/env.go` with one reader per type — `envString`, `envBool` via
   `strconv.ParseBool`, `envDuration` via `time.ParseDuration`, `envPathList` splitting on `:`,
   `envCIDRList` splitting on `,` — each returning a `*FatalError` with code `config_malformed`.
3. Add `envSecret(name string)`: read `name` and `name+"_FILE"`; both set is `config_conflict`; an unreadable
   file is `config_secret_unreadable`; strip one trailing newline from the file's contents.
4. Create `internal/config/config.go` with `Load`, filling every field above from its documented default,
   then running the validation table of doc 11 §8 in that order. An empty string means unset, never an empty
   value.
5. Implement the fatal checks: `config_missing` for the aria2 and qBittorrent credential pairs;
   `config_malformed` for a bad `host:port`, a `BasePath` without a leading `/` or with a trailing `/`, a
   `DLTOOL_ALLOWED_HOSTS` entry carrying a scheme, path, port or wildcard, and any non-absolute path;
   `config_path_unwritable` after one `MkdirAll` attempt on `ConfigDir` and the database directory. A data
   root is **not** in that list: doc 11 §8 makes a missing or unwritable data root a `warn` and forbids
   `MkdirAll` on it, because creating the directory would silently mask an unmounted volume.
6. Implement `writeSecrets`: read `<ConfigDir>/secrets.env`, and for each of `ARIA2_RPC_SECRET` and
   `DLTOOL_SECRET_KEY` that is missing, generate 32 bytes from `crypto/rand`, base64url-encode them, and
   rewrite the file with mode `0600`. Load `SecretKey` from it. There is no session or CSRF key: doc 11 §6
   states why neither exists.
7. Log every `warn` row of doc 11 §8 — a data root that is missing or not writable
   (`data_root_not_writable`, recorded on the root so `GET /system/info` and the UI banner can show it, and
   never `MkdirAll`-ed), a missing `yt-dlp`, JS runtime or `7zz` binary, a `WatchDir` outside every data
   root — and continue.
8. Edit `cmd/dl-tool/main.go` to call `config.Load`, exit 1 after logging one `error` record with the
   `err_code` attribute on failure, and pass `cfg.LogLevel` and `cfg.LogFormat` to `obs.NewLogger`.
9. Write `internal/config/config_test.go` covering: all defaults with an empty environment; `_FILE` preferred
   over its inline form; both forms set giving `config_conflict`; a malformed `BASE_PATH` giving
   `config_malformed`; two `Load` calls against two fresh directories giving different `SecretKey` values;
   and `fmt.Sprintf("%v", cfg.Aria2Secret)` returning `[REDACTED]`.
10. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [x] Every variable in doc 11 §2 has a field, a default and a test.
- [x] Each fatal row of doc 11 §8 returns a `*FatalError` with exactly the documented `err_code`.
- [x] `TestUnwritableDataRootIsNotFatal` shows `Load` succeeding with one `warn` carrying
      `err_code=data_root_not_writable` for a missing and for a read-only data root, and no `MkdirAll`
      attempt on either.
- [x] `TestSecretsAreDistinctPerInstance` shows two fresh config directories yielding different
      `DLTOOL_SECRET_KEY` values.
- [x] `TestSecretNeverPrints` asserts `%v`, `%s` and `json.Marshal` all render `[REDACTED]`.
- [x] `secrets.env` is written with mode `0600` and is not overwritten when it already holds both values.
- [x] `Load` opens no network connection and no database.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG=./internal/config/... && echo CONFIG_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/config` on its own line, no `FAIL`, and a final line of
exactly `CONFIG_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT read or write the database. The preference-category settings of doc 11 §5 are read from the
  `settings` table by T092; `Load` only seeds them on first boot, which T092 wires.
- Do NOT generate the one-time setup token; T009 owns it and its `<config>/setup-token` file.
- Do NOT detect the database directory's filesystem type; `config_network_fs` is raised by `store.Open` in
  T006.
- Do NOT create `internal/secure/session.go`, `csrf.go`, `hash.go` or `ssrf.go`.
- Do NOT invent a variable that is not in doc 11 §2; that table is the single source of truth.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
```text
$ make test PKG=./internal/config/... && echo CONFIG_OK
go test -race -count=1 ./internal/config/...
ok  	github.com/L-K-M/dl-tool/internal/config	1.065s
CONFIG_OK
```

```text
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
cmd/dl-tool/main.go
internal/config/config.go
internal/config/config_test.go
internal/config/env.go
internal/secure/secret.go
```

Audit regressions cover static local mount prefixes and config-directory privacy when
the database lives elsewhere.

```text
$ make test PKG=./internal/config/... && echo CONFIG_OK
go test -race -count=1 ./internal/config/...
ok  	github.com/L-K-M/dl-tool/internal/config	1.113s
CONFIG_OK
```

## Blocked
None.

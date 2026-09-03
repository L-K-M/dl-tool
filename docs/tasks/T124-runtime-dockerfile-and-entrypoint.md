# T124 — Write the runtime Dockerfile and the entrypoint

| Field | Value |
|---|---|
| **ID** | T124 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T003, T004, T010, T013 |
| **Blocks** | T093, T125 |
| **Parallel-safe** | no — it edits `cmd/dl-tool/main.go` |
| **Implements** | [NFR-025](../02-requirements.md#nfr-025-run-unprivileged-with-the-operators-uid-and-gid), [NFR-004](../02-requirements.md#nfr-004-shut-down-gracefully-on-sigterm) |
| **Decisions** | [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md), [ADR-0007](../decisions/0007-react-spa-embedded-in-the-binary.md) |
| **Est. size** | 3 new files, 1 edit, ~190 LOC |

## Goal
`make docker-build` produces one `alpine:3.22` image holding the static `dl-tool` binary with the SPA embedded,
plus `su-exec`, `ca-certificates`, `tzdata`, `7zip` and `nodejs`. The entrypoint applies `TZ` and `UMASK`,
drops to `PUID:PGID` with `su-exec` and `exec`s the binary; the `HEALTHCHECK` turns the container healthy
once `/healthz` answers.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — stage order, the `apk add` line, the `ENV` defaults, `HEALTHCHECK`, `ENTRYPOINT`, `CMD`.
2. [`docs/10-deployment-and-compose.md` §4 PUID, PGID, UMASK, TZ](../10-deployment-and-compose.md#4-puid-pgid-umask-tz) — the nine entrypoint steps, in that order.
3. [`docs/10-deployment-and-compose.md` §4.1 The `user:` alternative](../10-deployment-and-compose.md#41-the-user-alternative) — the already-non-root branch.
4. [`docs/11-config-reference.md` §3 Container-level variables](../11-config-reference.md#3-container-level-variables-entrypoint-not-the-application) — the four names and their defaults.
5. [`docs/13-testing-and-verification.md` §2 Makefile](../13-testing-and-verification.md#2-makefile) — the `docker-build` target this task's verification calls.

## Files
| Path | Action | Purpose |
|---|---|---|
| `Dockerfile` | create | Three stages: the Vite build, the `CGO_ENABLED=0` Go build, the alpine runtime. |
| `.dockerignore` | create | Keep `.git`, `docs/`, `node_modules/`, `bin/`, `config/` and `.env` out of the build context. |
| `deploy/entrypoint.sh` | create | The privilege drop of doc 10 §4, mode `0755`. |
| `cmd/dl-tool/main.go` | edit | Consume the `serve` verb and add the `healthcheck` verb the image invokes. |

No other file may be modified.

## Interface contract

[`docs/10-deployment-and-compose.md` §5](../10-deployment-and-compose.md#5-dockerfile) **owns the
`Dockerfile` verbatim** — as its post-T093 end state. Copy it from there, not from this file, with three
omissions that T093 restores: the `LABEL` block, the cross-compilation arguments (`TARGETARCH`,
`TARGETVARIANT` and the `GOARCH` plumbing), and the `ytdlp` stage together with its
`COPY --from=ytdlp /yt-dlp /usr/local/bin/yt-dlp` line. This image therefore ships three stages and no
yt-dlp binary; T093 adds the pinned, SHA-256-verified fetch. Keep the `DLTOOL_YTDLP_PATH` ENV pointing at
`/usr/local/bin/yt-dlp` so T093 only drops the binary in.


`deploy/entrypoint.sh`, mode `0755`:

```sh
#!/bin/sh
# dl-tool container entrypoint. Runs as root, drops to PUID:PGID, execs the binary.
# Order is fixed by docs/10-deployment-and-compose.md section 4.
set -eu

PUID="${PUID:-1000}"; PGID="${PGID:-1000}"
UMASK="${UMASK:-002}"; TZ="${TZ:-Etc/UTC}"

if [ -f "/usr/share/zoneinfo/$TZ" ]; then
  ln -snf "/usr/share/zoneinfo/$TZ" /etc/localtime
  printf '%s\n' "$TZ" > /etc/timezone
fi

umask "$UMASK"

if [ "$(id -u)" -ne 0 ]; then
  echo "entrypoint: already running as $(id -u):$(id -g); skipping user creation and chown" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

if [ "$PUID" = "0" ]; then
  echo "entrypoint: PUID=0, running dl-tool as root" >&2
  exec /usr/local/bin/dl-tool "$@"
fi

getent group  "$PGID" >/dev/null 2>&1 || addgroup -g "$PGID" dltool
getent passwd "$PUID" >/dev/null 2>&1 || \
  adduser -D -H -u "$PUID" -G "$(getent group "$PGID" | cut -d: -f1)" dltool

mkdir -p "${DLTOOL_CONFIG_DIR:-/config}"
chown -R "$PUID:$PGID" "${DLTOOL_CONFIG_DIR:-/config}"
# NEVER chown /data recursively: it can hold terabytes and the operator owns its permissions.

for root in $(printf '%s' "${DLTOOL_DATA_ROOTS:-/data}" | tr ':' ' '); do
  su-exec "$PUID:$PGID" test -w "$root" || \
    echo "entrypoint: data_root_not_writable $root as $PUID:$PGID" >&2
done

exec su-exec "$PUID:$PGID" /usr/local/bin/dl-tool "$@"
```

`.dockerignore`:

```gitignore
.git
.github
docs
bin
config
node_modules
web/node_modules
web/dist
.env
*.db
```

`cmd/dl-tool/main.go` — the two verbs the image invokes. humacli's root command already *is* the server, so
`serve` is consumed before `humacli.New` and `healthcheck` never reaches cobra:

```go
func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "serve": // the image CMD; the root command is already the server
			os.Args = append(os.Args[:1], os.Args[2:]...)
		case "healthcheck": // the image HEALTHCHECK
			os.Exit(healthcheck())
		}
	}
	// unchanged from T004: humacli.New, hooks, versionCmd, cli.Run
}

// healthcheck GETs {DLTOOL_BASE_PATH}/healthz on DLTOOL_HTTP_ADDR and returns the
// process exit code: 0 on 200, 1 otherwise. It shells out to nothing, so the image
// needs neither curl nor a shell in the health command.
func healthcheck() int {
	addr := os.Getenv("DLTOOL_HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	url := "http://" + addr + strings.TrimSuffix(os.Getenv("DLTOOL_BASE_PATH"), "/") + "/healthz"
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck: status", resp.StatusCode)
		return 1
	}
	return 0
}
```

## Steps
1. Create `.dockerignore` with the block above, so the build context carries no repository state.
2. Create `Dockerfile` with the three stages above. Keep `apk add --no-cache su-exec ca-certificates tzdata
   7zip nodejs` exactly as written; Python is never installed.
3. Keep `COPY --from=web /web/dist ./internal/api/dist`: that is the path T013 embeds with `//go:embed all:dist`.
4. Keep `EXPOSE 8080`, the `ENV` defaults, `ENTRYPOINT ["/entrypoint.sh"]` and `CMD ["serve"]`. Add no
   `VOLUME`: it would create anonymous volumes whenever a bind mount is forgotten.
5. Create `deploy/entrypoint.sh` as above, `chmod 0755` it, and confirm its last line is an `exec`: without
   it the shell stays PID 1 and swallows `SIGTERM`.
6. Edit `cmd/dl-tool/main.go`: add the `serve`/`healthcheck` switch as the first statement of `main`, add
   `healthcheck()`, and import `context`, `fmt`, `net/http`, `strings` and `time`.
7. Build the image, run it with `PUID=1001 PGID=1001` and a mounted data directory, write a file into `/data`
   and confirm the owning uid is `1001`.
8. Run it again with `--user 1000:1000` and confirm the entrypoint logs the already-non-root line, skips user
   creation and still starts.
9. `docker stop` the container; it must exit `0` within 20 seconds.

## Acceptance criteria
- [ ] `make docker-build` completes and the resulting image is based on `alpine:3.22`.
- [ ] `docker run --rm --entrypoint sh <image> -c 'command -v su-exec 7zz node'` prints three paths, and the image contains no `python3` and no `curl`.
- [ ] With `PUID=1001 PGID=1001`, `docker exec <c> id -u` prints `1001` and files written to `/data` are owned by `1001`.
- [ ] With `--user 1000:1000`, the entrypoint skips user creation and `chown`, and `/data` is never chowned recursively.
- [ ] `docker inspect --format '{{.State.Health.Status}}' <c>` reads `healthy` within 60 s of start.
- [ ] `docker stop <c>` returns within 20 s and the container exit code is `0`.
- [ ] With `UMASK=002`, a file written under `/data` is `664` while `/config` stays `0700`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make docker-build VERSION=t124 && docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:t124 -c 'command -v su-exec 7zz node; ! command -v python3 && echo NO_PYTHON'
```
Expected: the build ends with `naming to ghcr.io/l-k-m/dl-tool:t124`, then exactly four lines —
`/sbin/su-exec`, `/usr/bin/7zz`, `/usr/bin/node`, `NO_PYTHON`. It pulls three base images once.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, and nothing else. Use `git status`, not `git diff`: a file
this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT add `--platform=$BUILDPLATFORM`, `TARGETOS`/`TARGETARCH` cross-compilation, the OCI `LABEL` block,
  SBOM, provenance or signing; T093 hardens this file and T097 owns publishing.
- Do NOT add the `ytdlp` stage or download a yt-dlp binary; T093 owns the pinned, SHA-256-verified fetch.
  The `DLTOOL_YTDLP_PATH` default stays; the binary itself arrives with T093.
- Do NOT create `compose.yaml`, `compose.dev.yaml` or `.env.example` (T125), or `deploy/aria2/Dockerfile` (T115).
- Do NOT add `/custom-cont-init.d` or `DOCKER_MODS`: both execute third-party code fetched at runtime.
- Do NOT switch the base image to Debian or distroless: neither can perform the privilege drop.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence

**Sandbox limitation (stated, not worked around):** this session runs as a non-root user in a
container with no Docker CLI, no daemon socket, no `sudo`, and user namespaces disabled, so the
verbatim Verification block could not execute:

```
$ make docker-build VERSION=t124
docker build -t ghcr.io/l-k-m/dl-tool:t124 .
/bin/bash: line 1: docker: command not found
make: *** [Makefile:57: docker-build] Error 127
$ unshare -U -r true
unshare: unshare failed: Operation not permitted
```

The image build and every `docker run` acceptance criterion therefore remain unverified here and
need one run on a Docker-capable machine. Everything that does not need a daemon was run for real;
output below is observed, not expected.

**Interface-contract note:** the contract paragraph said to keep doc 10 §5's `ytdlp` stage while
also deleting the `TARGETARCH` plumbing that stage selects on, and while `ARG YTDLP_VERSION=""`
stays an empty pin — a guaranteed build failure. The same task file's Goal, Steps ("three stages"),
Out-of-scope ("Do NOT add the `ytdlp` stage ... T093 owns the pinned, SHA-256-verified fetch") and
acceptance criteria all say the opposite, so the Dockerfile ships three stages and keeps the
`DLTOOL_YTDLP_PATH` ENV default only. Flagged in the PR description.

**Build, lint, vet, test (the Go edit):**

```
$ gofmt -l cmd internal          # (no output)
$ go vet ./... && CGO_ENABLED=0 go build -trimpath -ldflags '-s -w -X main.version=t124' \
    -o bin/dl-tool ./cmd/dl-tool && echo BUILD_OK
BUILD_OK
$ golangci-lint run ./...
0 issues.
$ go test -race -count=1 ./...
ok  github.com/L-K-M/dl-tool/internal/api      19.694s
ok  github.com/L-K-M/dl-tool/internal/config   1.201s
ok  github.com/L-K-M/dl-tool/internal/jobs     4.688s
ok  github.com/L-K-M/dl-tool/internal/obs      1.212s
ok  github.com/L-K-M/dl-tool/internal/secure   4.211s
ok  github.com/L-K-M/dl-tool/internal/store    7.538s
ok  github.com/L-K-M/dl-tool/internal/sync     2.039s
```

**`serve` and `healthcheck` verbs, run for real against a live local server:**

```
$ DLTOOL_CONFIG_DIR=/tmp/t124cfg DLTOOL_DATA_ROOTS=/tmp DLTOOL_DB_PATH=/tmp/t124cfg/dl-tool.db \
    DLTOOL_HTTP_ADDR=127.0.0.1:18091 bin/dl-tool serve &   # booted; proves `serve` is consumed
$ curl -s http://127.0.0.1:18091/healthz
{"status":"ok"}
$ DLTOOL_HTTP_ADDR=127.0.0.1:18091 bin/dl-tool healthcheck; echo exit=$?
exit=0
$ kill -TERM $SRV; wait $SRV                     # NFR-004
sigterm: exit=0 elapsed_ms=26
$ DLTOOL_HTTP_ADDR=127.0.0.1:18091 bin/dl-tool healthcheck; echo exit=$?
healthcheck: Get "http://127.0.0.1:18091/healthz": dial tcp 127.0.0.1:18091: connect: connection refused
exit=1
# second server on :18092 with DLTOOL_BASE_PATH=/dl:
$ DLTOOL_HTTP_ADDR=:18092 DLTOOL_BASE_PATH=/dl  bin/dl-tool healthcheck; echo exit=$?   # bare ":port" normalised
exit=0
$ DLTOOL_HTTP_ADDR=:18092 DLTOOL_BASE_PATH=/dl/ bin/dl-tool healthcheck; echo exit=$?   # trailing slash trimmed
exit=0
$ DLTOOL_HTTP_ADDR=:18092 bin/dl-tool healthcheck; echo exit=$?                        # wrong base path
healthcheck: status 404
exit=1
```

**Entrypoint static and non-root-branch checks** (shellcheck 0.10.0, real run through a copy with
only the binary path shimmed to /tmp — the root branch needs a real container). Output below is
post-review: the first review round caught that the TZ block ran before the non-root check and
aborted `--user` containers under `set -eu`, and that the `tr`-based root loop split paths on
spaces; both were fixed (root-guarded TZ write, `IFS=:` split) and re-run.

```
$ shellcheck -s sh deploy/entrypoint.sh && echo SHELLCHECK_CLEAN
SHELLCHECK_CLEAN
$ /tmp/t124ep/entrypoint.sh serve            # non-root, default TZ=Etc/UTC
entrypoint: already running as 1000:1000; skipping user creation and chown
fake-dl-tool: args=[serve] uid=1000 gid=1000 umask=0002
$ TZ=Bogus/Zone UMASK=077 /tmp/t124ep/entrypoint.sh serve extra-arg
entrypoint: already running as 1000:1000; skipping user creation and chown
fake-dl-tool: args=[serve extra-arg] uid=1000 gid=1000 umask=0077
$ # data-root split, loop extracted verbatim (post round-2: IFS=: and set -f):
$ DLTOOL_DATA_ROOTS="/tmp/t124ep/Media Library:/tmp/t124ep/nas" test_split
check: [/tmp/t124ep/Media Library]
check: [/tmp/t124ep/nas]
$ DLTOOL_DATA_ROOTS='/tmp/t124ep/a*:/tmp/plain' test_split   # glob chars stay literal
check: [/tmp/t124ep/a*]
check: [/tmp/t124ep/plain]
```

**Dockerfile static check** (hadolint 2.14.0): only `DL3018` (apk packages unpinned) — the task
pins the `apk add` line verbatim and T093 owns hardening, so it stays.

**Alpine 3.22 ground truth** (from `dl-cdn.alpinelinux.org` APKINDEX and the actual `.apk`
contents, x86_64): `su-exec-0.2-r3` ships `sbin/su-exec`, `7zip-24.09-r0` ships `usr/bin/7zz`,
`nodejs-22.23.2-r0` ships `usr/bin/node`; the dependency closures of all five `apk add` packages
contain no `python` and no `curl`. A boot with `DLTOOL_YTDLP_PATH=/usr/local/bin/yt-dlp` absent
(the image's current state until T093 lands) was run locally: `/healthz` answers `{"status":"ok"}`
and the config loader logs a `binary_missing` warning instead of failing. Base image tags all
resolve on Docker Hub: `node:24-alpine`, `golang:1.26-alpine`, `alpine:3.22` → HTTP 200.

**Web build stage replicated locally:** `npm ci` (188 packages) then `npm run build` →
`dist/index.html` + `dist/assets/`, matching the stage's commands.

**Scope:**

```
$ git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
.dockerignore
Dockerfile
cmd/dl-tool/main.go
deploy/entrypoint.sh
```

Exactly the Files table.

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

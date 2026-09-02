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
`Dockerfile` verbatim**. Copy it from there, not from this file, with two deletions that T093 restores:
the `LABEL` block and the cross-compilation arguments (`TARGETARCH`, `TARGETVARIANT` and the `GOARCH`
plumbing). Keep every stage otherwise unchanged, including the `ytdlp` stage that downloads the pinned
`yt-dlp_musllinux` binary and verifies its SHA-256.


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
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

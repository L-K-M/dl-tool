# 13 — Testing and Verification

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** every task file (T001–T115), and before writing any `*_test.go` or `*.test.tsx`

## Purpose
Define the test layers, the Makefile targets every task's `## Verification` block calls, and the repo-wide
**Definition of Done**. It does not define product behaviour; it defines how behaviour is proven.

## Scope of this document
- In scope: test pyramid, `Makefile` target contents and exit behaviour, Definition of Done, the adapter
  contract suite, golden-file policy, frontend and E2E testing, CI gates, `scripts/doclint.sh`.
- Out of scope (lives instead in): env vars → [`11-config-reference.md`](11-config-reference.md); compose
  services, ports and image publishing → [`10-deployment-and-compose.md`](10-deployment-and-compose.md);
  the `Engine` interface → [`06-download-engines.md`](06-download-engines.md); screens, grid columns and
  a11y rules → [`09-web-ui-spec.md`](09-web-ui-spec.md); commits → [`14-conventions.md`](14-conventions.md).

## 1. Test pyramid

| Layer | Exact package | Version | What it proves |
|---|---|---|---|
| Go unit | stdlib `testing` + `github.com/stretchr/testify/require` | v1.12.1 | Pure logic: state machine, rid-delta computation, RSS matching, backoff. |
| Golden files | `github.com/google/go-cmp/cmp` | v0.7.0 | Parser output for a recorded daemon fixture is byte-stable. |
| Adapter contract | `github.com/testcontainers/testcontainers-go` | v0.44.0 | The adapter works against the **real daemon**, not a mock. |
| HTTP handler | `github.com/danielgtaylor/huma/v2/humatest` | v2.39.1 | Handlers, validation and problem+json errors without opening a socket. |
| API contract | stdlib `testing` + `go-cmp` | — | The served `/openapi.json` equals the committed `api/openapi.json`. |
| Frontend unit | `vitest` + `@testing-library/react` + `happy-dom` | 4.1.11 / 16.3.3 / pin at implementation time <!-- pin at implementation time --> | SSE reducer state transitions and `TaskGrid` rendering. |
| Mocked network | `msw` | 2.15.0 | The SPA runs with no backend and with deterministic error responses. |
| E2E | `@playwright/test` | 1.62.1 | Add → progress → complete in a real browser against a real stack. |
| Accessibility | `@axe-core/playwright` | pin at implementation time <!-- pin at implementation time --> | Zero serious/critical violations on the five main screens. |

Rules: unit and golden tests run with no Docker, no network and no daemon — `make test` is the fast lane.
Everything needing a container carries `//go:build integration` and runs only under `make test-integration`.
Test files sit beside their source as `*_test.go`, fixtures in a sibling `testdata/` directory.

## 2. Makefile

Every tool version comes from `go.mod` or `web/package-lock.json`, except the two pinned in the Makefile head.

```makefile
SHELL   := /bin/bash
GO      ?= go
PKG     ?= ./...
IMAGE   ?= ghcr.io/l-k-m/dl-tool
VERSION ?= dev
GOLANGCI_LINT_VERSION ?= PINME   # pin at implementation time; make setup fails until it is set
LYCHEE_VERSION        ?= PINME   # pin at implementation time; scripts/doclint.sh fails without lychee

.PHONY: setup gen lint vet typecheck test test-go test-web test-integration e2e \
        build docker-build compose-check doclint ci

setup:
	$(GO) mod download
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	cd web && npm ci
	cd web && npx playwright install --with-deps chromium
	cargo install --locked lychee --version $(LYCHEE_VERSION)

gen:
	./scripts/gen.sh

lint:
	test -z "$$(gofmt -l cmd internal)"
	golangci-lint run ./...
	cd web && npm run lint
	cd web && npx prettier --check .

vet:
	$(GO) vet ./...

typecheck:
	cd web && npx tsc --noEmit -p tsconfig.json

ifeq ($(PKG),./...)
test: test-go test-web
else
test: test-go
endif

test-go:
	$(GO) test -race -count=1 $(PKG)

test-web:
	cd web && npx vitest run

test-integration:
	$(GO) test -tags=integration -count=1 -timeout=20m ./internal/engine/...

e2e:
	cd web && npx playwright test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/dl-tool ./cmd/dl-tool

docker-build:
	docker build -t $(IMAGE):$(VERSION) .

compose-check:
	docker compose -f compose.yaml config -q
	docker compose -f compose.yaml -f compose.dev.yaml config -q

doclint:
	./scripts/doclint.sh

ci: lint vet typecheck test compose-check doclint
```

| Target | Runs | Exit behaviour |
|---|---|---|
| `setup` | Go module download, `golangci-lint` install, `npm ci`, Playwright chromium install, `lychee` install. | 0 once the toolchain is present; non-zero on any download failure. |
| `gen` | `scripts/gen.sh`: regenerates `api/openapi.json` and `web/src/api/schema.d.ts`. | 0 always if generation succeeds; it does **not** check for drift — CI does (§7). |
| `lint` | `gofmt -l`, `golangci-lint run`, ESLint, Prettier `--check`. | Non-zero on any unformatted file or any lint finding. |
| `vet` | `go vet ./...`. | Non-zero on any vet diagnostic. |
| `typecheck` | `tsc --noEmit`. | Non-zero on any TypeScript error. |
| `test` | Go tests with `-race` plus Vitest. With `PKG=` set, Go only. | Non-zero on any failing or panicking test. |
| `test-integration` | `-tags=integration` engine tests; needs a working Docker socket. | Non-zero on failure; non-zero (not skipped) when Docker is unreachable. |
| `e2e` | Playwright specs in `web/e2e/`. | Non-zero on any failing scenario. |
| `build` | `CGO_ENABLED=0 go build` → `bin/dl-tool`. | Non-zero on any compile error. |
| `docker-build` | `docker build` of the repo `Dockerfile`. | Non-zero on any build-stage failure. |
| `compose-check` | `docker compose config -q` for the base and dev overlay. | Non-zero on invalid YAML, an unknown key or an unresolvable variable. |
| `doclint` | `scripts/doclint.sh`. | Non-zero on any plan-document violation (§8). |
| `ci` | `lint vet typecheck test compose-check doclint` in that order. | Stops at the first non-zero target. |

`go test -race` needs cgo at test time; only the `build` target sets `CGO_ENABLED=0`.

## 3. Definition of Done

A task is DONE only when **all** of the following are true. This list is the single source; task files link
here rather than restating it.

1. `make lint` exits 0.
2. `make vet` exits 0.
3. `make typecheck` exits 0.
4. `make test` exits 0 **and** every test named in the task file's `## Verification` block appears in the
   output as `PASS` (Go) or as a passing Vitest assertion. A test that did not run does not count.
5. `make compose-check` exits 0, if the task touched `compose.yaml`, `compose.dev.yaml`, `Dockerfile`,
   `.env.example`, or any environment variable.
6. `make test-integration` exits 0, if the task touched `internal/engine/`.
7. Every checkbox under `## Acceptance criteria` is ticked, and the real command output is pasted verbatim
   under `## Evidence` in the task file. Asserted success without pasted output is not evidence.
8. `git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort` matches the task's
   `## Files` table exactly — no extra path, no missing path. `git diff` is **not** usable here: a file the
   task creates is untracked and never appears in `git diff --name-only`. The `docs` exclusion is what lets
   rules 7 and 9 (pasting Evidence, flipping the index row) coexist with this rule.
9. The task's row in [`tasks/00-task-index.md`](tasks/00-task-index.md) is set to `done`, in the same commit
   as the work.

**IMPORTANT** Never satisfy an item by skipping a test, weakening an assertion, deleting a test, or adding
`//nolint` / `_ = err`. If a check cannot pass, stop and write the reason under `## Blocked` in the task file.

## 4. Adapter contract tests

One shared, table-driven conformance suite lives in `internal/engine/enginetest/contract.go`. Every engine
adapter runs it; adding an engine adds one call, not a new suite.

```go
//go:build integration

package enginetest

// RunContract asserts that an Engine implementation honours the interface in
// docs/06-download-engines.md against a real daemon started by testcontainers.
func RunContract(t *testing.T, newEngine func(t *testing.T) engine.Engine) {
	t.Run("AddURL/Progress/Pause/Resume/Remove", func(t *testing.T) { /* ... */ })
	t.Run("ListReturnsStableIDs", func(t *testing.T) { /* ... */ })
	t.Run("UnknownIDReturnsErrNotFound", func(t *testing.T) { /* ... */ })
	t.Run("SpeedLimitRoundTrips", func(t *testing.T) { /* ... */ })
}
```

Call sites, one per adapter, in files that also carry `//go:build integration`:

| Adapter | Test file | Container |
|---|---|---|
| qBittorrent | `internal/engine/qbittorrent/contract_test.go` | `lscr.io/linuxserver/qbittorrent:5.2.3` (Docker Hub, updated 2026-08-30) |
| aria2 | `internal/engine/aria2/contract_test.go` | built from `deploy/aria2/Dockerfile` via `testcontainers.FromDockerfile` |
| yt-dlp | `internal/engine/ytdlp/contract_test.go` | none — subprocess against a local `yt-dlp` binary; skip with `t.Skip` when absent |

Rules:
- Never depend on `p3terx/aria2-pro`; it was last pushed 2022-09-06. The aria2 image is built in-repo and
  published as `ghcr.io/l-k-m/dl-tool-aria2`.
- Use an explicit wait strategy; the testcontainers default deadline is 60 s. For qBittorrent:
  `testcontainers.WithWaitStrategy(wait.ForHTTP("/api/v2/app/version").WithPort("8080/tcp"))`.
- Capability differences are skipped, never asserted away: aria2 has no per-file priorities, so that subtest
  calls `t.Skip` when the capability is absent. See [`06-download-engines.md`](06-download-engines.md).
- Downloads under test target a local HTTP fixture server or a Debian/Ubuntu release URL, never a third-party
  tracker. `make test` must stay green on a machine with no Docker; that is what the build tag buys.

## 5. Golden-file fixtures

| Rule | Value |
|---|---|
| Location | `internal/engine/<engine>/testdata/`, `internal/search/testdata/`, `internal/rss/testdata/` |
| Input name | `<source>_<version>.json` / `.xml` / `.torrent`, e.g. `qb_info_5.2.3.json` |
| Expected name | Same stem + `.golden.json` |
| Capture | Once, from a real daemon or feed; record the command and date in a sibling `testdata/README.md` |
| Diff | `github.com/google/go-cmp/cmp` v0.7.0 |
| Regenerate | `go test ./internal/engine/qbittorrent/... -update` |

```go
var update = flag.Bool("update", false, "rewrite .golden.json files from current parser output")

func TestParseTorrents(t *testing.T) {
	got := parseTorrents(mustRead(t, "testdata/qb_info_5.2.3.json"))
	golden := "testdata/qb_info_5.2.3.golden.json"
	if *update {
		writeGolden(t, golden, got)
	}
	want := loadGolden(t, golden)
	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("mismatch (-want +got):\n%s", diff)
	}
}
```

- Capture commands: `curl -s "$QBT/api/v2/torrents/info" > testdata/qb_info_5.2.3.json`; the
  `aria2.tellStatus` JSON-RPC response; `yt-dlp -J <url> > testdata/ytdlp_<site>.json`.
- Redact tokens, cookies, `Authorization` headers and local absolute paths before committing a fixture.
- Never delete a golden file to make a test pass. A diff caused by a real upstream change is committed
  together with the adapter fix, in the same task.

## 6. Frontend testing

| Concern | Tool | Location |
|---|---|---|
| SSE reducer (`rid` deltas applied to the task map) | vitest + `@testing-library/react` | `web/src/store/useTasks.test.ts` |
| `TaskGrid` rendering, sorting, selection, virtualisation | vitest + `@testing-library/react` | `web/src/components/TaskGrid/TaskGrid.test.tsx` |
| Network stubbing (`/api/v1/*`, `/api/v1/sync`) | `msw` 2.15.0, handlers shared from `web/src/mocks/handlers.ts` | per test |
| Browser E2E | `@playwright/test` 1.62.1 | `web/e2e/*.spec.ts` |

Reducer tests cover a full snapshot (`rid` reset), an incremental delta, a removal, an out-of-order `rid`
forcing a resync, and reconnection carrying `Last-Event-ID`.

### 6.1 Playwright scenarios that must exist

| Spec file | Scenario | Passes when |
|---|---|---|
| `web/e2e/setup.spec.ts` | Fresh install: the setup wizard creates the first admin, then that admin logs in. | `/auth/setup` succeeds once, a second attempt returns 409, and the task list renders. |
| `web/e2e/add-url.spec.ts` | Add an HTTPS URL and watch it complete. | The row appears as `queued`, moves through `downloading`, and reaches `completed` with progress streamed over SSE. |
| `web/e2e/add-magnet.spec.ts` | Add a magnet, choose files in the selection step, download it. | The inspect dialog lists files, deselected files stay at priority `0`, and the task reaches `completed` or `seeding`. |
| `web/e2e/rss-rule.spec.ts` | Create an RSS rule and open the dry-run panel. | Every evaluated feed item is listed with a reason code, matches and non-matches alike. |

E2E runs against a real stack started by `compose.yaml` with **both engine lanes up** — the aria2 lane
with `COMPOSE_PROFILES=aria2` plus `DLTOOL_ARIA2_URL` and `ARIA2_RPC_SECRET` in `.env`, and the
qBittorrent lane with `DLTOOL_QBITTORRENT_URL` plus `QBT_USERNAME`/`QBT_PASSWORD`
([`10-deployment-and-compose.md`](10-deployment-and-compose.md) §2) — because `add-url.spec.ts` downloads
a real HTTPS URL and `add-magnet.spec.ts` adds a real torrent; magnet and URL fixtures use the bundled
legitimate sources only (Arch Linux, Debian, Ubuntu). See
[`10-deployment-and-compose.md`](10-deployment-and-compose.md).

### 6.2 Accessibility and PWA gates

- `@axe-core/playwright` runs on the five main screens — Tasks, Search, RSS, Settings, and the setup wizard —
  with **zero serious or critical violations**.
- One explicit keyboard-map test walks the documented shortcuts and tab order in
  [`09-web-ui-spec.md`](09-web-ui-spec.md) with no pointer events.
- The virtualised grid asserts `aria-rowcount` equals the **total** task count, not the number of rows
  currently in the DOM. This is the failure mode virtualisation introduces.
- A Lighthouse installability check asserts manifest, maskable icons, `display: standalone`, `theme-color`
  and a registered service worker. Nothing works offline; the check asserts installability only.
- Byte, rate and duration strings are asserted through `Intl`, so a locale change cannot silently break them.

## 7. CI

| Workflow | Job | Runs | Blocking |
|---|---|---|---|
| `.github/workflows/ci.yml` | `lint` | `make lint`, `make vet`, `make typecheck` | yes |
| | `test` | `make test` | yes |
| | `gen-drift` | the drift gate below | yes |
| | `integration` | `make test-integration` | yes |
| | `compose` | `make compose-check`, `make docker-build` | yes |
| `.github/workflows/docs-lint.yml` | `doclint` | `cargo install --locked lychee --version $LYCHEE_VERSION`, then `make doclint` | yes |
| `.github/workflows/release.yml` | `image` | multi-arch buildx publish to `ghcr.io/l-k-m/dl-tool` | tags only |

Action versions read from the official READMEs on 2026-09-01: `actions/checkout@v4`,
`docker/setup-qemu-action@v4`, `docker/setup-buildx-action@v4`, `docker/login-action@v4`,
`docker/build-push-action@v7`, `docker/metadata-action@v6`. `actions/setup-go` and `actions/setup-node` are
pinned at implementation time. <!-- pin at implementation time --> Release-image details live in
[`10-deployment-and-compose.md`](10-deployment-and-compose.md).

### 7.1 Gate 1 — generated artefacts cannot drift

`scripts/gen.sh` is the only way `api/openapi.json` and `web/src/api/schema.d.ts` are produced:

```bash
#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
go run ./cmd/dl-tool openapi > api/openapi.json
cd web && npx openapi-typescript ../api/openapi.json -o src/api/schema.d.ts
```

The gate regenerates and fails on any difference:

```yaml
      - name: Regenerate the API contract
        run: make gen
      - name: Fail if generated files drifted
        run: git diff --exit-code -- api/openapi.json web/src/api/schema.d.ts
```

Because Huma derives the spec from the handler structs, a handler change that was not regenerated fails here.
See [`ADR-0003`](decisions/0003-chi-huma-code-first-openapi.md). A Go test also boots the server, fetches
`/openapi.json` and compares it with the committed file, so `make test` catches the same drift locally.

### 7.2 Gate 2 — the plan cannot rot

`make doclint` runs `scripts/doclint.sh` (§8) on every push and pull request. It fails on a clarification left
outside `## Open questions`, a task file missing a mandatory section, hedging, or a broken relative link.

## 8. `scripts/doclint.sh`

```bash
#!/usr/bin/env bash
# Machine checks for the plan documents. Exit 0 = clean, 1 = at least one violation.
set -euo pipefail
cd "$(dirname "$0")/.."

fail=0
note() { printf 'doclint: %s\n' "$1" >&2; fail=1; }

# Emit "path:line: text" for prose only, excluding fenced code blocks so that examples
# and templates do not trip the text checks. Git's `*` in a pathspec also matches `/`.
mapfile -t DOCS < <(git ls-files -- 'docs/*.md')
prose() {   # emits "path:line:section: text"
  awk 'BEGIN { fence = sprintf("%c%c%c", 96, 96, 96) }
       FNR == 1 { inf = 0; sec = "" }
       index($0, fence) == 1 { inf = !inf; next }
       !inf && /^## / { sec = substr($0, 4) }
       !inf { printf "%s:%d:%s: %s\n", FILENAME, FNR, sec, $0 }' "${DOCS[@]}"
}

# 1. No unresolved clarifications, except in an ADR or under "## Open questions"
if prose | grep -v '^docs/decisions/' | grep -v ':Open questions: ' | grep -F 'NEEDS CLARIFICATION'; then
  note 'unresolved NEEDS CLARIFICATION outside docs/decisions/ and ## Open questions'
fi

# 2. Every task file carries the three mandatory scope-fencing sections
shopt -s nullglob
for f in docs/tasks/T*.md; do
  grep -q '^## Verification'   "$f" || note "missing Verification section: $f"
  grep -q '^## Files'          "$f" || note "missing Files section: $f"
  grep -q '^## Out of scope'   "$f" || note "missing Out of scope section: $f"
done

# 3. No hedging language in plan documents
if prose | grep -v '^docs/decisions/' \
   | grep -iE '\b(TBD|we could|you could also|might want|maybe|probably)\b'; then
  note 'hedging language in a plan document'
fi

# 4. No absolute links back into this repository; relative links only
if prose | grep -iE 'https://github\.com/L-K-M/dl-tool/(blob|tree)/'; then
  note 'absolute self-link; use a relative path'
fi

# 5. Relative links and anchors resolve
if command -v lychee >/dev/null 2>&1; then
  lychee --offline --include-fragments docs README.md AGENTS.md || note 'broken relative link or anchor'
else
  note 'lychee is not installed; run make setup'
fi

exit "$fail"
```

The `prose` filter strips fenced code blocks, so an example or template cannot trip a text check.
`docs/decisions/` is exempt from checks 1 and 3 because an ADR records the options it rejected.

## Decisions referenced
| ADR | Decision |
|---|---|
| [ADR-0003](decisions/0003-chi-huma-code-first-openapi.md) | chi + Huma with code-first OpenAPI — makes the §7.1 drift gate possible |
| [ADR-0005](decisions/0005-aria2-qbittorrent-ytdlp-engines.md) | aria2, qBittorrent and yt-dlp as the v1 engines — the three adapters that run the §4 contract suite |
| [ADR-0007](decisions/0007-react-spa-embedded-in-the-binary.md) | React SPA embedded in the Go binary — why §6 needs both Vitest and Playwright |

## Open questions
- **UNVERIFIED:** lychee's fragment-checking flag is carried over from the research as `--include-fragments`
  and was not re-verified against lychee's own documentation. Confirm it with `lychee --help` when the
  docs-lint job is wired, and pin the lychee version in `make setup` at the same time.

## Change log
| Date | Change |
|---|---|
| 2026-09-01 | Initial version |
| 2026-09-01 | Consistency review: corrected the ADR-0005 and ADR-0007 links to the canonical filenames and removed the resolved open question about ADR slugs. |

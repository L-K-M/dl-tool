# T010 — Serve health, readiness and Prometheus metrics

| Field | Value |
|---|---|
| **ID** | T010 |
| **Milestone** | M0 |
| **Status** | todo |
| **Depends on** | T006, T007 |
| **Blocks** | T096, T124 |
| **Parallel-safe** | no — it edits `internal/api/server.go` and `cmd/dl-tool/main.go` |
| **Implements** | [FR-152](../02-requirements.md#fr-152-expose-health-and-readiness-endpoints), [FR-153](../02-requirements.md#fr-153-expose-prometheus-metrics-on-a-separate-listener) |
| **Decisions** | [ADR-0004](../decisions/0004-sqlite-as-the-only-datastore.md) |
| **Est. size** | 2 new source files, 1 test file, ~230 LOC |

## Goal
`GET /healthz` returns `{"status":"ok"}` as soon as the listener is up, touching neither the database nor an
engine. `GET /readyz` returns `{"status":"ready"}` only once migrations are applied and the database answers,
and 503 `/problems/not-ready` before that. `/metrics` is served on `DLTOOL_METRICS_ADDR` only, and returns
404 on the main listener.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/05-api-contract.md` §13.1 Process endpoints outside `/api/v1`](../05-api-contract.md#131-process-endpoints-outside-apiv1)
   — the three paths, their listeners and their bodies.
2. [`docs/11-config-reference.md` §2 `DLTOOL_` variables](../11-config-reference.md#2-dltool_-variables-application)
   — `DLTOOL_METRICS_ADDR`, and that the exact lowercase value `off` disables metrics entirely, while an
   empty value means unset and falls back to the default.
3. [`docs/14-conventions.md` §3 Logging](../14-conventions.md#3-logging) — the standard attribute keys.

## Files
| Path | Action | Purpose |
|---|---|---|
| `internal/obs/health.go` | create | The `/healthz` and `/readyz` handlers and the readiness gate. |
| `internal/obs/metrics.go` | create | The Prometheus registry, the metric set and the second listener. |
| `internal/obs/health_test.go` | create | Health, readiness and metrics-isolation tests. |
| `internal/api/server.go` | edit | Mount `/healthz` and `/readyz` under the base path, outside `/api/v1`. |
| `cmd/dl-tool/main.go` | edit | Start and stop the metrics listener; mark the store ready after migration. |

No other file may be modified.

## Interface contract

```go
package obs

// Health tracks readiness. Ready flips exactly once, after migrations succeed.
type Health struct{ /* db *sqlx.DB; ready atomic.Bool */ }

func NewHealth(db *sqlx.DB) *Health

// MarkReady is called by cmd/dl-tool once store.Open has returned.
func (h *Health) MarkReady()

// Live answers GET /healthz with 200 {"status":"ok"} and touches nothing.
func (h *Health) Live(w http.ResponseWriter, r *http.Request)

// Ready answers GET /readyz with 200 {"status":"ready"} when MarkReady has run and
// SELECT 1 succeeds, otherwise 503 application/problem+json /problems/not-ready.
func (h *Health) Ready(w http.ResponseWriter, r *http.Request)
```

```go
package obs

// Metrics owns a private registry so the process exposes only these series plus
// the Go and process collectors.
type Metrics struct{ Registry *prometheus.Registry }

func NewMetrics() *Metrics

// ListenAndServe serves GET /metrics on addr. The exact value "off" disables metrics and
// returns nil immediately; an empty addr means unset and falls back to the default
// (11-config-reference.md section 2).
func (m *Metrics) ListenAndServe(ctx context.Context, addr string) error
```

Every series below is registered here and **written elsewhere**, so `*Metrics` is passed to the components
that observe the events: the reconciler (`poll_duration`, `engine_errors`, `bytes_transferred`), the task
store (`task_transitions`), the worker pool (`jobs_total`), the SSE hub (`sse_clients`) and a 15-second
`tasks_total` sampler over `SELECT state, engine, count(*) FROM tasks GROUP BY 1, 2`. A task that adds one
of those components wires its metric at the same composition-root call site, per
[`14-conventions.md` §8.3](../14-conventions.md#83-wire-a-long-lived-component). A registered series that
nothing ever sets is a defect, not an empty gauge.

Metric set — names and labels are fixed here and used by every later milestone:

| Metric | Type | Labels |
|---|---|---|
| `dltool_tasks_total` | gauge | `state`, `engine` |
| `dltool_task_transitions_total` | counter | `from`, `to`, `engine` |
| `dltool_bytes_transferred_total` | counter | `direction` |
| `dltool_engine_poll_duration_seconds` | histogram | `engine` |
| `dltool_engine_errors_total` | counter | `engine`, `kind` |
| `dltool_jobs_total` | counter | `kind`, `outcome` |
| `dltool_sse_clients` | gauge | — |

Response bodies:

```json
GET /healthz  -> 200 {"status":"ok"}
GET /readyz   -> 200 {"status":"ready"}
GET /readyz   -> 503 {"type":"/problems/not-ready","title":"Not ready","status":503,"detail":"migrations have not completed"}
```

## Steps
1. Create `internal/obs/health.go` with `Health` as above. `Live` writes the fixed JSON with no dependency
   lookup at all — a slow engine must never restart the container.
2. `Ready` returns 503 until `MarkReady` has run, then runs `SELECT 1` with a one-second context and returns
   503 on failure. Its 503 body is `application/problem+json`.
3. Create `internal/obs/metrics.go`. Build a `prometheus.NewRegistry`, register the seven collectors above
   plus `collectors.NewGoCollector()` and `collectors.NewProcessCollector(...)`, and serve
   `promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})` at `/metrics` on its own `http.Server`.
4. `ListenAndServe` returns nil immediately for an empty address, and shuts the listener down when `ctx` is
   cancelled.
5. Edit `internal/api/server.go` to register `GET /healthz` and `GET /readyz` on the base sub-router, outside
   the `/api/v1` group, and therefore outside the `Authenticate` middleware. Register no `/metrics` route.
6. Edit `cmd/dl-tool/main.go`: build `obs.NewHealth(db)` and `obs.NewMetrics()`, keep the `*Metrics` in
   scope so later components can be handed it, start the `tasks_total` sampler on a 15 s ticker under the
   server context, call `MarkReady` after
   `store.Open` returns, start the metrics listener in `OnStart` and cancel it in `OnStop`.
7. Write `internal/obs/health_test.go` covering: `/healthz` returns 200 with the exact body while the
   database handle is nil; `/readyz` returns 503 `/problems/not-ready` before `MarkReady` and 200 after;
   `/metrics` on the main listener returns 404; the metrics listener returns a body containing
   `dltool_tasks_total` and `process_start_time_seconds`; `DLTOOL_METRICS_ADDR=off` starts no listener while
   an empty value still listens on the default address.
8. Run the verification command and paste its output under `## Evidence`.

## Acceptance criteria
- [ ] `TestHealthzDoesNotTouchTheDatabase` passes with a nil database handle.
- [ ] `TestReadyzIs503BeforeMigration` asserts the status and the `/problems/not-ready` slug.
- [ ] `TestMetricsNotOnMainListener` asserts 404 for `/metrics` on the main router.
- [ ] `TestMetricsListenerExposesMetricSet` asserts every one of the seven metric names is present.
- [ ] `TestEmptyMetricsAddrDisablesMetrics` asserts no listener is opened.
- [ ] `/healthz` and `/readyz` require no credential and sit outside `/api/v1`.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make test PKG="./internal/obs/... ./internal/api/..." && echo OBS_OK
```
Expected: `ok  	github.com/L-K-M/dl-tool/internal/obs` and `ok  	github.com/L-K-M/dl-tool/internal/api`,
no `FAIL`, and a final line of exactly `OBS_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT populate the metric values from real tasks, engines or jobs; each owning milestone increments its
  own series. This task registers them and proves the endpoint serves them.
- Do NOT implement `GET /system/info` or `GET /system/logs`; T092 and T096 own them.
- Do NOT add an engine reachability probe to `/readyz`; doc 05 §13.1 gates it on migrations and the database
  only, so a stopped engine never makes the container unhealthy.
- Do NOT expose `/metrics` on the main listener, and do NOT put authentication on the metrics listener; it
  is bound to loopback by default instead.
- Do NOT add tracing, OpenTelemetry or any exporter that leaves the host; dl-tool sends no telemetry.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

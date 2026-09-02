# 17 — Operations and Runbook

> **Status:** draft
> **Last reviewed:** 2026-09-01
> **Audience:** implementing agent
> **Read this before:** T005, T006, T009, T091, T099, T108, T113, T115

## Purpose

This document defines what the **operator** does and what dl-tool does **to itself** around a run: the boot
sequence and every reason it may refuse to start, graceful shutdown, backup and restore, upgrade and
rollback, disk management, log redaction, the diagnostics bundle, and a symptom→cause→fix table. It does not
define compose files, environment variables, DDL or endpoint shapes.

## Scope of this document

- In scope: startup and shutdown order; fail-fast conditions; the pre-migration backup and the
  schema-newer-than-binary refusal; boot reconciliation; nightly `VACUUM INTO`; the restore procedure and
  the `dl-tool restore --from` CLI; upgrade and rollback; disk reservation and low-space auto-pause;
  redaction rules applied to logs and bundles; the diagnostics bundle; the troubleshooting table.
- Out of scope (lives instead in): compose services, volumes, ports, the entrypoint script and the
  Dockerfile → [`10-deployment-and-compose.md`](10-deployment-and-compose.md); environment variables,
  boot-validation error codes and secret carriers → [`11-config-reference.md`](11-config-reference.md);
  DDL, the migration policy, backup file semantics and retention →
  [`04-data-model.md`](04-data-model.md); request and response shapes for `/system/*` →
  [`05-api-contract.md`](05-api-contract.md); the engine conformance table and the bandwidth precedence
  chain → [`06-download-engines.md`](06-download-engines.md); Makefile targets and the Definition of Done →
  [`13-testing-and-verification.md`](13-testing-and-verification.md).

---

## 1. Startup sequence

Two processes run in order inside one container: `deploy/entrypoint.sh` as `root`, then the `dl-tool` binary
as `PUID:PGID`. Nothing between them is optional and nothing is reordered.

```mermaid
flowchart TD
  A["entrypoint.sh as root: PUID, PGID, UMASK, TZ"] --> B["exec su-exec PUID:PGID dl-tool"]
  B --> C["S1 config.Load: parse and validate"]
  C -->|invalid| X1["exit 1, config_*"]
  C --> D["S2 network-filesystem check on the database directory"]
  D -->|"nfs, cifs, smb3, fuse.*"| X2["exit 1, config_network_fs"]
  D --> E["S3 acquire process lock, open SQLite"]
  E --> F["S4 read applied and embedded schema versions"]
  F -->|"applied > embedded"| X4["exit 1, schema_too_new"]
  F -->|"0 < applied < embedded"| P["unique pre-migration VACUUM INTO backup"]
  P -->|backup failed| X3["exit 1, backup_failed"]
  P --> G["S5 goose migrations"]
  F -->|"applied = 0 < embedded"| G
  F -->|"applied = embedded"| H["S6 PRAGMA integrity_check"]
  G --> H["S6 PRAGMA integrity_check"]
  H --> I["S7 mount and hardlink self-check"]
  I --> J["S8 bind listeners, serve /healthz"]
  J --> K["S9 engine capability probe and conformance"]
  K --> L["S10 boot reconciliation"]
  L --> M["S11 start cron, workers, watch folders; /readyz returns 200"]
```

### 1.1 Stage S0 — the entrypoint privilege drop

The ordered steps (read `TZ`, apply `umask`, create the `dltool` user and group, `chown /config` but never
`/data`, then `exec su-exec "$PUID:$PGID" …`) are owned by
[`10-deployment-and-compose.md`](10-deployment-and-compose.md#4-puid-pgid-umask-tz). Two operator-visible
consequences belong here:

| Symptom at S0 | Meaning | Action |
|---|---|---|
| Log line `data_root_not_writable` | `/data` is not writable as `PUID:PGID`. The container still starts so the UI can show the problem. | `chown -R "$PUID:$PGID"` the host directory backing `/data`, then restart. |
| Warning that dl-tool is running as root | `PUID=0`; the privilege drop was skipped. | Set a non-zero `PUID`/`PGID`. Files written as root are unusable from SMB and the NAS file manager. |
| `SIGTERM` ignored, container always killed at the grace period | The entrypoint was edited to launch the binary without `exec`. | Restore `exec`; without it the shell is PID 1 and swallows the signal. |

### 1.2 Stages S1–S2 — configuration and the network-filesystem refusal

`config.Load` performs no network I/O. The full condition→behaviour→`err_code` matrix is
[`11-config-reference.md` §8](11-config-reference.md#8-boot-validation); do not restate it in code comments,
implement it from there. Fatal means one `error` record carrying `err_code`, then `os.Exit(1)`.

**IMPORTANT** dl-tool **refuses to start** when the filesystem holding `DLTOOL_DB_PATH` is `nfs`, `cifs`,
`smb3` or any `fuse.*` type. SQLite's WAL mode requires shared memory between processes, which a network
filesystem cannot provide; the upstream wording is *"WAL does not work over a network filesystem"*. There is
**no** `journal_mode=DELETE` fallback and no override flag — a silently degraded database is worse than a
refusal.

| Detection step | Implementation |
|---|---|
| Resolve the directory | `filepath.Dir(DLTOOL_DB_PATH)`, after symlink resolution. |
| Read the mount type | `/proc/self/mountinfo`, longest-prefix match on the mount point; fall back to `statfs` `f_type` when the file is unreadable. |
| Reject | Exact match on `nfs`, `nfs4`, `cifs`, `smb3`; prefix match on `fuse.`. |
| Fail | `err_code=config_network_fs`, naming the directory and the detected type, exit 1. |

`/data` may be a network filesystem. Only the database directory is checked.

### 1.3 Stages S3–S6 — lock, version gate, backup and migrate

1. Derive the stable lock path with `databaseLockSuffix = ".lock"`, open it mode `0600`, acquire
   `flock(LOCK_EX|LOCK_NB)`, then write the process PID. A second server exits with `database_locked`.
   Keep the descriptor locked for the process lifetime; never unlink or replace the lock file.
2. Open SQLite and read the applied and highest embedded schema versions. If applied is greater, exit with
   `schema_too_new` **before** backup or goose runs.
3. When both versions match, skip backup and migration. Repeated boots at one version create no backup.
4. On a new database at version `0`, run goose without creating an empty rollback backup.
5. Only when `0 < applied < embedded`, back up to
   `dl-tool.db.pre-migration-<from>-to-<to>.<UTC>.bak`. `VACUUM INTO` a unique temporary name, integrity-check
   and fsync it, atomically rename it, then fsync the directory. Any failure exits with `backup_failed` before
   goose runs.
6. Run embedded migrations forward, logging each file and duration.
7. Run `PRAGMA integrity_check` and publish the result on `GET /system/info`.

Migration file naming, the mandatory `-- +goose Down` section and the version query live in
[`04-data-model.md` §5](04-data-model.md#5-schema-migration-policy).

### 1.4 Stage S7 — mount and hardlink self-check

Run per configured root, and again after any settings change that adds a destination. The procedure —
compare `st_dev`, then **attempt a real `link()` probe** with a `.dltool-linkprobe-<ulid>` file and unlink
both — is specified in
[`10-deployment-and-compose.md` §3.4](10-deployment-and-compose.md#34-startup-self-check-mandatory).

| Result | Recorded | Operator-visible effect |
|---|---|---|
| Same `st_dev`, `link()` succeeded | `hardlinks_available=true` | None. Moves are atomic `rename(2)`. |
| Same `st_dev`, `link()` failed | `hardlinks_available=false` | Persistent UI banner. exFAT, CIFS, NFS and FUSE unions report one device but reject `link(2)`, so `st_dev` alone is not evidence. |
| Different `st_dev` | `hardlinks_available=false` | Persistent UI banner: moves become copy-and-delete and take minutes on large files. |

The self-check **never fails the boot**. A wrong mount layout is a performance and disk-usage problem, not a
correctness one.

### 1.5 Stage S9 — capability probe and conformance

The probe runs at `Connect()` per engine and records the resolved versions in `engines.version`:
`aria2.getVersion`, qBittorrent `app/version` plus `app/webapiVersion`, `<DLTOOL_YTDLP_PATH> --version` and
`<DLTOOL_JS_RUNTIME_PATH> --version`. A missing JavaScript runtime raises `js_runtime_missing`, disables the
media lane and shows a warning; it never fails a download silently
([`06-download-engines.md` §7.6](06-download-engines.md#76-packaging-pinning-and-the-boot-capability-probe)).

Conformance then asserts that each engine's competing automation is off — qBittorrent
`rss_processing_enabled`, `scheduler_enabled` and `auto_tmm_enabled` all `false`, no search plugins
installed, and every engine's internal queue limit raised out of the way. The authoritative table is
[`06-download-engines.md` §9](06-download-engines.md#9-engine-conformance-at-boot). A conformance failure is
a visible warning with a "fix it for me" action. **It is never a crash and never blocks start-up**, because
an operator locked out of the UI cannot fix the setting the warning is about.

An engine that does not answer at S9 is marked disconnected and retried in the background; boot continues.
Task creations routed to it fail with `engine_unavailable` until it returns.

### 1.6 Stage S10 — boot reconciliation

In-memory state never survives a restart; the database is the only source of truth. For every task not in a
terminal state (`completed`, `removed`, `error`), match on `(engine, engine_ref)` and apply:

| Engine holds the handle | dl-tool row said | Action |
|---|---|---|
| yes | any non-terminal state | Adopt the engine's reported state through the normalisation table in [`06-download-engines.md`](06-download-engines.md). Emit `task.reconciled`. |
| no | `downloading`, `seeding`, `checking` | Re-submit from the stored source with resume semantics (`--continue` for aria2 and yt-dlp; re-add by infohash for qBittorrent), then adopt the new `engine_ref`. |
| no | `queued`, `paused` | Leave as is. The admission controller re-submits when a slot frees. |
| n/a | `moving`, `extracting` | Owned by the `jobs` table, not the engine: reset the `running` row to `pending` and let the worker retry. Both handlers are idempotent. |
| yes, unknown to dl-tool | — | A foreign task. Ignore it — it never enters the queue ([ADR-0017](decisions/0017-exclusive-control-of-engines.md)). |

<!-- UNVERIFIED: whether an aria2 GID survives a daemon restart through `--save-session` / `--input-file` was
     not confirmed in the research corpus. Implement the "no handle" row as the expected path for aria2. -->

Reconciliation writes one `task_events` row per task it changed, so the History tab explains a post-restart
state change instead of the user discovering it.

---

## 2. Shutdown

`docker stop` sends `SIGTERM` and then `SIGKILL` after the grace period, which defaults to **10s** for Linux
containers — far too short for a WAL checkpoint on NAS disks. The shipped compose therefore sets
`stop_grace_period` per service ([`10-deployment-and-compose.md` §2](10-deployment-and-compose.md#2-composeyaml)):

| Service | `stop_grace_period` | Why |
|---|---|---|
| `dl-tool` | `60s` | Drain, checkpoint, close. Target under 20s; the margin covers slow NAS disks. |
| `qbittorrent` | `120s` | libtorrent writes resume data for every torrent on shutdown. A kill before it finishes forces a full re-check of those torrents on next start. linuxserver's sample uses `10s` and the official `qbittorrent-nox` compose uses `30m`; raise it further for very large libraries. |
| `aria2` | `30s` | Writes `/config/aria2.session` on exit. |

dl-tool's `SIGTERM` handler, in order:

1. Stop accepting new HTTP requests; `/readyz` starts returning `503` so a proxy drains it.
2. Cancel the cron scheduler and stop claiming new `jobs` rows. In-flight jobs run to their next checkpoint.
3. Cancel every running yt-dlp `exec.CommandContext`; the partial file plus `--continue` makes the respawn a
   resume, not a restart.
4. Flush task state and `engine_ref` values to SQLite. Engine-side transfers are **not** paused — pausing on
   every restart would break seeding and confuse the user.
5. Close SSE connections and the job workers.
6. `PRAGMA wal_checkpoint(TRUNCATE)`, close the database, release and close the process-lock descriptor,
   then exit 0.

A clean shutdown checkpoints and removes `dl-tool.db-wal` and `dl-tool.db-shm`, leaving only `dl-tool.db`.
Their presence after a stop means the shutdown was killed, not graceful.

---

## 3. Backup and restore

### 3.1 What is backed up

| Artefact | Mechanism | Covers |
|---|---|---|
| `/config/dl-tool.db` | `VACUUM INTO`, nightly and pre-migration | Everything: users, tasks, settings, indexers, feeds, rules, schedule. |
| `/config/secrets.env` | Operator copies the file | The at-rest secret key `DLTOOL_SECRET_KEY` and `ARIA2_RPC_SECRET`. Losing it makes every stored `*_enc` secret undecryptable (re-enter them) and forces an aria2 secret re-share; the file is mode `0600`. |
| Portable settings | `GET /settings/export` | Configuration without identities — safe to attach to a bug report ([`05-api-contract.md` §11.5](05-api-contract.md#115-get-settingsexport-and-post-settingsimport)). |

### 3.2 The nightly job

A `robfig/cron/v3` entry runs `VACUUM INTO` once per night and retains the newest **7** files matching
`dl-tool.db.<UTC>.bak`. It never counts pre-migration or replaced-database backups. The same operation backs
`POST /system/backup`. Semantics, including retention, are owned by
[`04-data-model.md` §6](04-data-model.md#6-backup-and-restore). Two failure modes the operator will meet:

- **The target file must not already exist** (an existing *empty* file is accepted); otherwise the statement
  fails with an error. Every run generates a fresh UTC timestamp and never reuses a name.
- An **interrupted** `VACUUM INTO` — power loss, an unplanned stop — can leave an incomplete and corrupt
  output. dl-tool integrity-checks and fsyncs a unique temporary file, renames it into place, then fsyncs the
  directory, so a partial file is never mistaken for a good backup.

Never copy `dl-tool.db` directly: while running, that loses committed WAL contents; while stopped, stale
sidecars can later be applied to the wrong database. Use the restore command, whose process lock, checkpoint
and atomic rename cover both cases.

### 3.3 Direct file replacement is unsupported

Do not remove, overwrite or copy the database or its sidecars by hand. Use §3.4, which leaves the original
database intact until a checked replacement is ready and preserves a rollback snapshot.

### 3.4 `dl-tool restore --from <file>`

This is the only supported restore path
([FR-146](02-requirements.md#fr-146-restore-a-backup-from-the-command-line)).

```bash
docker compose stop dl-tool
docker compose run --rm dl-tool restore --from /config/backups/dl-tool.db.20260901T030000Z.bak
docker compose start dl-tool
```

| Gate | Check | Refusal |
|---|---|---|
| Not running | Acquire `flock(LOCK_EX\|LOCK_NB)` on the stable process lock before opening either database. | Exit 1, `restore_server_running`, naming the PID recorded by the lock holder. |
| Source path | Resolve to a regular file inside `DLTOOL_CONFIG_DIR`; reject the live database, lock and sidecars. | Exit 1, `restore_source_rejected`; the live database is untouched. |
| Schema version | The backup's `MAX(version_id)` must not exceed the highest embedded migration. Older is valid. | Exit 1, `restore_schema_too_new`, printing both versions. |
| Integrity | `PRAGMA integrity_check` on the backup, opened read-only. | Exit 1, `restore_integrity_failed`; the live database is untouched. |

Every gate completes before the command changes the live database. On success:

1. Copy the source to a unique `dl-tool.db.restore-<ULID>.tmp` beside `DLTOOL_DB_PATH` using `O_EXCL`, set
   `PUID:PGID` and mode `0600`, fsync it, then integrity-check the staged copy.
2. If a live database exists, create `dl-tool.db.replaced-<UTC>.bak` through `VACUUM INTO` a unique temporary
   path; integrity-check and fsync it, rename it into place and fsync the directory.
3. Checkpoint the live database with `PRAGMA wal_checkpoint(TRUNCATE)`, close every SQLite handle, then remove
   its now-stale `-wal` and `-shm` sidecars. A crash here leaves the complete old database plus its backup.
4. Atomically rename the staged copy over `DLTOOL_DB_PATH` and fsync the directory. A crash yields either the
   complete old file or the complete checked replacement, never a partially copied live file.
5. Print the restored task count. The next server boot migrates an older restored schema forward.

Any failure before step 4 removes only the temporary stage and leaves the live database untouched.

### 3.5 Settings export and import

`GET /settings/export` and `POST /settings/import` move configuration between instances without carrying a
database file. The export **deliberately excludes** sessions, users and password hashes, API tokens, engine
and notification-channel secrets, indexer API keys and every task table. It is not a backup: after importing
settings into a fresh instance, the operator still creates the first admin through the setup wizard.

---

## 4. Upgrade and rollback

1. **Pin.** The shipped compose pins a major tag, `ghcr.io/l-k-m/dl-tool:1`, never `:latest`. Pin the engine
   images too — a floating `lscr.io/linuxserver/qbittorrent:latest` can change the libtorrent version
   underneath a running library.
2. **Upgrade.** Run `docker compose pull && docker compose up -d`. When a migration is pending, startup takes
   the unique pre-migration backup from §1.3. Do not prune the previous image until the rollback window has
   expired and the backup has been tested.
3. **Verify.** `curl -fsS localhost:8091/healthz`, then check `GET /system/info` for the expected `version`
   and `database.schema_version`.
4. **Roll back.** Restore the pre-migration backup **and** pin the previous tag. Doing only one of the two
   fails: an old binary against a new schema stops at `schema_too_new` (§1.3), and a new tag against an old
   database simply re-migrates.

```bash
docker compose stop dl-tool
docker compose run --rm dl-tool restore \
  --from /config/backups/dl-tool.db.pre-migration-7-to-8.20260901T030000Z.bak
# edit compose.yaml: image: ghcr.io/l-k-m/dl-tool:1.3.7
docker compose up -d
```

**Do not use `containrrr/watchtower`.** It was archived (read-only) on 2025-12-17 and its maintainers
declined to bless any fork. Community forks exist; none is endorsed here. Unattended auto-update of a
download manager runs a schema migration with nobody watching — use a monitor-only notifier and pull by hand.

---

## 5. Disk management

| Rule | Value | Behaviour |
|---|---|---|
| Reservation | `free = f_bavail * f_frsize`; `committed = Σ(total_bytes - completed_bytes)` over active tasks on the same filesystem | A new task is admitted only when `free - committed - min_free_space >= announced size`. Unknown size (a magnet before metadata, an HTTP response with no `Content-Length`) skips the pre-check and is re-checked when the size resolves. |
| `min_free_space` | Per root, default **2 GiB** | Head-room that is never allocated to a task. |
| Low-space monitor | Every **30s** per active destination | Below `min_free_space`, **auto-pause** the active tasks on that filesystem with error code `disk_full` and raise a banner, rather than letting an engine write until `ENOSPC`. |
| `ENOSPC` | From any write path | Set `disk_full` and keep the task resumable. **Partial data is never deleted**; the task resumes when space returns. |
| Hardlinks | — | A hardlinked library copy consumes no extra space, so `du` over `/data` double-counts. Free space comes from `statvfs`, never from `du`. |

Storage quota (`users.quota_bytes` → `quota_exceeded`) and the concurrency limits (`max_active_*` →
`concurrency_limit`) are separate mechanisms with separate codes; see
[`04-data-model.md` §4.7](04-data-model.md#47-storage-quota-versus-concurrency-limit).

---

## 6. Log redaction

Secrets never reach a log record, an error message, `GET /system/logs` or a diagnostics bundle, at any level
including `debug`. Values are typed `secure.Secret`, whose `String`, `Format` and `MarshalJSON` render
`[REDACTED]`; the API placeholder is the literal `"__redacted__"`
([`11-config-reference.md` §6](11-config-reference.md#6-secrets)).

| Never logged | Where it lives |
|---|---|
| `DLTOOL_ARIA2_SECRET`, `DLTOOL_QBITTORRENT_PASSWORD` | Environment or `_FILE`; the generated `ARIA2_RPC_SECRET` also rests in `/config/secrets.env` |
| At-rest secret key `DLTOOL_SECRET_KEY` | `/config/secrets.env` |
| Password hashes, API token values | `users`, `api_tokens` |
| Indexer `api_key`, notification `secret_enc`, engine `secret_enc` | Configuration tables |
| `extract_passwords` | `settings` |
| Headers `Authorization`, `Cookie`, `X-Api-Key` | Requests |
| Query or path parameters `apikey`, `token`, `passkey` | Indexer and tracker URLs, which routinely embed a per-user passkey |
| URL userinfo (`https://user:pass@host/…`) | FTP and HTTP task sources |

Redaction happens **before the record is stored**, not at read time, so an operator who reads
`/config/logs/` directly sees the same redacted text.

---

## 7. Diagnostics bundle

```bash
docker compose exec dl-tool dl-tool diagnostics --out /config/diagnostics-20260901T094500Z.tar.gz
```

One command produces one file that is safe to attach to a public issue. It reads the database read-only and
works whether or not the server is running.

| Collected | Detail |
|---|---|
| `system-info.json` | The `GET /system/info` document: version, commit, schema version, engine health and versions, task counts by state, job counts, active schedule mode and time zone. |
| `config.json` | Every resolved configuration value **with secrets replaced by `[REDACTED]`**, plus which source (env, `_FILE`, database, default) supplied each one. |
| `logs.ndjson` | The last 5 000 structured log records, already redacted per §6. |
| `boot-checks.json` | Filesystem type of the database directory, `hardlinks_available` per root, `statvfs` free space per root, `PRAGMA integrity_check`, applied schema version, entrypoint UID/GID/umask/TZ. |
| `conformance.json` | The §1.5 probe results and every conformance assertion with its observed value. |
| `schema.sql` | `sqlite3 .schema` equivalent — DDL only. |
| `counts.csv` | Row counts per table. |
| `engine-versions.txt` | `aria2.getVersion`, qBittorrent `app/version` and `app/webapiVersion`, `yt-dlp --version`, JS-runtime `--version`. |

| Deliberately excluded | Reason |
|---|---|
| The database file itself, and any `/config/backups/*` | Contains password hashes, API tokens and every session. |
| `/config/secrets.env` | `DLTOOL_SECRET_KEY` and `ARIA2_RPC_SECRET`. |
| `users`, `sessions`, `api_tokens` rows | Identities and credentials. |
| Task names, source URIs, destinations, file names | A file list is the most privacy-sensitive data in the product. Only counts and state histograms are included. |
| Indexer URLs and API keys, feed URLs, tracker URLs | Carry per-user passkeys. |
| Anything under `/data` | Never read by this command. |

The bundle is written with mode `0600` owned by `PUID:PGID`. There is no endpoint that returns it: it is a
deliberate, local operator action.

---

## 8. Symptom, cause, fix

| Symptom | Most likely cause | Fix |
|---|---|---|
| Tasks sit in `queued` with error code `concurrency_limit` and nothing starts | A `max_active_*` limit is reached. Seeding does not count toward any of them, so a queue of seeders is not the cause. | Raise `max_active_total`, `max_active_per_engine` or `max_active_per_user` in Settings. The engines' own queues are deliberately raised out of the way — raising them does nothing. |
| Speeds are far below the configured limit, and no per-task limit is set | A 24×7 schedule cell is active and applying the alternative-speed pair. | Check the active mode on `GET /system/info` (`schedule.active_mode`) and the grid in Settings. Precedence is `min(cell, global, per-task)` → [`06-download-engines.md` §10](06-download-engines.md#10-bandwidth-precedence-and-fan-out). |
| Everything dl-tool started is paused at the same minute every day | The active cell is `0` No Download, which **pauses** rather than throttles. Tasks the user paused stay paused. | Edit that cell. dl-tool resumes exactly the tasks it paused when the cell changes. |
| Files land somewhere other than the chosen destination, and move again later | qBittorrent's Automatic Torrent Management is on and relocates by category, overriding `tasks.destination`. | Apply the conformance warning's "fix it for me" action, which sets `auto_tmm_enabled=false` (§1.5). Then move the affected tasks with `PATCH /tasks/{id}`. |
| A completed task sits in `moving` for minutes and the disk is busy | Downloads and library are on different mounts, so `rename(2)` returned `EXDEV` and the move is a copy-and-delete. | Put both under the single `/data` mount ([ADR-0012](decisions/0012-single-data-mount.md)). Confirm with the banner and `hardlinks_available` from §1.4. |
| Hardlinks unavailable although both paths are one mount | `st_dev` matches but `link(2)` is rejected — exFAT, CIFS, NFS or a FUSE union. | Move `/data` to a filesystem that supports hard links. Nothing in dl-tool can work around it. |
| Task creation fails with `engine_unavailable`; the engine tile is red | The engine container is down, or its URL, port or secret is wrong. Engine ports are never published, so only the compose network reaches them. | `docker compose ps`, then `POST /engines/{id}/test` for the exact transport error. Check `DLTOOL_ARIA2_SECRET` matches `ARIA2_RPC_SECRET`; a rotated secret logs `aria2_secret_rotated` and needs an aria2 restart. |
| Log lines `database is locked`; the UI stalls | `/config` is on a network filesystem — dl-tool should have refused to start, so a bind mount was changed under a running container. Otherwise a second process holds the file. | Move `/config` to local storage; restart. Never run two dl-tool containers against one database. |
| Every active task pauses with `disk_full` while the disk still shows free space | Free space fell below `min_free_space` on that root, or the reservation for committed-but-unwritten bytes is exhausted. | Free space or lower `min_free_space`. Tasks resume by themselves; no partial data was deleted (§5). |
| `ENOSPC` from a watch folder while the disk is empty | inotify watches are exhausted. `fs.inotify.max_user_watches` is a **host** kernel setting a container inherits. | On the host: `echo "fs.inotify.max_user_watches=524288" \| sudo tee -a /etc/sysctl.conf && sudo sysctl -p`. dl-tool falls back to 10s polling instead of crashing. |
| YouTube and other media downloads fail; task events show `js_runtime_missing` | No JavaScript runtime at `DLTOOL_JS_RUNTIME_PATH`. yt-dlp needs one for full YouTube support; the media lane is disabled, not silently broken. | Use the official image, which ships `nodejs`, or set `DLTOOL_JS_RUNTIME_PATH` to an existing runtime and restart. |
| Media downloads that used to work now fail to extract | The pinned yt-dlp build has aged out; extractors rot fast and runtime self-update is disabled by design ([ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md)). | `docker compose pull && docker compose up -d`. The weekly rebuild bumps the pin. |
| Search returns nothing for every query | No indexer is enabled, or the bundled engines are disabled. Imported definitions start **disabled** on purpose. | `GET /indexers`, enable at least one, then `POST /indexers/{id}/test` for the per-indexer error. |
| Search returns nothing from one indexer only | Wrong API key, wrong category mapping, or an unreachable host blocked by SSRF protection. | `POST /indexers/{id}/test`; a blocked host reports `ssrf_blocked`. Private-range indexers need `DLTOOL_SSRF_ALLOW_PRIVATE=true`. |
| Container restarts on start-up with exit code 1 and one `error` record | A fail-fast condition from §1.2 or §1.3. | Read the `err_code` in that record and look it up in [`11-config-reference.md` §8](11-config-reference.md#8-boot-validation). Never add a restart loop to work around it. |
| The UI loads but assets 404 behind a reverse proxy | `DLTOOL_BASE_PATH` does not match the proxy's prefix. | Set both to the same value → [`10-deployment-and-compose.md` §7.3](10-deployment-and-compose.md#73-base-path-requirements). |

---

## Decisions referenced

| ADR | Decision |
|---|---|
| [ADR-0004](decisions/0004-sqlite-as-the-only-datastore.md) | SQLite as the only datastore — hence WAL, `VACUUM INTO` and the network-filesystem refusal |
| [ADR-0011](decisions/0011-alpine-runtime-with-puid-pgid.md) | Alpine runtime image with a `su-exec` PUID/PGID privilege drop |
| [ADR-0012](decisions/0012-single-data-mount.md) | A single `/data` mount — hence the hardlink self-check |
| [ADR-0015](decisions/0015-db-backed-in-process-job-queue.md) | DB-backed in-process job queue — hence job reset on boot |
| [ADR-0017](decisions/0017-exclusive-control-of-engines.md) | Exclusive control of the configured engines — hence conformance and ignored foreign tasks |
| [ADR-0018](decisions/0018-pin-ytdlp-by-version-and-hash.md) | Pin yt-dlp by version and hash; never self-update at runtime |

## Open questions

- (none — the backup-naming ambiguity was resolved 2026-09-01 by correcting the example in
  [`05-api-contract.md`](05-api-contract.md) §13 to the `dl-tool.db.<UTC>.bak` form this document and
  [`04-data-model.md`](04-data-model.md) §6 already use)

## Change log

| Date | Change |
|---|---|
| 2026-09-01 | Initial version. |
| 2026-09-01 | `secrets.env` described correctly: `DLTOOL_SECRET_KEY` (the at-rest encryption key behind every `*_enc` column) and `ARIA2_RPC_SECRET` — no session or CSRF keys exist. |
| 2026-09-01 | Review pass: the never-logged table names `secrets.env` as an at-rest home of the aria2 secret, so an inventory or scrub does not miss the stale copy. |
| 2026-09-01 | Resolved the backup-naming open question: the §13 example in [`05-api-contract.md`](05-api-contract.md) was corrected to the `dl-tool.db.<UTC>.bak` form used here and in [`04-data-model.md`](04-data-model.md) §6. |
| 2026-09-01 | Made migration backup and database restore idempotent, lock-protected and crash-safe. |

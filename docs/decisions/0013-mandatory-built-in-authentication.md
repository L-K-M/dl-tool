# 0013 - Mandatory built-in authentication

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

> **Amended 2026-09-02 by [ADR-0019](0019-single-account-no-ownership.md):** the multi-user reasoning
> below is withdrawn — there is one account, no roles and no ownership. The decision itself —
> authentication is mandatory, with no anonymous mode and no default credentials — stands, on the
> exposure grounds rather than the identity ones.

## Context and Problem Statement

Absent or default authentication is the dominant root cause of critical vulnerabilities in this software
class. qBittorrent shipped `admin`/`adminadmin` with no forced change; CVE-2023-30801 (CVSS 9.8,
**reportedly** exploited in the wild in March 2023) chained those defaults to "run external program on
completion" for unauthenticated remote code execution, and issue #18731 carries the victim's own log line
dropping an XMRig miner. SABnzbd's advisory for CVE-2023-34237 (GHSA-hhgh-xgh3-985r; CNA 8.1, NVD 9.8) says
"By default SABnzbd is only accessible from `localhost`, with no authentication required for the web
interface" — safe until someone exposes it; Deluge CVE-2017-9031 was an unauthenticated file read. dl-tool
also needs identity for product reasons: per-user destination, quota and task ownership are core features.

## Decision Drivers

- Every incident above was exploited at scale only because the web UI was reachable and unauthenticated, so
  the first-run state is the security-critical state.
- Multi-user is a product requirement: a user's destination jail, quota and task list all key off an
  identity that must exist before the first task.
- A home server that permanently locks its only admin out is a support disaster, so hardening is rate
  limiting rather than lockout. And because the implementing agent is a weaker model, "authentication can be
  switched off" becomes "authentication is off in the deployment we ship" through one wrong default.

## Considered Options

- **Option A** — Mandatory built-in local users and server-side sessions, first run gated by a one-time token.
- **Option B** — SABnzbd's posture: no app authentication, bind to loopback, document a reverse proxy.
- **Option C** — qBittorrent's post-4.6.1 model: a random temporary password printed to stdout at each start.
- **Option D** — Delegate to the reverse proxy: trusted-header forward auth or OIDC, no local password store.

## Decision Outcome

Chosen option: **Option A, mandatory built-in authentication**, because it is the only option safe in the
state the software ships in — first boot, before the operator has configured anything — and the only one
that gives dl-tool the identity its multi-user features require.

Concretely: on first start with an empty `users` table dl-tool generates a one-time setup token, prints it
to stdout and writes `<config>/setup-token` mode `0600`; every endpoint except `POST /auth/setup` returns
`401` with `/problems/setup-required`; `POST /auth/setup` consumes the token, requires a password of at
least 12 characters, deletes the token file and returns `409` thereafter. Passwords are argon2id
(`m=19456`, `t=2`, `p=1`) stored as a PHC string, and no admin password is ever read from an environment
variable — it would land in `docker inspect` and shell history. See
[`../12-security-and-threat-model.md`](../12-security-and-threat-model.md).

### Consequences

- Good, because there is no state in which dl-tool serves data without credentials, so exposing the port
  cannot silently mean exposing the queue and the filesystem browser. Ownership, quotas and the destination
  jail also have a subject from the first request, which Options B and C cannot provide.
- Bad, because dl-tool is stricter than the precedent it cites: Sonarr's "mandatory" authentication keeps
  two config-file escape hatches, `AuthenticationMethod: External` and
  `AuthenticationRequired: DisabledForLocalAddresses`. dl-tool ships neither, so an operator who wants a
  frictionless LAN-only kiosk cannot have one, and a lost password needs the documented recovery path in
  [`../17-operations-and-runbook.md`](../17-operations-and-runbook.md).
- Neutral, because forward auth and OIDC remain supported as an **additional** layer; they never replace the
  local session, and proxy headers are never trusted as identity. Brute-force handling is per-account
  exponential backoff from 1 s to a 15-minute cap plus a per-IP bucket of 10 attempts per 5 minutes,
  returning `429` — never a lockout, which is itself a denial-of-service primitive.

### Confirmation

The first-run state is the state to test; it is asserted by
[FR-115](../02-requirements.md#fr-115-complete-a-first-run-setup-using-a-one-time-token) and
[NFR-011](../02-requirements.md#nfr-011-ship-no-default-credentials), task T009:

```bash
make test PKG=./internal/api/...
grep -rniE "adminadmin|DLTOOL_ADMIN_PASSWORD|anonymous" --include='*.go' cmd internal
```

Expected: exit 0, including a table-driven test in `internal/api/auth_test.go` that walks the registered
Huma operations and asserts every one except `POST /auth/setup` and `POST /auth/login` returns `401`
without credentials; `grep` prints nothing and exits 1.

## Pros and Cons of the Options

### Option A - mandatory built-in users and sessions

- Good, because the shipped default is the safe one — the only property that mattered in all four incidents,
  and sessions are server-side rows, so an admin can revoke one and rotating the key kills all of them.
- Bad, because dl-tool now owns password storage, hashing parameters and rate limiting.

### Option B - no authentication, loopback only, proxy in front

- Good, because it is the least code and cannot leak a password store it does not have.
- Bad, because it is the posture SABnzbd's own advisory blames for CVE-2023-34237, self-hosters publish the
  port anyway, and per-user quotas and destinations become impossible.

### Option C - random temporary password printed at each start

- Good, because it is a real improvement over fixed defaults and is what qBittorrent shipped in 4.6.1.
- Bad, because the credential lives in container logs, more widely readable than a `0600` file, and
  regenerates at every restart until an admin intervenes.

### Option D - forward auth or OIDC only

- Good, because it gives single sign-on, MFA and a central policy the app need not implement.
- Bad, because header identity is forged by anything reaching the port directly, so it is safe only when the
  proxy is the sole network path, and it makes a proxy mandatory for a `docker compose up` product.

## More Information

- Research: `security.md` §5.3–§5.5, §6 rows 2, 6, 11, 14 and fact-check FC1, FC2 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../12-security-and-threat-model.md`](../12-security-and-threat-model.md),
  [`../05-api-contract.md`](../05-api-contract.md) and [`../09-web-ui-spec.md`](../09-web-ui-spec.md).

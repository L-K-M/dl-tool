# 0019 - One account, no ownership model

> **Status:** accepted
> **Date:** 2026-09-02
> **Deciders:** repository owner

## Context and Problem Statement

[ADR-0001](0001-control-plane-over-existing-engines.md) justified building dl-tool partly on multi-user:
task ownership, per-user default destinations and per-user quotas, a capability no self-hosted download
engine has — qBittorrent issue #3327 has been open roughly a decade.

That reasoning is sound, and it is not what the owner asked for:

> "I just want a tool that basically does the same thing from the user's pov and that I can host wherever
> I want using docker compose."

Multi-user was the single largest source of cross-cutting complexity in the plan. Ownership threads
through the task store, every read endpoint, the live-update projection, the filesystem jail, quota
admission, the RSS rule engine, search-result authorisation and the settings UI — 34 of 122 task files.
None of it is visible to a household running one instance for itself.

## Decision Drivers

- The product goal is Download Station's *experience*, not its user model. Download Station inherits DSM's
  accounts; dl-tool has no DSM to inherit from and would have to build the whole thing.
- Every ownership check is a place to get authorisation subtly wrong. The plan already had to fix two:
  live updates leaking other users' tasks and aggregates ([#9](https://github.com/L-K-M/dl-tool/pull/9)),
  and search responses exposing another user's results ([#32](https://github.com/L-K-M/dl-tool/pull/32)).
- Authentication is not the same thing as multi-user, and is not in question:
  [ADR-0013](0013-mandatory-built-in-authentication.md) stands.
- Nothing here forecloses multi-user later. It is additive: an `owner_id` column, a role check and a jail.

## Considered Options

- **Option A** — Keep the full multi-user model: roles, ownership, per-user destinations, storage and
  concurrency quotas, and a per-user filesystem jail.
- **Option B** — One account. Authentication stays; roles, ownership and per-user limits go.
- **Option C** — Several logins sharing everything: keep the `users` table and login separation, drop
  ownership and quotas.

## Decision Outcome

Chosen option: **Option B, one account**, because it removes the entire authorisation surface rather than
leaving a weakened version of it, and because the differentiator it gives up was never what the owner
asked for.

dl-tool authenticates exactly one operator account, created by the first-run setup wizard. There are no
roles, no `owner_id` on any row, no per-user destination, quota or jail, and no user-management API beyond
changing that account's own password.

**Retained, and not to be confused with multi-user:**

- Authentication itself, the setup token and the no-default-credentials rule (ADR-0013).
- API tokens, which are for automation, not for separating people.
- The filesystem jail against `DLTOOL_DATA_ROOTS`. That is path-traversal defence and stays exactly as
  specified; only the *per-user subtree* jail is gone.
- Opaque search-result ids. Those hide tracker passkeys from browser payloads, which is true of one user
  as much as of ten.
- Global admission control: `max_active_total` and `max_active_per_engine`. Only `max_active_per_user` goes.

### Consequences

- Good, because 34 task files simplify and the authorisation surface disappears rather than shrinking.
- Good, because every read endpoint, the SSE projection and the rule engine lose a filtering step that had
  to be correct everywhere and was wrong twice.
- Bad, because a household sharing one instance shares one login and sees each other's downloads. That is
  the accepted cost; Download Station's own answer to this needs DSM accounts.
- Neutral, because the schema keeps a single-row `users` table rather than inventing a different shape, so
  adding accounts later is a migration, not a redesign.

### Confirmation

No ownership column and no role check may exist:

```bash
grep -rn "owner_id\|role IN (\|non-admin" --include='*.go' --include='*.sql' cmd internal
```

Expected: no output, exit 1 from `grep`. `T009` additionally asserts that a second call to the setup
endpoint returns `409` once the single account exists.

## Pros and Cons of the Options

### Option A - full multi-user

- Good, because it is a genuine gap in every self-hosted download client, and Flood is the only project
  that has it.
- Bad, because it is the plan's largest cross-cutting concern for a capability the owner did not ask for.
- Bad, because authorisation bugs are silent: the two found in review both leaked data while every test passed.

### Option B - one account

- Good, because the simplest correct answer to "who may do this" is "the operator, who is the only user".
- Bad, because it gives up the differentiator ADR-0001 partly rested on. ADR-0001's other three —
  one queue across protocols, the server-side destination browser, and the cross-engine bandwidth
  governor — are untouched and remain reasons to build this.

### Option C - shared logins without ownership

- Good, because it keeps per-person credentials, so revoking one person does not rotate everyone's.
- Bad, because it is the confusing middle: separate logins that imply separation the product does not
  provide. A household that wants that wants Option A.

## More Information

- Supersedes the multi-user differentiator in [`../01-vision-and-scope.md`](../01-vision-and-scope.md) and
  the ownership clauses of [ADR-0001](0001-control-plane-over-existing-engines.md).
- Authentication is unchanged: [ADR-0013](0013-mandatory-built-in-authentication.md).
- Affected documents: [`../02-requirements.md`](../02-requirements.md),
  [`../04-data-model.md`](../04-data-model.md), [`../05-api-contract.md`](../05-api-contract.md),
  [`../09-web-ui-spec.md`](../09-web-ui-spec.md), [`../12-security-and-threat-model.md`](../12-security-and-threat-model.md).

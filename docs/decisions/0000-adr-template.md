# NNNN - Short title naming the problem and the chosen solution

> **Status:** proposed | accepted | superseded by [ADR-NNNN](NNNN-slug.md)
> **Date:** YYYY-MM-DD
> **Deciders:** repository owner

## Context and Problem Statement

State the forces in two to four sentences. Name the concrete constraint that makes this a decision rather
than a preference.

## Decision Drivers

- Driver one, stated as a quality or a constraint, not as a preference.
- Driver two.

## Considered Options

- **Option A** — one line on what it is.
- **Option B** — one line on what it is.
- **Option C** — one line on what it is.

Every option listed here must be a real alternative with a real advantage. An option that nobody could
have chosen does not belong in this list.

## Decision Outcome

Chosen option: **Option B**, because <the single decisive reason, naming the driver it satisfies>.

### Consequences

- Good, because <positive consequence>.
- Bad, because <negative consequence that is accepted, not hidden>.
- Neutral, because <consequence that is neither>.

### Confirmation

Name something executable. A `make` target, a CI job, a named test, or a `grep` whose expected output is
stated. "Code review" is not a confirmation.

```bash
make lint && make test PKG=./internal/<pkg>/...
```

Expected: exit 0.

## Pros and Cons of the Options

### Option A

- Good, because <argument>.
- Bad, because <argument>.

### Option B

- Good, because <argument>.
- Bad, because <argument>.

### Option C

- Good, because <argument>.
- Bad, because <argument>.

## More Information

- Research: `<report>.md` §N, summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../03-architecture.md`](../03-architecture.md).

## Notes for the author

- File name is `NNNN-kebab-title.md`, four digits, allocated in [`README.md`](README.md).
- Keep the file between 40 and 110 lines.
- Headings above are mandatory and must appear in this order.
- Once the status is `accepted`, no other document re-argues the decision; they link here instead.

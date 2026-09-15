# 0020 - golang.org/x/text provides NFC and case folding for path safety

> **Status:** accepted
> **Date:** 2026-09-14
> **Deciders:** repository owner

## Context and Problem Statement

[`12-security-and-threat-model.md`](../12-security-and-threat-model.md) §3.2 step 3 requires every
path segment to be Unicode-normalised to NFC, and §3.3 rule 6 keys the per-download collision map on
the case-folded name. The Go standard library ships neither normalisation nor full case folding, so
conforming code needs a module that T004's pinned list does not name — which hard rule 3 permits only
with this record.

## Decision Drivers

- Segments arrive in attacker-controlled torrent metadata; NFC and folding are the security rules the
  §3.4 hostile-path table pins — rows 23 and 30 cannot pass without them.
- `golang.org/x/text` is the Go project's own extension repository: `unicode/norm` implements the
  standard's normalisation forms and `cases.Fold` implements full case folding, both conformance-
  tested against the Unicode data the toolchain ships.
- Hard rule 3 keeps the dependency surface reviewed; this record is that review.

## Considered Options

- **Option A** — Add `golang.org/x/text`, pinned in `go.mod` like every other module.
- **Option B** — Drop §3.2 step 3 and weaken §3.3 rule 6 to `strings.EqualFold`, leaving the pinned
  list unchanged.
- **Option C** — Maintain private NFC and folding tables inside `internal/fsx`.

## Decision Outcome

Chosen option: **Option A**, because §3.2 and §3.3 are accepted requirements that no pinned or
standard-library facility implements, and x/text is the reference implementation the Go project
itself ships.

`golang.org/x/text v0.41.0` sits in `go.mod`'s direct require block; the version follows the module
graph like every other pin and changes only through the same review as any direct require. Only
`unicode/norm` and `cases` are imported, and only by `internal/fsx/safepath.go`.

### Consequences

- Good, because the §3.4 rows that rely on NFC and on case-insensitive collision handling are
  enforced by a conformance-tested implementation rather than a private table.
- Good, because `internal/uri`'s segment sanitiser can adopt `fsx.SanitiseSegment` — the comment in
  `metainfo.go` already assigns it the NFC step once this module exists.
- Bad, because the dependency surface grows by one module, and later tasks must not pull in further
  `x/text` subpackages without the same review.
- Neutral, because `go.sum` already carried x/text `go.mod` hashes transitively; the change is its
  promotion to an imported, versioned module.

### Confirmation

```bash
grep -n 'golang.org/x/text' go.mod
grep -rln '"golang.org/x/text' --include='*.go' .
make test PKG=./internal/fsx/...
```

Expected: one direct-require line naming `v0.41.0`; exactly one importing file,
`internal/fsx/safepath.go`; and exit 0 with `TestSanitiseSegmentTable` running all thirty §3.4 rows.

## Pros and Cons of the Options

### Option A - add golang.org/x/text

- Good, because it is the reference implementation, conformance-tested against the Unicode data the
  spec names.
- Bad, because it is a new direct dependency — the cost this record exists to make explicit.

### Option B - drop NFC and full folding

- Good, because `go.mod` stays exactly as T004 pinned it.
- Bad, because it deletes two accepted requirements: `strings.EqualFold` is simple folding and
  misses multi-rune folds such as `ß`, and nothing else normalises, so §3.4 row 23 could not pass.

### Option C - private Unicode tables

- Good, because no module is added.
- Bad, because the tables must be regenerated for every Unicode version forever; an unaudited copy
  of x/text's own data is strictly worse than importing it.

## More Information

- The gap this closes was predicted: `PLAN-REVIEW-FINDINGS.md` F556 records that §3.2 and §3.3 need
  x/text while T004 does not list it and T046 could not add it without this record.
- Depends on this decision: [`12-security-and-threat-model.md`](../12-security-and-threat-model.md)
  §3.2 and §3.3, [`tasks/T046-filesystem-roots-and-browse.md`](../tasks/T046-filesystem-roots-and-browse.md).

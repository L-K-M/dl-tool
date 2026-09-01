# 0016 - Relicense from the Unlicense to Apache-2.0

> **Status:** proposed - repository owner must decide
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

`LICENSE` is currently the **Unlicense**, a public-domain dedication with a permissive fallback. It is
OSI-approved (requested March 2020, granted June 2020), but that approval carried an acknowledged "general
agreement that the document is poorly drafted", and the criticisms on record are "possibly inconsistent and
non-standard", "making it difficult for some projects to accept Unlicensed code as third-party
contributions" and "possibly being incoherent in some legal systems" — public-domain dedication is not a
recognised act in several European jurisdictions. With one contributor and no external contributions, this
is the cheapest moment the choice will be made. This ADR records the analysis; it changes nothing.

## Decision Drivers

- No patent grant. Fedora considered dropping CC0 for code "because of how it excludes any patent grant,
  since it's a public domain dedication rather than a license". Google also bars employees from contributing
  to public-domain-equivalent licences, while permitting 0BSD.
- The cost is asymmetric in time: the code is already public-domain-dedicated, so **anyone may relicense the
  current snapshot without permission**. The cost arrives with the first contribution under a different
  licence, or the first contributor who objects on principle.
- The neighbourhood is copyleft — Jackett GPL-2.0, the \*arr apps GPL-3.0, qBittorrent GPL-2/3 — so copyleft
  would be unremarkable here, while yt-dlp is itself Unlicense.

## Considered Options

| Licence | Patent grant | Copyleft | Network (SaaS) clause | Contributor friction | Fit for a self-hosted server app |
|---|---|---|---|---|---|
| Unlicense | ✗ | ✗ | ✗ | High (Google bans; EU validity doubts) | Poor |
| MIT | ✗ (implied at best) | ✗ | ✗ | Lowest | Good but no patent term |
| **Apache-2.0** | **✓ explicit §3**, with a defensive termination trigger | ✗ | ✗ | Very low; corporate-friendly | **Strong** |
| GPL-3.0 | ✓ (§11) | Strong | ✗ | Medium | Fine, but no protection against a hosted fork |
| AGPL-3.0 | ✓ | Strong | **✓ §13** | Higher (some companies ban AGPL outright) | Strongest reciprocity |

**Option A** stay on the Unlicense · **Option B** Apache-2.0 · **Option C** AGPL-3.0 · **Option D** MIT.

## Decision Outcome

Chosen option: **Option B, Apache-2.0**, because it is the only candidate adding an explicit patent grant
with defensive termination at essentially zero contributor friction, and its §5 inbound clause ("Unless You
explicitly state otherwise, any Contribution intentionally submitted for inclusion in the Work … shall be
under the terms and conditions of this License") removes the ambiguity the Unlicense creates. AGPL-3.0 is
the second choice if the owner wants copyleft.

**The `LICENSE` file is UNCHANGED and remains the Unlicense until the repository owner decides.** Nothing in
the plan assumes Apache-2.0. If the owner says yes, the mechanical steps are, in order:

1. Replace `LICENSE` with the verbatim Apache-2.0 text; add `NOTICE` with `Copyright 2026 <holder>`.
2. Add `// SPDX-License-Identifier: Apache-2.0` to the head of every `.go`, `.ts` and `.tsx` file.
3. Change `org.opencontainers.image.licenses` in the `Dockerfile` `LABEL` block to `"Apache-2.0"`.
4. Add a DCO `Signed-off-by:` requirement to `CONTRIBUTING.md`; commit with a message recording that the
   previous Unlicense dedication permits the relicensing.

If the owner says no, set this ADR's status to `rejected` and leave every artefact unchanged.

### Consequences

- Good, because users and corporate contributors get an explicit patent licence and a jurisdiction-proof
  grant, and `NOTICE` gives attribution a defined home.
- Bad, because SPDX headers become a per-file convention a weaker model forgets without a lint rule, and it
  forecloses nothing against a hosted fork. Accepted; AGPL-3.0 is the option that does not accept it.
- Neutral, because the image is a separate question: the pinned yt-dlp build is GPLv3+ by its own README, so
  the image ships a GPLv3+ subprocess ([ADR-0018](0018-pin-ytdlp-by-version-and-hash.md)) either way.

### Confirmation

While this ADR is `proposed`, the check is that nothing has changed:

```bash
head -1 LICENSE | grep -qF 'This is free and unencumbered software released into the public domain.'
grep -c 'Unlicense' Dockerfile
```

Expected: exit 0, and `1`. On acceptance the check inverts into a CI gate: `LICENSE` contains `Apache
License`, `NOTICE` exists, and `grep -rL 'SPDX-License-Identifier' --include='*.go' cmd internal` is empty.

## Pros and Cons of the Options

### Option A - stay on the Unlicense

- Good, because it is the shortest possible licence, imposes nothing, and needs no work at all.
- Bad, because it has no patent grant, is banned as an inbound licence at a large employer, and may not be
  a valid legal act in parts of Europe.

### Option B - Apache-2.0

- Good, because §3 grants a "perpetual, worldwide, non-exclusive, no-charge, royalty-free, irrevocable"
  patent licence terminating for anyone filing a patent suit, and it is unambiguously valid everywhere.
- Bad, because it is permissive: a proprietary fork is allowed, and only attribution is owed back.

### Option C - AGPL-3.0

- Good, because §13 makes a modified networked version offer its Corresponding Source to its users — the
  only real protection against a hosted fork — and GPL-3.0 code may be combined into an AGPL work.
- Bad, because several organisations ban AGPL outright, and for a self-hosted tool that protection buys
  little against a measurable loss of contributors.

### Option D - MIT

- Good, because it is the lowest-friction, best-understood licence in open source.
- Bad, because its patent position is implicit at best, improving on the Unlicense only by accident.

## More Information

- Research: `security.md` §9.1, quoting Apache-2.0 §3 and AGPL-3.0 §13 from the SPDX texts — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: `LICENSE`, `NOTICE`, `CONTRIBUTING.md` and the `Dockerfile` `LABEL` block.

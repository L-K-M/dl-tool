# T097 — Publish signed multi-arch images and bump the yt-dlp pin weekly

| Field | Value |
|---|---|
| **ID** | T097 |
| **Milestone** | M7 |
| **Status** | todo |
| **Depends on** | T002, T093, T094 |
| **Blocks** | T113, T115 |
| **Parallel-safe** | yes — touches `.github/` and `CONTRIBUTING.md` only |
| **Implements** | [NFR-028](../02-requirements.md#nfr-028-harden-the-release-supply-chain), [NFR-005](../02-requirements.md#nfr-005-publish-a-multi-architecture-image) |
| **Decisions** | [ADR-0018](../decisions/0018-pin-ytdlp-by-version-and-hash.md), [ADR-0011](../decisions/0011-alpine-runtime-with-puid-pgid.md) |
| **Est. size** | 3 new files, ~330 LOC |

## Goal
A `v*.*.*` tag publishes `linux/amd64` and `linux/arm64` images to `ghcr.io/l-k-m/dl-tool` with an SBOM,
`mode=max` provenance and a keyless cosign signature. A weekly job opens a pull request that bumps the
yt-dlp version and its two SHA-256 pins; a human merges it. `CONTRIBUTING.md` carries the v1.0.0 release
checklist.

## Context you need
Read ONLY these, in this order. Do not explore the rest of the repo.
1. [`docs/10-deployment-and-compose.md` §10 Release workflow](../10-deployment-and-compose.md#10-release-workflow) — the workflow, verbatim, the four notes and the `cosign verify` command.
2. [`docs/12-security-and-threat-model.md` §8 Supply chain](../12-security-and-threat-model.md#8-supply-chain) — digest pinning, SHA-pinned actions, keyless signing, SBOM.
3. [`docs/12-security-and-threat-model.md` §8.1 yt-dlp: freshness versus unreviewed code](../12-security-and-threat-model.md#81-yt-dlp-freshness-versus-unreviewed-code) — the four-point policy the weekly job implements.
4. [`docs/13-testing-and-verification.md` §7 CI](../13-testing-and-verification.md#7-ci) — which workflow owns which job, and the pinned action majors.
5. [`docs/10-deployment-and-compose.md` §5 `Dockerfile`](../10-deployment-and-compose.md#5-dockerfile) — the three `ARG` lines the weekly job rewrites.

## Files
| Path | Action | Purpose |
|---|---|---|
| `.github/workflows/release.yml` | create | Multi-arch build, SBOM, provenance and cosign signing for `ghcr.io/l-k-m/dl-tool`. |
| `.github/workflows/ytdlp-bump.yml` | create | Weekly pull request bumping `YTDLP_VERSION` and the two hashes. |
| `.github/copilot-instructions.md` | create | Mirror of `AGENTS.md` so Copilot reads the same rules. |
| `CONTRIBUTING.md` | edit | Add the "Releasing" section with the v1.0.0 checklist and the verify command. |

No other file may be modified.

## Interface contract

`.github/workflows/release.yml` is reproduced **verbatim** from
[`docs/10-deployment-and-compose.md` §10](../10-deployment-and-compose.md#10-release-workflow) with **one
change**: the `strategy.matrix.include` list carries only the `dl-tool` entry. T115 adds the
`dl-tool-aria2` entry when that image's build context exists.

`.github/workflows/ytdlp-bump.yml`:

```yaml
name: ytdlp-bump
on:
  schedule:
    - cron: '17 4 * * 1'      # Mondays, 04:17 UTC
  workflow_dispatch:

permissions:
  contents: write
  pull-requests: write

jobs:
  bump:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - name: Resolve the newest stable yt-dlp release
        id: rel
        run: |
          set -euo pipefail
          tag=$(gh release view --repo yt-dlp/yt-dlp --json tagName -q .tagName)
          echo "tag=$tag" >> "$GITHUB_OUTPUT"
        env:
          GH_TOKEN: ${{ secrets.GITHUB_TOKEN }}
      - name: Download both musllinux assets and compute the hashes
        id: sums
        run: |
          set -euo pipefail
          tag='${{ steps.rel.outputs.tag }}'
          base="https://github.com/yt-dlp/yt-dlp/releases/download/$tag"
          curl -fsSL -o amd64 "$base/yt-dlp_musllinux"
          curl -fsSL -o arm64 "$base/yt-dlp_musllinux_aarch64"
          echo "amd64=$(sha256sum amd64 | cut -d' ' -f1)" >> "$GITHUB_OUTPUT"
          echo "arm64=$(sha256sum arm64 | cut -d' ' -f1)" >> "$GITHUB_OUTPUT"
      - name: Rewrite the three ARG lines in the Dockerfile
        run: |
          set -euo pipefail
          sed -i \
            -e 's|^ARG YTDLP_VERSION=.*|ARG YTDLP_VERSION="${{ steps.rel.outputs.tag }}"|' \
            -e 's|^ARG YTDLP_SHA256_AMD64=.*|ARG YTDLP_SHA256_AMD64="${{ steps.sums.outputs.amd64 }}"|' \
            -e 's|^ARG YTDLP_SHA256_ARM64=.*|ARG YTDLP_SHA256_ARM64="${{ steps.sums.outputs.arm64 }}"|' \
            Dockerfile
      - name: Smoke-test the pinned build
        run: docker build --build-arg TARGETARCH=amd64 --target ytdlp -t ytdlp-pin-check .
      - name: Open a pull request
        uses: peter-evans/create-pull-request@v7   # pin by commit SHA at implementation time
        with:
          branch: chore/ytdlp-pin
          title: 'chore: bump yt-dlp pin to ${{ steps.rel.outputs.tag }}'
          body: |
            Automated weekly pin bump. A human must review and merge; nothing auto-merges.
            Policy: docs/decisions/0018-pin-ytdlp-by-version-and-hash.md
          commit-message: 'chore: bump yt-dlp pin to ${{ steps.rel.outputs.tag }}'
```

The `Releasing` section added to `CONTRIBUTING.md`, as a checklist:

```markdown
## Releasing

1. Every task in `docs/tasks/00-task-index.md` is `done` or `dropped`.
2. `make ci` is green on `main`.
3. `make test-integration` is green against real qBittorrent and aria2 containers.
4. `make e2e` is green, including the accessibility and installability gates.
5. The yt-dlp pin in `Dockerfile` is at most seven days old.
6. `docs/` carries no `[NEEDS CLARIFICATION]` marker: `make doclint` proves it.
7. Tag `v1.0.0` and push the tag; the `release` workflow publishes and signs both images.
8. Verify the published image before announcing it:

    cosign verify ghcr.io/l-k-m/dl-tool:1.0.0 \
      --certificate-identity-regexp '^https://github\.com/L-K-M/dl-tool/\.github/workflows/release\.yml@refs/tags/v' \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

9. `docker buildx imagetools inspect ghcr.io/l-k-m/dl-tool:1.0.0` lists `linux/amd64` and `linux/arm64`.
10. A clean `docker compose up -d` from the published tag reaches the first-run wizard.
```

<!-- UNVERIFIED: doc 10 §10 records that the cosign action version and flag names were not covered by the
     research corpus, and the create-pull-request action is not in the corpus either. Confirm both against
     their own documentation and pin them by commit SHA before merging. -->

## Steps
1. Create `.github/workflows/release.yml` from doc 10 §10, keeping `platforms: linux/amd64,linux/arm64`,
   `provenance: mode=max`, `sbom: true`, the registry build cache and the keyless `cosign sign --yes` step.
2. Keep the matrix to the single `dl-tool` entry and leave a comment saying T115 adds `dl-tool-aria2`.
3. Keep the `permissions` block exactly as documented: `contents: read`, `packages: write`,
   `id-token: write` for keyless signing, `attestations: write`.
4. Pin every third-party action by commit SHA, with the version in a trailing comment; only
   `actions/checkout` and the `docker/*` actions may keep the majors doc 10 §10 verified.
5. Create `.github/workflows/ytdlp-bump.yml` as above. It opens a pull request and never pushes to `main`,
   never auto-merges, and never publishes an image.
6. Create `.github/copilot-instructions.md` as a short mirror of `AGENTS.md`: where the plan lives, how to
   pick a task, the seven hard rules of [`docs/14-conventions.md` §7](../14-conventions.md#7-repository-wide-hard-rules).
7. Edit `CONTRIBUTING.md` to add the `Releasing` section above, placed after "Changing the plan itself".
8. Confirm the release workflow does not run `make docker-build`: the buildx action is the only builder, so
   the `docker-container` driver is used and the attestations attach to an image index.
9. Run the local multi-arch dry run in the Verification block and confirm both platforms build with
   provenance and SBOM enabled.

## Acceptance criteria
- [ ] The release workflow builds `linux/amd64` and `linux/arm64` from one tag.
- [ ] `provenance: mode=max` and `sbom: true` are both set on the build step.
- [ ] The cosign step is keyless and runs only when the event is not a pull request.
- [ ] Every third-party action is pinned by commit SHA.
- [ ] The weekly job opens a pull request and never merges or publishes anything.
- [ ] The weekly job rewrites exactly the three `ARG` lines and nothing else in `Dockerfile`.
- [ ] `CONTRIBUTING.md` carries the ten-step v1.0.0 checklist including the `cosign verify` command.

## Verification
Run exactly this. Paste the output under "Evidence".
```bash
make doclint && docker buildx build --platform linux/amd64,linux/arm64 --provenance=mode=max --sbom=true --output=type=cacheonly -f Dockerfile . && echo RELEASE_DRYRUN_OK
```
Expected: `doclint` prints nothing; buildx reports both `linux/amd64` and `linux/arm64` stages completing
and no error; the final line of stdout is exactly `RELEASE_DRYRUN_OK`.

Also confirm scope:
```bash
git status --porcelain=v1 -uall -- . ':(exclude)docs' | awk '{print $NF}' | sort
```
Expected: exactly the paths in the Files table, in that order, and nothing else. Use `git status`, not
`git diff`: a file this task creates is untracked, and `git diff --name-only` never lists an untracked file.

## Out of scope — do NOT
- Do NOT edit `.github/workflows/ci.yml` or `docs-lint.yml`; T002 owns the CI workflows, the OpenAPI drift gate and the doc-lint job.
- Do NOT add the `dl-tool-aria2` matrix entry; T115 owns it.
- Do NOT set the yt-dlp pin values by hand here; T093 wrote the first pin and T113 refreshes it.
- Do NOT enable auto-merge on the weekly pull request: a human reviews every yt-dlp bump.
- Do NOT add a runtime update check to the application; there is no phone-home, by default or otherwise.

## Forbidden shortcuts
- Do NOT skip/xfail a test, weaken an assertion, or delete a test to make a check pass.
- Do NOT add `//nolint`, `// nolint`, or `_ = err` to silence a linter; fix the cause.
- Do NOT edit files outside the Files table. If you believe you must, STOP and write why under "Blocked".

## Evidence
<Agent pastes command output here before marking done.>

## Blocked
<Only if you had to stop. State the exact ambiguity and which file should answer it.>

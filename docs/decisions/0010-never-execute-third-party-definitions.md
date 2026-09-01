# 0010 - Never execute third-party definition code

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

Download Station's search feature is a `.dlm` tarball whose `search.php` is **arbitrary PHP**: the official
DLM guide documents `prepare($curl, $query)` and `parse($plugin, $response)` as its two mandatory methods,
and the only isolation Synology documents is "Minimal Privilege — Download Station search module is run
using `nobody` privilege" plus "Restricted File Access" — no namespaces, no seccomp, no egress restriction.
qBittorrent's equivalent is a nova3 `.py` plugin installed **by URL** from its `search-plugins` repository,
under a wiki that says "Install only from sources you trust, and review the script before installing".
dl-tool must decide whether "add a search source" means "add data" or "add an interpreter".

## Decision Drivers

- Adding a search source is a routine, unprivileged user action; a design where it can also mean remote code
  execution is wrong at the interface level, not merely in code.
- Everything a definition does is build a URL, fetch it, and pull fields out of RSS, JSON or HTML. All three
  are expressible as data. The `.dlm` corpus is also small and largely dead — eight collections found, most
  targeting defunct sites — so an interpreter buys compatibility with almost nothing.
- The implementing agent is a weaker model. A sandbox must be correct on the first attempt; a parser that
  only reads data has no equivalent failure mode. Even data needs limits: `yaml.load()` reached RCE
  (CVE-2020-14343) and a YAML alias bomb reached unauthenticated DoS in Kubernetes (CVE-2019-11253).

## Considered Options

- **Option A** — Execute definitions as Download Station does: a PHP interpreter, uid `nobody`, path allow-list.
- **Option B** — Declarative only: a Torznab client plus `dlsearch/v1` YAML; `.dlm` and `.py` are converted, never run.
- **Option C** — Embed a deterministic sandbox now: Starlark (`go.starlark.net`) or CEL, no I/O, step-budgeted.
- **Option D** — Implement Cardigann v11 in full, so Jackett and Prowlarr definitions run unmodified.

## Decision Outcome

Chosen option: **Option B, declarative only**, because it removes the class instead of mitigating it: with
no interpreter in the image there is no sandbox to get wrong, and importing a hostile `.dlm` becomes a
parsing problem rather than a privilege problem. Option C is the v2 answer if the declarative format ever
proves insufficient — Starlark, never a general-purpose interpreter.

Definitions are treated as hostile data: `gopkg.in/yaml.v3` with `KnownFields(true)`, a size cap before
parsing, depth, node and alias caps after it, JSON-Schema validation in which an unknown key is an error,
`http`/`https` only, every fetch through the SSRF-guarded client, Go's linear-time RE2 `regexp` for every
supplied pattern, and a per-definition deadline. Limits: [`../12-security-and-threat-model.md`](../12-security-and-threat-model.md).

### Consequences

- Good, because the runtime image carries no PHP, Python, Perl, Ruby or Lua
  ([ADR-0011](0011-alpine-runtime-with-puid-pgid.md)): the mitigation is verified by inspecting the image.
- Good, because it settles three neighbouring questions identically: no qBittorrent search plugin is
  tolerated at engine conformance, `/custom-cont-init.d` and `DOCKER_MODS` are not implemented, and no "run
  external program on completion" feature exists — the one that turned CVE-2023-30801 and CVE-2023-34237
  into remote code execution.
- Bad, because a `.dlm` doing anything beyond building one URL cannot be converted; it imports as
  metadata-only and **disabled**, with its PHP shown read-only for the user to port by hand. The 547–553
  Cardigann definitions do not run either — accepted, since that corpus is overwhelmingly piracy trackers.
- Neutral, because the image does ship a JavaScript runtime (`nodejs`) — yt-dlp's `yt-dlp-ejs` dependency,
  invoked only by the pinned binary ([ADR-0018](0018-pin-ytdlp-by-version-and-hash.md)); no dl-tool code
  path feeds a definition to it.

### Confirmation

No interpreter reaches the image and no definition path shells out:

```bash
docker run --rm --entrypoint sh ghcr.io/l-k-m/dl-tool:dev -c 'command -v php python python3 perl ruby lua'
grep -rn "os/exec" --include='*.go' internal/search internal/rss
```

Expected: both print nothing and exit non-zero. The import path is then proven against real fixtures by
[NFR-020](../02-requirements.md#nfr-020-execute-no-third-party-code) with `make test PKG=./internal/search/...`.

## Pros and Cons of the Options

### Option A - execute definitions in a Download-Station-style sandbox

- Good, because it is the only option with real `.dlm` compatibility: existing login and pagination logic
  keeps working unchanged.
- Bad, because uid `nobody` plus a path allow-list is not a sandbox: the module keeps full outbound network
  access, an SSRF primitive against the LAN, and adding a source becomes an admin-only act.

### Option B - declarative only, static conversion for imports

- Good, because the format is the sandbox: there is nothing to escape from, and one tested Torznab client
  already covers Prowlarr, Jackett, NZBHydra2 and bitmagnet.
- Bad, because sites needing JavaScript, a captcha or a cookie login are unsupported.

### Option C - Starlark or CEL sandbox now

- Good, because Starlark is deterministic, has no imports or I/O and is step-limited by design, which makes
  it a defensible sandbox rather than a hopeful one.
- Bad, because it is a second authoring language with no evidence yet that the declarative format falls
  short, and it reintroduces a per-import review burden that nobody discharges.

### Option D - full Cardigann v11 compatibility

- Good, because roughly 550 maintained definitions would work and users could read Jackett's documentation.
- Bad, because v11 is captcha blocks, FlareSolverr hints, cookie login, 25 filters, `:has()` selectors and a
  `download.before` chain — a multi-month build plus an annual schema-bump treadmill, over a GPLv2-derived
  corpus that is over 500 piracy trackers deep.

## More Information

- Research: `indexers.md` §4.3, §4.4, §4.7, §6, §7.4; `security.md` §4 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../07-search-and-indexers.md`](../07-search-and-indexers.md),
  [`../08-rss-automation.md`](../08-rss-automation.md) and
  [`../12-security-and-threat-model.md`](../12-security-and-threat-model.md).
- Enables [ADR-0008](0008-torznab-first-declarative-yaml-second.md); constrains [ADR-0009](0009-native-cross-protocol-rss-rules.md).

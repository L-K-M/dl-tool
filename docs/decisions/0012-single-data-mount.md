# 0012 - A single /data mount

> **Status:** accepted
> **Date:** 2026-09-01
> **Deciders:** repository owner

## Context and Problem Statement

dl-tool downloads into one directory and then moves the result into another, and the engine containers write
the same files. Each bind mount is a separate mount point inside a container's mount namespace, so `st_dev`
differs between `/downloads/x` and `/media/x` even when both are one filesystem on the host. `link(2)` then
returns `EXDEV` and so does `rename(2)`, which turns every completion into a copy plus delete. The Servarr
guide states the outcome directly: passing in `/tv`, `/movies` and `/downloads` "makes them look like two
different file systems, even if they are a single file system outside the container. This means hard links
won't work *and* instead of an instant/atomic move, a slower and more IO intensive copy+delete is used."
Download Station teaches users a `/downloads` share, so the wrong layout is also the familiar one. dl-tool
must pick a container path convention and hold every service to it.

## Decision Drivers

- A completed 40 GB task must finish in milliseconds, not ten minutes of NAS disk I/O, and must not need
  40 GB of free space to move a file already on the disk.
- The failure is silent: nothing errors, downloads just get slow and the disk fills with duplicates, so the
  design has to prevent it rather than report it.
- `st_dev` alone does not settle it: exFAT, CIFS, NFS and FUSE unions such as Unraid's `/mnt/user` can report
  one device and still reject `link(2)`.
- Every path dl-tool stores is also interpreted by a different container. A path that means one thing to
  dl-tool and another to qBittorrent is a class of bug a weaker model cannot debug from a log line.

## Considered Options

- **Option A** — One `/data` bind mount, at the identical container path in every service.
- **Option B** — Download Station's shape: `/downloads` for in-progress work and a separate library mount.
- **Option C** — One host tree, but each service keeps its own container path, with a path-mapping table in
  dl-tool translating between them — the \*arr "remote path mappings" model.
- **Option D** — Named Docker volumes instead of bind mounts, one volume shared by every service.

## Decision Outcome

Chosen option: **Option A, a single `/data` mount at the identical path everywhere**, because it is the only
option under which `rename(2)` succeeds and hardlinks exist, and the only one where a stored path string
means the same thing in every container. `compose.yaml` therefore maps one host directory to `/data` in
`dl-tool`, `qbittorrent` and `aria2`, using the TRaSH `torrents/`, `usenet/`, `media/` tree.

Two mandatory mechanisms back it up, both specified in
[`../10-deployment-and-compose.md`](../10-deployment-and-compose.md): a boot self-check that compares
`st_dev` **and** then really creates, links and unlinks a probe file per root, recording
`hardlinks_available`; and `internal/fsx/move.go` as the only code allowed to relocate a completed file,
trying `os.Rename` first and falling back to copy, `fsync`, rename-into-place and unlink on `EXDEV` alone.

### Consequences

- Good, because completion is an atomic `rename(2)`: a task never occupies twice its size, and hardlinks
  work, so a library copy and a seeding copy share blocks and removing one leaves the other intact.
- Good, because `tasks.destination` is verbatim usable as an engine save path, with no translation layer.
- Bad, because operators with an existing split layout must restructure, and NAS users must expose the parent
  of both directories as one share. A legacy `/downloads`-only mount is accepted with a visible warning.
- Bad, because a separate fast disk for in-progress files reintroduces the cross-device boundary. v1 refuses
  that trade: the self-check reports it and the move helper handles it, but the default is one filesystem.
- Neutral, because `usenet/` is created and left empty; v1 has no NZB engine and a v2 lane should not need a
  re-layout.

### Confirmation

The compose file must resolve, and the filesystem behaviour is asserted rather than assumed:

```bash
make compose-check && make test PKG=./internal/fsx/...
curl -s localhost:8091/api/v1/system/info | jq '[.roots[].hardlinks_available] | all'
```

Expected: exit 0 from both `make` targets, including the `EXDEV` fallback test, and `true` from a stack
started with the shipped `compose.yaml`. A `false` is the self-check reporting a misconfigured host.

## Pros and Cons of the Options

### Option A - one /data mount, identical path everywhere

- Good, because it is one rule with no exceptions, and it matches what the surrounding ecosystem already
  documents, so a NAS user who has read the TRaSH guides recognises the layout.
- Bad, because it constrains where the operator may put things: everything dl-tool touches lives under one
  root, and the `DLTOOL_DATA_ROOTS` list exists for genuinely separate pools, not for splitting one workflow.

### Option B - separate /downloads and library mounts

- Good, because it mirrors Download Station and the historical linuxserver.io convention, so it is what an
  arriving user expects to type.
- Bad, because it is precisely the configuration the Servarr guide names as broken: no hardlinks, and every
  move degraded to copy plus delete.

### Option C - per-service paths with a mapping table

- Good, because each container keeps whatever path its own documentation uses, and the \*arr stack proves the
  pattern works in practice.
- Bad, because the mapping table is state that can be wrong, and when it is wrong the symptom is a task that
  completes into a directory nobody can find.

### Option D - named volumes

- Good, because Docker manages ownership and the operator never types a host path.
- Bad, because the data is invisible to the NAS file manager, to SMB and to the NAS's own backup product, and
  it lands on the small system volume rather than the large pool.

## More Information

- Research: `deployment.md` §3.1–§3.4, §11.3, §11.5 — summarised in
  [`../16-prior-art-and-research.md`](../16-prior-art-and-research.md).
- Depends on this decision: [`../10-deployment-and-compose.md`](../10-deployment-and-compose.md) and
  [`../12-security-and-threat-model.md`](../12-security-and-threat-model.md), which jails paths to the roots.
- The entrypoint that never recursively chowns this mount: [ADR-0011](0011-alpine-runtime-with-puid-pgid.md).

# aria2 testdata

## `aria2_tellstatus_1.37.0.json`

One recorded `aria2.tellStatus` JSON-RPC response: an HTTP(S) download mid-transfer (`status:
"active"`, one connection, one selected file). `errorCode`, `errorMessage`, `infoHash`,
`numSeeders`, `seeder`, `followedBy`, `verifiedLength` and `verifyIntegrityPending` are legitimately
absent: the first two exist only on stopped/completed downloads, the rest are BitTorrent-only or
conditionally absent (docs/06-download-engines.md §4.4).

- Captured: 2026-09-04, from a live aria2 **1.37.0** daemon (musl static build of the upstream
  `release-1.37.0` source, asset `aria2-x86_64-linux-musl_static.zip` from
  github.com/abcfy2/aria2-static-build — no Docker or aria2 package was available on the capturing
  machine; `aria2c --version` reported `aria2 version 1.37.0`).
- Subject: `https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.16.tar.xz`, throttled with
  `max-download-limit=1M` so the capture landed while the download was active.

Capture commands:

```sh
aria2c --enable-rpc --rpc-listen-port=6800 --dir=<DLDIR> --daemon=true
GID=$(curl -s http://127.0.0.1:6800/jsonrpc -d '{"jsonrpc":"2.0","id":"add","method":"aria2.addUri","params":[["https://cdn.kernel.org/pub/linux/kernel/v6.x/linux-6.16.tar.xz"],{"max-download-limit":"1M"}]}')
curl -s http://127.0.0.1:6800/jsonrpc -d "{\"jsonrpc\":\"2.0\",\"id\":\"rec\",\"method\":\"aria2.tellStatus\",\"params\":[$GID,[\"gid\",\"status\",\"totalLength\",\"completedLength\",\"uploadLength\",\"downloadSpeed\",\"uploadSpeed\",\"dir\",\"files\",\"errorCode\",\"errorMessage\",\"infoHash\",\"numSeeders\",\"seeder\",\"connections\",\"followedBy\",\"verifiedLength\",\"verifyIntegrityPending\"]]}" > aria2_tellstatus_1.37.0.json
```

Redaction per docs/13-testing-and-verification.md §5: the local absolute path prefix in `dir` and
`files[].path` was replaced with `/data`. Everything else is byte-for-byte what the daemon emitted,
including the `\/` solidus escaping.

Expected output lives in `aria2_tellstatus_1.37.0.golden.json`; regenerate with
`go test ./internal/engine/aria2/... -update`.

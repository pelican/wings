# RFC 0001 — Wings Plugin Ecosystem

- Status: **Proposed**
- Author: Alexandru Vlad
- Date: 2026-11-14

## Summary

A data-driven **capability-provider** system for Wings. Functionality that would
otherwise require forking core (event integrations, extra API routes, scheduled
jobs, later incremental backups and lifecycle hooks) becomes a **provider** behind
a small typed contract. Providers are either **native** (compiled into Wings) or
**external** (a subprocess in any language speaking JSON-RPC over stdio).

## Motivation

Wings has **no plugin seam**. Backup dispatch is a compile-time switch on the
adapter in `router/router_server_backup.go`, and every hosting company that
needs something extra — an event feed to its own monitoring, a node endpoint
for its billing, a periodic cleanup job — carries it as a patch to router,
server and cmd code. Each of those patches conflicts on every upstream merge,
which is a large part of why forks drift away from Wings. This RFC builds the
seam once so that new capabilities — and third-party contributions — plug in
without core surgery, and a fork's own features live in its own packages.

## Design

### One contract, many transports

A **capability** is a typed Go interface (`EventSink` today, later
`BackupProvider`). Providers register against a capability; the host dispatches
to the enabled providers for it. The caller never knows whether the provider is
native or external.

```
router / server ──► plugin.Registry ──► EventSink
                                         ├─ native   (in-proc Go)
                                         └─ external  (subprocess, JSON-RPC/stdio)
```

Transports are pluggable behind the contract. v1 ships **native** and **stdio
JSON-RPC**; `go-plugin`/gRPC and **WASM** are future transports that require no
contract change.

### Registration: manifest-based autoregister

External plugins ship a `plugin.yaml` **manifest** (name, capability, entrypoint,
provides, `requires.hostAPI`, version, minWings). Wings
discovers plugins by scanning the plugins directory and reading manifests
**without executing them**. `config.yml` carries only **policy** (directory,
autoload, enabled/disabled lists, grants) — never per-plugin plumbing. Native
providers self-register in code via `init()`.

Security rule — **discover ≠ execute**: every trust decision (run it? what may it
access?) is made pre-spawn from the manifest. Plugin processes are spawned
lazily, on first use.

### Phased trust (the load-bearing decision)

Trust is **phased**: native first-party providers are compiled in, while
external plugins require operator approval before loading. The constant across
both is a single mediated boundary — every plugin→host call goes through
`HostHandler`, **deny-by-default**, where granted = `manifest.requires ∩
policy`. Opening a public marketplace later means adding stronger sandbox
transport and user-approved grant subsets, not re-architecting the boundary.

### Transport choice: stdio JSON-RPC

For a heterogeneous control plane (events, host callbacks, later hooks) JSON-RPC over
stdio gives maximum flexibility: transport-agnostic, polyglot, no IDL/codegen,
bidirectional (host callbacks + event notifications), evolvable via an
`initialize` handshake. The host owns lifecycle, security, and — critically —
bulk data never flows through the RPC (see Backups). `go-plugin`/gRPC is reserved
for cases that need typed streaming or bundled mTLS across many remote plugins;
WASM for sandboxing untrusted logic.

## Wire protocol

JSON-RPC 2.0, newline-delimited, over the plugin's stdio. Full spec in
[`plugin/sdk/SPEC.md`](../../plugin/sdk/SPEC.md). Lifecycle: `initialize` →
capability traffic (`event` notifications) → stdin close = exit. Host callbacks (`host.*`) and
notifications (`event`, later `progress`) share the connection.

## Milestones

| Phase | Contents | Status |
|-------|----------|--------|
| **M1** | contract, registry, manifest discovery, policy config | **Done** |
| **M1.5** | `wings plugins list/get/enable/disable/doctor`, `Disabled` blocklist policy | **Done** |
| **M2** | `plugin/jsonrpc` bidirectional transport, `plugin/external` lazy-spawn supervisor, `plugin/sdk` + SPEC, Bun/TS reference plugin | **Done** |
| **M3** | `EventSink` capability + `plugin.Emit` (wired to server state, stats and install), `HostAPI` + grant-gated host callbacks, native `RegisterRoutes` / `RegisterTask` | **Done** |
| **M4** | `BackupProvider` contract + **restic** provider (bundled binary, `exec --json`); requires Panel snapshot-chain model | **Deferred** |
| **M5** | content-bound trust, archive signing, registry install, updater | **Done** |
| **M6** | per-plugin server scoping of host calls and events, subprocess sandboxing (unprivileged uid, seccomp/namespaces), WASM transport | Next |

## Backups (M4) — why deferred, and the shape

Incremental backup is the motivating "hard" capability, but it is **not
Wings-only**. Restic snapshot chains break every Panel assumption baked into the
current contract: single checksum, single `{uuid}.tar.gz` artifact, single size,
one-row-per-UUID (`SetBackupStatus`), single-stream `io.Reader` restore, single
`download_url`, and a `RestoreCallback` with no delete/tombstone channel. The
generate path also runs against the **live** filesystem with no quiesce.

Decision: adopt **restic** (pure-Go, `CGO_ENABLED=0` static binary, best `--json`
control plane, widest backends) as a **bundled subprocess**; the data plane is
restic-owned (it reads the server dir and writes the repo), and only control +
progress flow over the plugin boundary. Kopia is the fallback if in-process
embedding is ever mandated; rustic (Rust/FFI) would break the static build.
Deferred until the Panel-side chain model + a `Server.Backup` quiesce hook land.

## Security

- Plugins run as a subprocess; unprivileged-uid drop + seccomp/namespaces are a
  follow-up before untrusted use.
- Host callbacks are deny-by-default and grant-scoped, and return projections:
  never the Panel client, raw server configuration or container inspect.
- Host calls and events are **not yet scoped per server**: a plugin granted a
  method can use it on any server on the node (M6).
- Archive signing is enforced when signatures are present or required by policy;
  external plugins also require operator trust approval before loading. Sandbox
  transport remains a marketplace follow-up.

## Testing

`plugin/...`, `cmd` and `router` pass with `-race` (registry + enable policy,
manifest discovery/validation, JSON-RPC round-trip, string and number ids,
error propagation, bidirectional host callback, close-safety, host grant gate,
event fan-out, trust fingerprint, signing, install limits, and plugin route
scopes inheriting the router's auth).

## Alternatives considered

- **Go `plugin` (.so)** — same-version/dep lock, no isolation, no Windows.
  Rejected.
- **hashicorp/go-plugin (gRPC)** — heavier (proto + handshake + mTLS), weak SDK
  for non-Go authors; overkill for a small control plane. Kept as a future
  transport.
- **Interpreted Go in-process (e.g. Yaegi)** — no build step and readable
  source, but the plugin shares the daemon's address space: an infinite loop,
  a large allocation or a panic in a goroutine the plugin starts reaches Wings
  itself, and plugins are limited to Go. Not chosen as the only transport; it
  could sit beside stdio as another transport behind the same contract.
- **WASM (wazero/Extism) now** — right for untrusted sandboxed *logic*, but
  can't wrap libraries that need sockets or the host. Scheduled for M6.

## Next

The restic backup provider (M4) and per-server scoping of host calls and events
(M6) are the next pieces.

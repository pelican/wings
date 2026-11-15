# Wings Plugin HostAPI (v1 — design)

The HostAPI is the surface Wings exposes to plugins so a plugin can do what
Wings itself does — power servers, touch the filesystem, tail consoles, make
backups, react to events, schedule work. The goal is **permissive to
creativity**; the guardrail is that **every method is a named grant**, so the
surface can be full today (first-party) and scoped later (marketplace) without
re-architecting.

Grounded in an enumeration of the real Wings surface; each method lists the
backing symbol it wraps.

## Model

- **Deny-by-default dispatch.** Inbound plugin calls are `host.<grantKey>`
  routed through `plugin/host.go:HostHandler`. A method runs only if its
  `grantKey` is in the plugin's granted set; unknown/ungranted → JSON-RPC
  `-32601` / `-32001`. The tables below are the full design; what is wired today
  is listed in "Implemented today".
- **Grants** = manifest `requires.hostAPI` **∩** operator policy
  (`GrantsFor`/`intersect`). First-party default = grant everything requested
  (the operator approves that request when trusting the plugin), narrowed by
  `plugins.grants.<name>` when set. Full surface either way.
- **Wire.** Request/response via `conn.Call`; **streaming** (console, stats,
  install output, backup progress) via `conn.Notify` — the host pushes
  notifications to the plugin, same channel as `events`.
- **Versioned.** `initialize` negotiates a `hostApiVersion`; additive only.

## Implemented today

These grant keys are registered in `cmd/pluginhost*.go` (asserted by
`TestHostMethodsRegistered`). Everything else in the tables below is design.

| domain | grant keys |
|---|---|
| server (read) | `server.list`, `server.get`, `server.status`, `server.state`, `server.stats`, `server.allocations`, `server.logs`, `resolveAddress` |
| server (mutating) | `server.power`, `server.command` |
| fs (read) | `fs.list`, `fs.read` (≤ 1 MiB, base64), `fs.diskUsage` |
| fs (mutating) | `fs.write` (≤ 1 MiB, base64), `fs.mkdir`, `fs.delete` (refuses the root), `fs.rename`, `fs.copy`, `fs.chmod` (Unix mode, e.g. `0o2750`) |

The fs methods apply the server's file denylist the same way the HTTP file API
does: reads, writes, copies and chmods of a denylisted path are refused, renames
check both ends, and a recursive delete leaves denylisted entries in place.
| events | `events.publish` |

Known gaps against the rules below: **rule 4 is not enforced yet**. Scoped
methods accept any server UUID on the node and `server.list` returns every
server, so granting a plugin a method grants it for all servers. Events are
likewise delivered node-wide. Per-plugin server scoping is the next step before
untrusted plugins. `fs.search` is not wired because Wings' search lives in the
HTTP handler (`router/router_server_files_search.go`) rather than on
`Filesystem`.

## Hard security rules (non-negotiable)

These come straight from the surface map — a HostAPI method that breaks one is a
tenant/secret/node compromise:

1. **Never hand a plugin the `remote.Client`** — it carries the node↔Panel Bearer
   token and node-wide authority (`remote/http.go:41`). No passthrough. Panel
   ops (`GetServers`, `GetServerConfiguration`, `ValidateSftpCredentials`, all
   `Set*Status`) stay off the surface.
2. **Never return raw `Configuration`** — `EnvVars` are per-tenant secrets
   (`server/configuration.go:73`; `ToAPIResponse` embeds it). Project to
   non-secret fields.
3. **Never return raw `ContainerInspect`** — `Config.Env` / mounts / host paths
   leak secrets + layout. Curated subset only. Never expose the process-wide
   docker client singleton.
4. **Per-server scoping is mandatory.** `Manager.All/Keys` and `Registry.Emit`
   are node-wide today → a single-server plugin must never see other tenants.
   Every method binds to the plugin's **granted server set**, exactly as
   `ResolveAddress` constrains via `manager.Get`.
5. **Filesystem = server-relative only.** All paths resolve through the
   server's `Filesystem` (an `os.Root` over its data directory). Never accept
   host-absolute paths or hand a plugin the root itself.
6. **`fs.pull` is the SSRF surface** — host validates the URL/target, never the
   plugin. **Backup UUIDs** must pass `validateIdentifier` (path-traversal guard).
7. **`events.publish` / `activity.log`** must be namespaced and reject reserved
   topics/event keys, and force the server scope — a plugin must not spoof
   `StatusEvent`/console lines or forge `server:power.*` activity.

## Domains

Method name = `host.<grantKey>`. `M` = mutating, `S` = streaming.

### server (runtime) — `server/*.go`, `environment/*`

| grantKey | M | S | backing | notes |
|---|---|---|---|---|
| `server.power` | ✓ | | `power.go:HandlePowerAction` | start/stop/restart/**kill**. kill=SIGKILL data-loss. gate + audit; drop `kill` for untrusted |
| `server.command` | ✓ | | `environment.SendCommand` | arbitrary console stdin = in-game command exec. audit; allowlist for untrusted |
| `server.console.tail` | | | `Server.ReadLogfile` | last N lines. output may contain secrets |
| `server.console.stream` | | ✓ | `listeners.processConsoleOutputEvent` → LogSink | high-volume; honor console throttle + backpressure |
| `server.state` / `.subscribe` | | ·/✓ | `IsRunning`/`Environment.State`, `OnStateChange` | already fanned to listeners as `server.state` |
| `server.stats` / `.subscribe` | | ·/✓ | `resources.go:Proc` / `StatsEvent` | resource sample; rate-limit polling |
| `server.status` | | | `IsInstalling/IsTransferring/IsRestoring/IsSuspended/...` | lifecycle predicate aggregate; safe |
| `server.install` | ✓ | (✓) | `install.go:Install` | runs egg install script in privileged container over the volume |
| `server.reinstall` | ✓ | (✓) | `install.go:Reinstall` | force-stop + re-run install over populated data. **highest risk**; never untrusted |

### fs — `server/filesystem/*` (all server-relative, `os.Root`-contained)

| grantKey | M | S | backing | notes |
|---|---|---|---|---|
| `fs.list` | | | `Filesystem.ListDirectory` | Stat[]; large-dir response is the cap |
| `fs.read` | | ✓ | `Filesystem.File`/read | stream bytes, don't marshal whole file |
| `fs.write` | ✓ | ✓ | `Filesystem.Write` | quota-checked (`HasSpaceFor`) + chown to server user |
| `fs.mkdir` | ✓ | | `CreateDirectory` | |
| `fs.delete` | ✓ | | `Delete` | **destructive**; deny delete-root/truncate |
| `fs.rename` | ✓ | | `Rename` | |
| `fs.copy` | ✓ | | `Copy` | quota-checked |
| `fs.chmod` | ✓ | | `Chmod` | |
| `fs.compress` / `fs.decompress` | ✓ | (✓) | `CompressFiles`/`DecompressFile` | quota + overflow-guarded |
| `fs.pull` | ✓ | ✓ | `Pull` | **SSRF surface** — host validates target |
| `fs.diskUsage` | | | `DiskUsage`/`CachedUsage` | |
| `fs.search` | | | needs a `Filesystem` search (today in `router_server_files_search.go`) | resource-bounded, capped results |

Omit or high-trust-gate: `fs.symlink`, delete-root/`TruncateRootDirectory`,
`SetDiskLimit`, and the quota-skipping `WriteFile`(deprecated)/`extractStreamUnsafe`.

### backup — `server/backup/*`

| grantKey | M | S | backing | notes |
|---|---|---|---|---|
| `backup.create` | ✓ | ✓ | `Server.Backup` → `Generate` | adapter `wings`/`s3`; disk-fill for local; progress on event bus |
| `backup.restore` | ✓ | ✓ | `Server.RestoreBackup` → `Restore` | **destructive**: suspends+stops server, overwrites files |
| `backup.delete` | ✓ | | `Remove` | **irreversible** `os.Remove` |
| `backup.read` (details/checksum/size/locate) | | | `Details`/`Checksum`/`Size`/`LocateLocal` | checksum is **sha1** (stale doc says sha256) |
| `backup.list` | | | **GAP** — no index | Wings keeps no backup index; needs scandir (local) or Panel proxy |

Coupling: create/restore call `remote.Client` (`SetBackupStatus`,
`GetBackupRemoteUploadURLs`, `SendRestorationStatus`). Exposing cleanly needs the
BackupProvider refactor, which the RFC defers — this domain lands with it, not
before.

### platform — manager / env / events / network / activity

| grantKey | M | S | backing | notes |
|---|---|---|---|---|
| `server.list` | | | `manager.All`/`Keys` | **scope to granted servers**; project `{uuid,name,state,suspended}` |
| `server.get` | | | `manager.Get` | project only — **strip `EnvVars`** |
| `server.allocations` | | | `Allocations` | prefer default mapping; full set reveals host IPs |
| `resolveAddress` | | | `Allocations.DefaultMapping` | `ip:port` of the default allocation |
| `env.stats` | | | `Proc`→`environment.Stats` | safe once scoped |
| `env.inspect` | | | `ContainerInspect` | **project** `{id,running,startedAt,exitCode,oomKilled,image}` — never raw |
| `env.readLog` | | | `Readlog` | may leak secrets; cap lines |
| `events.subscribe` | | ✓ | `EventListener`/`Registry.Emit` | **add per-server filter** (node-wide today) |
| `events.publish` | ✓ | | `Bus.Publish` | always published on the `plugin` topic with `{topic, data}` as the payload, so a plugin cannot use a Wings topic; websocket clients receive a `plugin` event |
| `activity.log` | ✓ | | `SaveActivity` | force server scope; namespace key; never touch `remote.Client` |
| `env.updateLimits` | ✓ | | `InSituUpdate` | read limits from server config, **not** plugin |
| `env.imagePull` | ✓ | ✓ | `ensureImageExists` | **avoid** or restrict to egg image (registry-cred abuse) |
| `panel.*` | — | | `remote.Client` | **DENY** — never mapped |

## Grant tiers (default policy)

- **Safe-by-default (read/projected):** `server.list`, `server.get`,
  `server.status`, `server.state`, `server.stats`, `server.allocations`,
  `resolveAddress`, `env.stats`, `env.inspect`, `env.readLog`,
  `server.console.tail`, `fs.list`, `fs.read`, `fs.diskUsage`, `fs.search`,
  `events.subscribe`.
- **Gated (mutating, operator grant):** `server.power`, `server.command`,
  `fs.write/mkdir/delete/rename/copy/chmod/compress/decompress/pull`,
  `events.publish`, `activity.log`, `env.updateLimits`, `backup.*`.
- **Avoid / never untrusted:** `server.reinstall`, `env.imagePull`,
  `fs.symlink`/root-truncate, any `panel.*`.

## Implementation notes

- **One bridge package** `plugin/host/` (or grow `plugin/host.go`): a
  `HostAPI` with grouped methods + a dispatcher that extends `HostHandler`'s
  switch. Each method: resolve server via `manager.Get` (scope check),
  project/validate, call the backing symbol.
- **Projection helpers** to strip secrets (`Configuration`→public,
  `ContainerJSON`→public) — shared, tested.
- **Streaming**: a `host.subscribe`-style call that registers the plugin for a
  topic; host pushes `conn.Notify`. Reuse the event delivery path.
- **Per-server scope**: a `grantedServers(plugin)` gate every method calls first.
- **Backup domain waits on the deferred backup provider** (remote.Client coupling).

## Rollout order

1. **Read/projected core** — `server.list/get/status/state/stats`, `env.*`,
   `console.tail`, `resolveAddress`. Low risk, immediately useful, exercises the
   scoping + projection helpers.
2. **fs read** — `fs.list/read/search/diskUsage`.
3. **Streaming** — `console.stream`, `state/stats.subscribe`.
4. **Mutating (gated)** — `server.power/command`, `fs.write/...`,
   `events.publish`, `activity.log`.
5. **Backups** — `backup.*` alongside the restic provider, once the RFC's
   deferred backup work lands.

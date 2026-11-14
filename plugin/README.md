# Wings Plugin Ecosystem — developer guide

This package lets Wings functionality be **pluggable** instead of hardcoded. A
plugin can react to server events, expose HTTP routes, run scheduled jobs, and
call back into Wings — either compiled in (**native**) or as a separate process
in any language (**external**).

The motivating case is forks and hosts that carry their own features: today each
one is a patch to router, server and cmd code that has to be re-applied on every
upstream merge. With this package a feature lives in its own Go package (or its
own executable), and the only shared line is one import in `plugin/builtin`.

If you read one thing, read the two-layer model below.

## The mental model: two layers

```
PLUGIN SYSTEM  — the machinery (this package)
  ├─ registry          providers by capability + enable policy  registry.go
  ├─ transports        native (in-proc) | external subprocess   external/, jsonrpc/
  ├─ config/manifests  which plugins are on, what they may do    config policy + manifest.go
  ├─ HostAPI           what a plugin may ask Wings to do          host.go + cmd/pluginhost*.go
  └─ hooks             Register{EventSink,Routes,Task}            events.go, route.go, task.go

CAPABILITIES  — what plugins actually DO (ride on the machinery)
  ├─ EventSink      subscribe to server lifecycle events (state, stats, install)
  └─ ...            new capabilities are a Go interface + a constant; nothing
                    else in the machinery changes
```

The **machinery** is the real deliverable. A capability is a typed Go
interface; the registry, manifest discovery, trust and transports are shared by
every capability.

## Where things live

| path | what |
|------|------|
| `plugin/capability.go` | the `Capability` type |
| `plugin/registry.go` | the registry: `RegisterEventSink`, `Emit`, enable policy |
| `plugin/events.go` | `EventSink` contract + `Event` (the event/stream bus) |
| `plugin/host.go` | `HostAPI` + `HostHandler` (deny-by-default host callbacks) + grants |
| `plugin/manifest.go` | external plugin `plugin.yaml` discovery (read, never executed) |
| `plugin/route.go` | `RegisterRoutes` — add HTTP endpoints |
| `plugin/task.go` | `RegisterTask` — add scheduled jobs |
| `plugin/jsonrpc/` | bidirectional JSON-RPC 2.0 over stdio (the external wire) |
| `plugin/external/` | spawns + supervises external plugin processes |
| `plugin/sdk/` | Go SDK for plugin authors + `SPEC.md` (wire), `HOSTAPI.md` (host surface) |
| `plugin/builtin/` | blank-imports native plugins so they register |
| `cmd/pluginhost*.go` | the concrete `HostAPI` implementation (server/fs/events methods) |
| `cmd/plugins.go` | the `wings plugins list/get/enable/disable/doctor/add/update/trust` CLI |

## Native vs external

- **Native** — Go code compiled into Wings, registered from an `init()` and
  linked by one blank import in `plugin/builtin`. Fastest; trusted because it is
  part of the binary.
- **External** — a separate executable in any language that ships a
  `plugin.yaml` manifest and speaks JSON-RPC over stdio. Discovered from the
  plugins directory, approved by the operator, spawned lazily on first use.
  (e.g. `examples/plugins/state-log`, Bun + TypeScript.)

Both satisfy the *same* contract; the rest of Wings can't tell them apart.

## The three ways to extend (native) — all the same shape

Every native extension is `init()` → `Register…()` → Wings mounts it at startup.

### 1. Event sinks

```go
func init() {
    plugin.RegisterEventSink(auditLog{})
}
// auditLog implements: Name() string; Events() []string; OnEvent(ctx, plugin.Event) error
```

### 2. HTTP routes

```go
func init() {
    plugin.RegisterRoutes("healthz", plugin.Route{
        Scope:   plugin.ScopePublic,        // public | node | server
        Method:  http.MethodGet,
        Path:    "/health",                 // -> GET /ext/healthz/health
        Handler: func(c *gin.Context) { c.JSON(200, gin.H{"ok": true}) },
    })
}
```

Scopes map onto the router's existing auth: `public` is mounted at
`/ext/<plugin>/…` with no auth, `node` at `/api/ext/<plugin>/…` behind the node
token, and `server` at `/api/servers/:server/ext/<plugin>/…` behind the node
token plus the server-exists check.

### 3. Scheduled tasks

```go
func init() {
    plugin.RegisterTask("reaper", plugin.Task{
        Name:     "reap",
        Interval: time.Minute,
        Run:      func(ctx context.Context) error { /* ... */ return nil },
    })
}
```

Then add `_ "example.com/you/wings-plugins/reaper"` to `plugin/builtin`.

## End-to-end: how an event flows

```
container state change
  │
  ▼  server/listeners.go       (s.OnStateChange as before)
plugin.Emit({type: "server.state", server, data})     registry.go
  │
  ▼  registry: every enabled sink subscribed to "server.state"
EventSink ─► native sink        (in-process, own goroutine)
          ─► external plugin    (JSON-RPC "event" notification over stdio)
```

Delivery is asynchronous and best-effort: a slow or failing plugin never stalls
the listener that produced the event. Each sink has a queue of 256 events,
delivered in order; a sink that falls further behind loses events instead of
making Wings hold them.

## Configuration

Central policy lives in `config.yml` (operator-owned, tiny):

```yaml
plugins:
  directory: /var/lib/pelican/plugins   # where external plugins live
  autoload: true                        # discovered plugins run unless disabled
  disabled: []                          # exceptions
  grants: {}                            # per-plugin host-capability overrides
```

The `plugins:` block is read only from the file: a configuration push from the
Panel (`POST /api/update`) keeps the node's existing plugin policy, because the
plugins directory also holds the operator's trust approvals.

Per-plugin settings live **inside the plugin's own directory** (Wings never
parses them). See `sdk/SPEC.md`.

External plugins describe themselves in `plugin.yaml`:

```yaml
apiVersion: v1
name: state-log
capability: events
version: 1.0.0                   # authoring metadata (author/description/
author: example                  # license/source optional but expected for
license: MIT                     # anything distributed)
minWings: 1.0.0                  # minimum Wings version
entrypoint: ["bun", "index.ts"]
provides: { events: ["server.state"] }
requires: { hostAPI: ["server.logs"] }   # host capabilities it asks for
```

Discovery reads manifests **without executing them** — every trust decision
(run it? what may it access?) happens before the process is spawned.

## HostAPI — what a plugin can ask Wings to do

A plugin calls back into Wings over the same connection, deny-by-default:
`granted = manifest.requires.hostAPI ∩ operator policy`. Methods are
`host.<grantKey>` (e.g. `host.server.stats`, `host.fs.read`, `host.server.power`).
Full list + security rules in `sdk/HOSTAPI.md`. Implementation:
`cmd/pluginhost*.go` (a registry of small typed handlers).

## Security in one breath

- **Discover ≠ execute** — manifests are read without running the plugin.
- **Deny-by-default host calls** — a plugin only gets the capabilities it was granted.
- **Operator trust** — external plugins must be approved, and approval is bound
  to their manifest scope plus installed content.
- **Host owns the dangerous bits** — route auth and server lookup run in Wings
  before a plugin sees anything; host methods return projections, never raw
  configuration or the Panel client.
- **Not yet:** per-server scoping of host calls and events (a granted plugin
  sees every server on the node), and process sandboxing (unprivileged uid,
  seccomp/namespaces). See `sdk/HOSTAPI.md`.

## Operate it

```
wings plugins list             # native + discovered external, with state
wings plugins doctor           # why a plugin is / isn't enabled
wings plugins disable X        # persist to config.yml
wings plugins update X         # check + install a newer version (--check to only check)
wings plugins trust X          # review and approve an installed external plugin
wings plugins untrust X        # revoke approval so the plugin will not load
```

## Updating external plugins

An external plugin can carry an `update:` URL in its manifest pointing at an
update manifest:

```json
{ "version": "1.2.0", "download": "https://.../state-log-1.2.0.tar.gz",
  "sha256": "<hex>", "signature": "<base64>" }
```

`wings plugins update <name>` fetches it (HTTPS only, including every redirect; credentials are dropped if a redirect leaves the registry host), compares versions,
downloads the archive, **verifies the sha256 and signature policy**, extracts it
(path-traversal guarded) into a staging dir next to the plugin, asks the
operator to approve the staged manifest, then atomically swaps it in (keeping a
`.bak`, restored on failure). Restart Wings to load the new version.

## Installing from a registry

A registry is a static `registry.json` served over HTTPS (à la a shadcn
registry) — no central server. Configure namespaces in `config.yml`; a private
(company) registry carries auth:

```yaml
plugins:
  registries:
    community:
      url: https://plugins.example.com/registry.json
    acme:                                  # private company registry
      url: https://registry.acme.com/registry.json
      token: ${ACME_REGISTRY_TOKEN}        # -> Authorization: Bearer …, env-expanded
      # headers: { X-Api-Key: "${ACME_KEY}" }
```

`${VAR}` is expanded from the environment, so tokens stay out of `config.yml`.
The auth is sent to both the registry index fetch and the archive download.

```
wings plugins add @community/state-log                   # by namespace
wings plugins add https://host/registry.json#state-log   # by explicit URL
wings plugins add state-log                              # search all registries
```

A bare name must be listed in exactly one configured registry, and every
registry must be readable; otherwise the command asks for `@namespace/name`.
That way a public registry can't take over a name your private registry uses.

`registry.json`:

```json
{
  "name": "community-plugins",
  "plugins": [
    { "name": "state-log", "version": "1.2.0", "capability": "events",
      "download": "https://.../state-log-1.2.0.tar.gz", "sha256": "<hex>" }
  ]
}
```

Install reuses the staged update path: HTTPS fetch, sha256 verify, **signature
check**, zip-slip-guarded extract, manifest identity validation, operator trust
approval, and atomic install.

## Signing

Installing/updating runs code you didn't write, so archives can be signed
(Ed25519) and verified against a trusted-key store before install.

```yaml
plugins:
  require_signatures: true                  # reject unsigned/unverified installs
  trusted_keys:
    - "example:BASE64_ED25519_PUBKEY"        # id:key (id optional)
```

Publisher workflow:

```
wings plugins keygen                         # -> wings-plugin.key + a public key line
wings plugins sign state-log-1.2.0.tar.gz    # -> base64 signature
```

Put the signature in the registry/update entry and give the operator the public
key for `trusted_keys`:

```json
{ "name": "state-log", "version": "1.2.0",
  "download": "https://.../state-log-1.2.0.tar.gz",
  "sha256": "<hex>", "signature": "<base64>" }
```

Policy: unsigned + not required → allowed; unsigned + `require_signatures` →
rejected; a present signature must verify against a trusted key regardless
(a signature that fails means tampering). The archive is signed over its sha256
digest, so a tampered archive fails even if its sha256 matches a forged manifest.

## Next reading

- `sdk/SPEC.md` — the external wire protocol (write a plugin in any language).
- `sdk/HOSTAPI.md` — the full host surface a plugin can call.
- `examples/plugins/state-log/` — a real external plugin (Bun + TypeScript).

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
PLUGIN SYSTEM  — the machinery
  ├─ registry          providers by capability + enable policy  registry.go
  ├─ transports        native (in-proc) | external subprocess   external/, jsonrpc/
  ├─ config/manifests  which plugins are on, what they may do    config policy + manifest.go, trust.go
  ├─ HostAPI           what a plugin may ask Wings to do          host.go + cmd/pluginhost*.go
  ├─ hooks             Register{EventListener,Routes,Task}       registry.go, route.go, task.go
  └─ distribution      registries, download, verify, install     install/

CAPABILITIES  — what plugins actually DO (ride on the machinery)
  ├─ EventListener      subscribe to server lifecycle events (state, stats, install)
  └─ ...            new capabilities are a Go interface + a constant; nothing
                    else in the machinery changes
```

The **machinery** is the real deliverable. A capability is a typed Go
interface; the registry, manifest discovery, trust and transports are shared by
every capability.

## Where things live

The plugin system is a set of small modules inside the one Wings binary (a
modular monolith). Each has one job and a short list of what it may import.

| module | what | may import (from Wings) |
|--------|------|-------------------------|
| `plugin/contract` | what a plugin implements: `Event`, `EventListener`, `Capability` | nothing; standard library only |
| `plugin/jsonrpc` | bidirectional JSON-RPC 2.0 over stdio (the external wire) | nothing; standard library only |
| `plugin/sdk` | Go SDK for external plugins + `SPEC.md` (wire), `HOSTAPI.md` (host surface) | `contract`, `jsonrpc` |
| `plugin` | the runtime: registry and `Emit` (`registry.go`), enable policy, `RegisterRoutes` (`route.go`), `RegisterTask` (`task.go`), grant check on host calls (`host.go`), manifests (`manifest.go`), trust store (`trust.go`), inventory for the CLI; re-exports the `contract` types | `config`, `contract`, `jsonrpc` |
| `plugin/install` | registries, download, sha256 and signature checks, extraction, staged install with rollback | `config`, `plugin` |
| `plugin/external` | spawns and supervises external plugin processes | `config`, `plugin`, `jsonrpc` |
| `plugin/builtin` | blank-imports native plugins so they register | the native plugins |
| `cmd/pluginhost*.go` | the concrete `HostAPI` (server/fs/events methods); the only place plugin code meets `server` | |
| `cmd/plugins.go` | the `wings plugins list/get/enable/disable/doctor/add/update/trust` CLI | |

`plugin/boundaries_test.go` enforces the last column (with `config`'s own
dependency, `system`): it fails if, say, `plugin/sdk` starts importing
`plugin`, which would put Wings, gin and the Docker client into every Go
plugin binary. A Go plugin built with the SDK depends on about 70 packages and
builds to about 3 MB.

## Native vs external

- **Native** — Go code compiled into Wings, registered from an `init()` and
  linked by one blank import in `plugin/builtin`. Fastest; it runs inside Wings
  with all of Wings' access, so it is trusted as much as the Wings build it is
  part of. (e.g. `examples/plugins/go-native-auditlog`.)
- **External** — a separate executable in any language that ships a
  `plugin.yaml` manifest and speaks JSON-RPC over stdio. Discovered from the
  plugins directory, approved by the operator, spawned lazily on first use.
  (e.g. `examples/plugins/state-log` in Bun + TypeScript,
  `go-external-statecount` in Go, `python-install-log` in Python.)

Both satisfy the *same* contract; the rest of Wings can't tell them apart.

## The three ways to extend (native) — all the same shape

Every native extension is `init()` → `Register…()` → Wings mounts it at startup.

### 1. Event listeners

```go
func init() {
    plugin.RegisterEventListener(auditLog{})
}
// auditLog implements: Name() string; Events() []string; OnEvent(ctx, plugin.Event) error
// (plugin.Event is contract.Event; native plugins can use either name)
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
  ▼  registry: every enabled listener subscribed to "server.state"
EventListener ─► native listener        (in-process, own goroutine)
          ─► external plugin    (JSON-RPC "event" notification over stdio)
```

Delivery is asynchronous and best-effort: a slow or failing plugin never stalls
the listener that produced the event. Each listener has a queue of 256 events,
delivered in order; a listener that falls further behind loses events instead of
making Wings hold them.

## Configuration

Central policy lives in `config.yml` (operator-owned, tiny):

```yaml
plugins:
  directory: /var/lib/pelican/plugins   # where external plugins live
  autoload: true                        # discovered plugins run unless disabled
  disabled: []                          # exceptions
  grants: {}                            # per-plugin host-capability overrides
  run_as_root: false                    # see "Threat model": plugins run as system.user
```

The `plugins:` block is read only from the file: a configuration push from the
Panel (`POST /api/update`) keeps the node's existing plugin policy, because the
plugins directory also holds the operator's trust approvals.

Per-plugin settings and state live in the plugin's data directory,
`<plugins directory>/.data/<name>/`, passed to the plugin as `WINGS_PLUGIN_DATA`
(Wings never parses them). It is owned by the user the plugin runs as (mode
0700), and it is the only place the plugin can write. Updates leave it alone and
it is not part of the trust approval, unlike the plugin's own directory, which is
read-only to the plugin. See `sdk/SPEC.md`.

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
(run it? what may it ask Wings for?) happens before the process is spawned.

## HostAPI — what a plugin can ask Wings to do

A plugin calls back into Wings over the same connection, deny-by-default:
`granted = manifest.requires.hostAPI ∩ operator policy`. Grants limit what a
plugin can ask Wings, not what its process can do on the machine (see
"Threat model"). Methods are
`host.<grantKey>` (e.g. `host.server.stats`, `host.fs.read`, `host.server.power`).
Full list + security rules in `sdk/HOSTAPI.md`. Implementation:
`cmd/pluginhost*.go` (a registry of small typed handlers).

## Security in one breath

- **Discover ≠ execute** — manifests are read without running the plugin.
- **Deny-by-default host calls** — a plugin can only call the host methods it was
  granted. That limits what it can ask Wings, not what its process can do.
- **Operator approval** — external plugins must be approved, and approval is bound
  to their manifest scope plus installed content. Approving is a decision to
  trust the plugin's author; Wings does not review the code.
- **Not root** — when Wings runs as root, external plugins run as `system.user`.
- **Host owns the dangerous bits** — route auth and server lookup run in Wings
  before a plugin sees anything; host methods return projections, never raw
  configuration or the Panel client.
- **Not yet:** per-server scoping of host calls and events (a granted plugin
  sees every server on the node), and sandboxing beyond the OS user
  (seccomp/namespaces). See `sdk/HOSTAPI.md`.

## Threat model

A plugin is code the operator chooses to run on the node. Wings can't make
arbitrary code safe or readable. What it does is make the operator's decision
informed, make it stick, and limit the damage when trust is misplaced.

**Guaranteed:**

- Nothing runs until it is approved (`wings plugins add`, `update` or `trust`).
- What runs is exactly what was approved: the approval covers a hash of the
  plugin's files and of its manifest's entrypoint, events and host access. A
  plugin whose files changed is not loaded at Wings' next start until it is
  approved again.
- Host access is deny by default: a plugin can call only the host methods it
  requested and was granted.
- Downloads are https only, checked against their sha256, and against their
  signature when one is offered or required; registry credentials only go to
  the registry's own origin.

**Not guaranteed, and said so at every approval:**

- That the code is safe, or readable. Wings does not review plugin code, and a
  plugin may ship compiled programs. The approval prompt says whether the
  plugin is **source** (every file is valid UTF-8 text with no NUL bytes) or
  **binary**, and lists its binary files.
- That host access bounds the plugin. It limits what the plugin can ask Wings;
  the process itself has the access of the OS user it runs as. By default that
  is `system.user`, which owns every server's files under `system.data`: a
  plugin can read and change them, and use the network, without any host grant.

Every approval ends with: "This plugin runs on this node as <user>, with that
user's access to the machine. The host access listed only limits what it can ask
Wings."

**Process user.** When Wings runs as root, external plugins run as
`system.user` (`pelican` by default), never as root, with no supplementary
groups and `HOME` set to their data directory. The plugin's own directory is
read-only to that user: Wings installs plugin files as root with mode 0755, and
refuses to start a plugin when that user owns, or can write, any of its files or
any directory above them. The data directory is owned by that user; Wings gives
it (and files an earlier version left owned by root) to that user before each
start, skipping symlinks and hard-linked files. The operator can opt out with
`plugins.run_as_root: true` in `config.yml` (a Panel config push can't set it).
When Wings itself is not root (rootless, or a developer machine), plugins run as
that same user. Native plugins run inside Wings, as Wings' user.

Limits to keep in mind:

- All external plugins run as the same user, so each can read the others' data
  directories.
- A plugin's process gets only `PATH`, locale, `TZ`, TLS certificate and proxy
  variables from Wings' environment (`plugin/sdk/SPEC.md`, Lifecycle), so
  `WINGS_TOKEN` and registry tokens referenced as `${NAME}` stay with Wings. A
  proxy URL with a password in it does reach the plugin.
- Signing proves who published an archive, not that it is safe.

**Responsibilities.** Wings: the guarantees and the disclosure above, safe
defaults, and honest documentation. The node operator: what to install, which
registries and keys to trust. The plugin author: an honest manifest, published
source, signed releases. A registry (and the Pelican Hub): review, signing, and
taking down bad releases.

**Not yet:** sandboxing beyond the OS user (seccomp, namespaces), host access
scoped per server, registry-driven revocation, and a source-only policy.

## Operate it

```
wings plugins list             # native + discovered external, with state
wings plugins doctor           # why a plugin is / isn't enabled
wings plugins disable X        # persist to config.yml
wings plugins update X         # check + install a newer version (--check to only check)
wings plugins trust X          # show what an installed external plugin is, and approve it
wings plugins untrust X        # revoke approval so the plugin will not load
```

## Updating external plugins

An external plugin can carry an `update:` URL in its manifest pointing at an
update manifest:

```json
{ "version": "1.2.0", "download": "https://.../state-log-1.2.0.tar.gz",
  "sha256": "<hex>", "signature": "<base64>" }
```

The Pelican Hub's existing per-plugin manifest is read too, so a plugin
published on the Hub can use `update: https://hub.pelican.dev/plugins/<slug>/update.json`
as is: the release under a `"*"` key, its URL in `download_url`, and `[]` when
there is no release.

`wings plugins update <name>` fetches it (HTTPS only, including every redirect; credentials are dropped if a redirect leaves the registry host), compares versions,
downloads the archive, **verifies the sha256 and signature policy**, extracts it
(path-traversal guarded; symlinks must resolve inside the plugin, hard links
are refused) into a staging dir next to the plugin, asks the
operator to approve the staged manifest, then atomically swaps it in. The old
version waits in a hidden `.wings-backup-*` directory until the approval is
saved, and is put back if anything fails. Restart Wings to load the new version.

## Installing from a registry

A registry is a `registry.json` served over HTTPS (à la a shadcn registry):
a static file on any host, or generated by a site such as the Pelican Hub.
There is no required central server. Configure namespaces in `config.yml`; a
private (company) registry carries auth:

```yaml
plugins:
  registries:
    pelican:                               # the Pelican Hub (planned, see below)
      url: https://hub.pelican.dev/wings/registry.json
    acme:                                  # private company registry
      url: https://registry.acme.com/registry.json
      token: ${ACME_REGISTRY_TOKEN}        # -> Authorization: Bearer …, env-expanded
      # headers: { X-Api-Key: "${ACME_KEY}" }
```

`${VAR}` is expanded from the environment, so tokens stay out of `config.yml`.
The auth is sent to both the registry index fetch and the archive download.

```
wings plugins add @pelican/state-log                     # by namespace
wings plugins add https://host/registry.json#state-log   # by explicit URL
wings plugins add state-log                              # search all registries
```

A bare name must be listed in exactly one configured registry, and every
registry must be readable; otherwise the command asks for `@namespace/name`.
That way a public registry can't take over a name your private registry uses.

`registry.json`:

```json
{
  "name": "pelican",
  "plugins": [
    { "name": "state-log", "version": "1.2.0", "capability": "events",
      "author": "Pelican", "description": "Log server state changes",
      "download": "https://hub.pelican.dev/plugins/state-log/download/42/state-log.zip",
      "sha256": "<hex>" }
  ]
}
```

Archives may be `.tar.gz` or `.zip`; the format is read from the content.

**The Pelican Hub does not serve `/wings/registry.json` yet.** It already has
everything an entry needs for its Panel plugins: the slug, the latest approved
version, the author, the description, a public download route and the
package's sha256. Serving Wings plugins needs a way to mark a plugin as a
Wings plugin and the route itself; see
[the RFC](../docs/rfc/0001-plugin-ecosystem.md#the-pelican-hub-as-the-first-registry).
Until then, `pelican` above is an example; a registry that can't be read makes
a bare-name `add` fail, so don't add it to a node's config before it is live.

Install reuses the staged update path: HTTPS fetch, sha256 verify, **signature
check**, zip-slip-guarded extract, manifest identity validation, operator
approval, and atomic install.

An archive has `plugin.yaml` either at its root, or inside a single top-level
directory with nothing else next to it (how GitHub release archives and the
Pelican Hub package plugins); that directory's contents become the plugin. Any
other layout is refused.

## Signing

Installing/updating runs code you didn't write, so archives can be signed
(Ed25519) and verified against a trusted-key store before install. A signature
proves who published the archive, not that its code is safe.

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
- `examples/plugins/` — four small working plugins, each with a README:
  - `python-install-log/` — external, Python standard library only: appends
    installs to a log file. Ships as source.
  - `state-log/` — external, Bun + TypeScript, no SDK: calls back into Wings
    with `host.server.logs`. Ships as source (or, optionally, compiled).
  - `go-external-statecount/` — external, Go with the SDK: counts state changes
    and keeps them in the plugin's data directory. Ships compiled, so operators
    see it as a binary plugin.
  - `go-native-auditlog/` — native (Go, compiled into Wings): an event
    listener, a node route and a scheduled task.

  `plugin/external/examples_test.go` starts the three external ones the way
  Wings does and checks each one reacts to an event (skipped if its runtime is
  not installed).

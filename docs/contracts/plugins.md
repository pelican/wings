# Contract: plugins across Wings and Panel

**Contract v3.1, 2026-11-15.** Source of truth for plugin interfaces shared by pelican/wings and pelican/panel.
Owner: Alexandru Vlad (Scai). Sections: R (registry format), L (Panel access levels), A (Wings plugin HTTP
API for the Panel).

Work against this contract, as of v3.1:
- pelican/wings PR 251 (stacked on PR 250), branch `scai/plugin-panel-api`: sections R (Wings side), L, A.
- pelican/panel PR 2785, branch `scai/plugin-registries`: section R for Panel plugins.
- pelican/panel PR 2786, branch `scai/wings-plugins-contract` (builds on pelican/panel PR 2701): sections L, A.

## Change rule

Change this contract only through the owner. If something in it doesn't work, say so in your report under
"Contract issues" and use the safest option meanwhile. Additive changes (a new optional field) bump the minor
note in the version history; anything that changes or removes a field, status code or behaviour is a new
version and needs the owner first.

## Safety (for every agent working against this contract)

Do not create secrets, keys, DNS records or registries; do not tag, release or deploy; do not touch live
nodes, servers or production data; do not merge; do not comment on other people's PRs or issues, without the
owner's explicit yes. Never print secrets. Prepare code, tests, a draft PR and an owner checklist.

## R. Registry format (shared by Wings and Panel)

A registry is one JSON document served over **https**. Anyone can host one: a static file (GitHub Pages, S3,
a company server) or a site that generates it (the Pelican Hub, planned at
`https://hub.pelican.dev/wings/registry.json`). Wings implements this today in
`plugin/install/catalog.go`; the Panel implements the same rules for Panel plugins.

### R1. Document

```json
{
  "name": "pelican",
  "plugins": [
    {
      "name": "state-log",
      "version": "1.2.0",
      "target": "wings",
      "capability": "events",
      "author": "Pelican",
      "description": "Log server state changes",
      "download": "https://hub.pelican.dev/plugins/state-log/download/42/state-log.zip",
      "sha256": "9f2c…64 hex chars",
      "signature": "base64…",
      "minWings": "1.0.0"
    },
    {
      "name": "player-counter",
      "version": "2.0.1",
      "target": "panel",
      "author": "Example",
      "description": "Show player counts",
      "download": "https://plugins.example.com/player-counter-2.0.1.zip",
      "sha256": "…",
      "panel_version": "^1.0"
    }
  ]
}
```

| Field | Type | Required | Meaning |
|---|---|---|---|
| `name` (document) | string | no | Display name of the registry. |
| `plugins` | array | yes | Entries. An empty array is valid. Never `null`. |
| `name` | string | yes | Plugin identifier. Must equal the `name` (Wings `plugin.yaml`) or `id` (Panel `plugin.json`) inside the archive; install fails otherwise. Must not be empty, start with `.`, contain `/`, `\` or `..`. For Wings plugins, `available` is reserved (the path of `GET /api/plugins/available`) and refused. |
| `version` | string | yes | Version of the archive at `download`. Compared numerically per dot-separated part, leading `v` ignored (`1.10.0` > `1.9.2`). |
| `target` | `"wings"` \| `"panel"` | no | Which side installs this entry. **Missing means `"wings"`** (registries written before v1). Each side ignores entries for the other side, and entries with a `target` it doesn't know, including for bare-name lookups (R3). |
| `capability` | string | no | Wings only (`"events"` today). When present it must match the archive's manifest. |
| `author`, `description` | string | no | Shown to the admin before install. |
| `download` | string | yes | https URL of the archive; the format is read from the content, not the name. Wings accepts `.tar.gz` and `.zip`. **The Panel accepts `.zip` only**, so `target: "panel"` entries must be zips. |
| `sha256` | string | yes | Lowercase hex SHA-256 of the archive bytes. Install fails on mismatch. |
| `signature` | string | no | Base64 Ed25519 signature over the **32 raw bytes** of the archive's SHA-256 digest. Present but not a non-empty string (e.g. `""`, `null`, a number) is an invalid entry, never "unsigned". |
| `minWings` | string | no | Wings only. Minimum Wings version. |
| `panel_version` | string | no | Panel only. Same meaning as `panel_version` in the Panel's `plugin.json`. |

Unknown fields are ignored by both sides (forward compatible). Maximum document size: 1 MiB.

### R2. Fetching and downloading

1. https only, on every hop. A redirect to a non-https URL is refused. At most 10 redirects.
2. Credentials: a registry may carry a bearer token (sent as `Authorization: Bearer <token>`) and extra
   headers. They are sent **only** to the registry's own origin (same scheme, host and port): to fetch the
   document, and to download an archive at that same origin. They are dropped on any request or redirect to
   another origin, and for the rest of a redirect chain once it has left the origin.
3. Credential values may reference environment variables (`${NAME}`) so secrets stay out of config files.
4. The archive is verified against `sha256` before anything is extracted; extraction refuses absolute paths,
   `..`, links leaving the plugin directory and hard links, and caps total size and entry count. Size caps
   are enforced while bytes arrive (a response without `Content-Length` must not be read whole first).
5. Signatures: if `signature` is present it must verify against one of the admin's trusted keys (base64
   Ed25519 public keys, optionally `id:key`); a signature that fails is always an error. If it is absent, the
   install proceeds unless the admin requires signatures. "Require signatures" covers **every** install path
   on that side: registries, update manifests, the Pelican Hub, URL imports and uploaded archives. Wherever a
   source offers a signature (registry entry, update manifest, Hub release), it is carried through to the
   check.
6. Default trusted keys: none today. **Once the Pelican Hub signs its releases, its public key ships as a
   default trusted key on both sides** (owner decision, 2026-11-15), so Hub releases verify without per-node
   setup. Operators can remove it. Until then a Hub `signature` would fail on every node, so the Hub must not
   start signing before Wings and the Panel ship the key.

### R3. Choosing a registry

Registries are configured by the admin under a namespace (`pelican`, `acme`, …). A plugin is requested as:

- `@namespace/name`: that registry only, with its credentials.
- `https://host/registry.json#name`: that URL only, without credentials.
- `name`: every configured registry. It succeeds only if **exactly one** registry lists it (for this side's
  `target`) **and every registry could be read**. Otherwise it asks for `@namespace/name`, so a public
  registry can't take over a name a private one uses.

Each install still shows the admin what the plugin is and asks for approval; a registry can never push code.

### R4. Update manifests

A plugin can name an update URL (Wings: `update:` in `plugin.yaml`; Panel: `update_url` in `plugin.json`).
Both sides accept both shapes:

- Plain: `{"version": "1.2.0", "download": "https://…", "sha256": "…", "signature": "…"}`
- Pelican Hub (`/plugins/{slug}/update.json` today): `{"*": {"version": "…", "download_url": "https://…",
  "channel": "stable", "sha256": "…"}}`, and `[]` when there is no release. `download_url` is a synonym for
  `download`.

## L. Panel access levels (Wings)

How much the Panel may do with **Wings** plugins on a node. Set by the node operator in the node's
`config.yml` as `plugins.panel_access`, or with the `WINGS_PLUGIN_PANEL_ACCESS` environment variable (which
wins). A Panel config push can never change it. An unknown value means `off`.

| Level | Panel may | Default |
|---|---|---|
| `off` | nothing; every plugin endpoint answers 403 | |
| `view` | list and inspect plugins; enable, disable and change settings of plugins the node already trusts | yes |
| `signed` | also install, update and uninstall from the node's configured registries when the archive's signature verifies against the node's trusted keys (even if the node doesn't otherwise require signatures) | |
| `full` | also install from any URL or an uploaded archive, signed or not, unless the node itself has `require_signatures` on | |

At every level the Panel cannot change the node's registries, trusted keys, signature requirement or grants.
Ways to set it: `wings configure --plugin-access <level>` (the Panel's auto-deploy command can add this flag
for new nodes), `wings plugins panel-access <level>` on an existing node, or the environment variable.

## A. Wings plugin HTTP API for the Panel

Implemented in Wings by `router/router_plugins.go`; `plugin/API.md` has full examples. If that document and
this section disagree, this section wins and the document is a bug.

### A1. Transport

- Every endpoint is under the node's API with the node token (`Authorization: Bearer <token>`), like
  `/api/system`. Request and response bodies are JSON (except the multipart upload in A5).
- `{id}` is the plugin's name. Both sides must refuse an empty or invalid id (empty, starting with `.`,
  containing `/`, `\` or `..`) before sending or acting: Wings answers 400, and the Panel must not build a
  URL such as `/api/plugins//uninstall`.
- Errors are `{"error": "<message>"}` plus fields named below.

| Status | Meaning |
|---|---|
| 400 | bad id, body or query |
| 403 | the node's access level (L) doesn't allow it; body adds `"panel_access": "<level>"`, `"required": "<level>"` |
| 404 | on `GET /api/plugins`: this node's Wings has no plugin API. On any `/api/plugins/{id}...`: no such plugin or file (JSON `{"error"}`). Clients decide "API missing" from the list call only. |
| 409 | wrong state (enable an untrusted plugin, install an installed one, check updates of an untrusted plugin), or an approval token unknown, used, expired or sent to another endpoint. Body may add `"reason"`: `"token"` (approval token problem), `"destination_changed"` (A4), `"untrusted"` |
| 413 | body or file over its cap |
| 422 | settings that don't validate (body adds `"fields": {key: message}`); a registry, download, checksum, signature or archive that was refused |
| 429 | 16 installs already waiting for approval on this node |
| 501 | load order (A6) |

### A2. Endpoints

| Method and path | Level | Result |
|---|---|---|
| `GET /api/plugins` | view | `{"enabled": true, "directory": string, "panel_access": level, "data": [Plugin], "error"?: string}`. `panel_access` is always present; `enabled` is always `true` (reserved) |
| `GET /api/plugins/available` | signed | `{"data": [Available], "errors": {"<namespace>": "<message>"}}`: the `target: "wings"` entries of every configured registry (A3.2); a registry that couldn't be read is in `errors`, not a failure of the call |
| `GET /api/plugins/{id}/update-check` | view | `{"installed": string, "latest": string\|null, "available": bool, "error"?: string}`: reads the plugin's update URL without staging anything. No update URL (or native): `latest: null`, `available: false`. A failed fetch: 200 with `error` set and `available: false`. An untrusted external plugin: 409 `reason: "untrusted"`, and nothing is fetched |
| `GET /api/plugins/{id}` | view | Plugin |
| `GET /api/plugins/{id}/source` | view | `{"files": [string], "truncated": bool}` (at most 5000) |
| `GET /api/plugins/{id}/source?file=<path>` | view | `{"file", "size", "binary": bool, "content"?}` (at most 1 MiB; no `content` when binary) |
| `POST /api/plugins/{id}/enable`, `/disable` | view | `{"status": "enabled"\|"disabled", "plugin": Plugin}`; enable of an untrusted or broken plugin is 409 |
| `PUT /api/plugins/{id}/settings` | view | body = the values object; `{"status": "saved", "values": {...}}` |
| `POST /api/plugins/{id}/install` | signed | two steps (A4); optional body `{"registry": "<namespace>"}` |
| `POST /api/plugins/{id}/update` | signed | two steps (A4), from the plugin's own update URL; or `{"status": "up_to_date", "installed", "latest"}` |
| `POST /api/plugins/{id}/uninstall?delete=true\|false` | signed | disables it and revokes its approval; `delete=true` also removes its directory. Its data (settings, state) is kept. `{"status": "uninstalled", "deleted": bool}` |
| `POST /api/plugins/import` | full | two steps (A4); body `{"url", "sha256"?, "signature"?}` or multipart field `plugin` (+ optional `signature`), at most 200 MiB |
| `POST /api/plugins/order` | view | 501 (A6) |

A body of `[]` is treated as `{}`.

### A3. Plugin object

| Field | Type | Notes |
|---|---|---|
| `id`, `name` | string | the plugin's name |
| `kind` | `"native"` \| `"external"` | |
| `author`, `version`, `description` | string | |
| `category`, `capability` | string | capability, or `"plugin"` for a native plugin with only routes/tasks |
| `url`, `update_url`, `wings_version` | string | manifest `source`, `update`, `minWings`; **omitted** when empty |
| `api_version` | number | |
| `transport` | string | `"stdio"` for external plugins |
| `events`, `host_access` | [string] | `host_access` = what the manifest requests; the operator may narrow it |
| `entrypoint` | [string] | external plugins only; **omitted** for native plugins |
| `settings` | [Setting] | the schema, A3.1; `[]` when none |
| `meta.status` | `"enabled"` \| `"disabled"` \| `"errored"` | `errored` adds `meta.error`, `meta.status_message`; an untrusted plugin is `disabled` with a `status_message` |
| `meta.trust` | `"native"` \| `"trusted"` \| `"untrusted"` \| `"changed"` | `changed` = approved once, files changed since |
| `meta.values` | object | stored values; secrets as `"********"`; a missing key means its `default` applies |
| `meta.enabled`, `meta.is_trusted` | bool | |
| `meta.enable_reason` | string | e.g. `"autoload"`, `"allowlist"`, `"blocklist"` |
| `meta.can_enable`, `can_disable`, `can_configure`, `can_update`, `can_uninstall` | bool | plugin state combined with the node's access level; the Panel shows actions from these |
| `meta.load_order` | number | always 0 |
| `meta.registered` | object | what the plugin registered (e.g. `{"hooks": [...]}`) |

Every list that is present is an array, never `null`.

**A3.1 Setting:** `{"key", "label"?, "description"?, "type": "string"|"number"|"boolean"|"select",
"options"?: [{"value", "label"}], "default"?, "required": bool, "secret": bool}`. `key` matches
`^[A-Za-z0-9_][A-Za-z0-9_-]*$` (no dots: form libraries read dots as nesting); Wings refuses a manifest whose
schema has other keys. Values are sent as JSON of that type (a number is a JSON number, not a string). `PUT
.../settings` replaces all values: a key left out or `null` is unset; `"********"` for a secret keeps the
stored value. There is no multiline `text` type. A client that meets a type or key it doesn't support shows
that setting read-only and sends its stored value back unchanged.

`can_update` means the plugin has an update URL and the node's level allows updates; whether a newer version
exists comes from `update-check`.

**A3.2 Available:** `{"name", "version", "registry": "<namespace>", "author"?, "description"?,
"capability"?, "has_signature": bool, "installed": string|null}`. `installed` is the installed version, or
`null`. `has_signature` says the entry carries a signature; it is verified only when staging.

### A4. Two-step install, update and import

1. First call: Wings downloads, checks and stages the archive (nothing is installed) and answers **202**:
   `{"status": "approval_required", "action": "install"|"update"|"import", "plugin": {name, version, author,
   description, capability, source, entrypoint, events, settings}, "host_access": [string], "replaces":
   string|null, "signed": bool, "approval_token": string, "expires_at": RFC 3339, "approve_with": {method,
   path, body}}`.
2. The Panel shows the admin `plugin`, `host_access`, `replaces` and `signed`, and asks for approval.
3. Second call to the **same** endpoint with `{"approve": "<approval_token>"}`: Wings installs, records the
   approval in the node's trust store and loads the plugin; **200** `{"status": "installed"|"updated",
   "plugin": Plugin}`.

The token is single-use, valid 10 minutes, tied to the staged files (a change after staging refuses the
approval) and to its endpoint and plugin. A new stage of the same plugin replaces the waiting one. `signed`
is `true` only when the archive's signature actually verified against a trusted key, at any level. Below
`full`, that is required.

At approval Wings re-checks, under the install lock, against the node's **current** state:
- the access level: a stage that needs a level the node no longer has is refused (403);
- the signature policy: the staged archive's signature against the current `trusted_keys` and
  `require_signatures`; if it no longer passes, 422;
- the destination: if the installed plugin changed since staging (installed, removed or another version),
  409 with `reason: "destination_changed"`. Stage again.

An unknown, used or expired token is 409 with `reason: "token"`.

### A5. Changes take effect without a restart

Enable, disable, install, update and uninstall through this API (and the same changes through the `wings
plugins` CLI) apply to the running daemon at once (API) or within about 5 seconds (CLI): events, plugin
routes and tasks follow the enable state, and external plugin processes are started or stopped to match.
Settings are read by the plugin from `settings.json` in its data directory when the file changes.

### A6. No load order

Each external plugin is its own process and every event goes to every subscribed plugin, so there is no
order. `POST /api/plugins/order` answers 501; the Panel should not offer reordering for Wings plugins.

### A7. Panel obligations

- Scope by node: a Panel admin may only list or act on plugins of nodes they can access, and the Panel must
  check the node of every client-supplied `{node}:{id}` key before calling Wings.
- Treat 403 (level) and 404 (no API) per node as "not available on this node", not as a Panel error; show the
  level and how to raise it (L).
- Use the `can_*` flags and the access level to decide which actions to show.
- New nodes: the auto-deploy `wings configure` command may add `--plugin-access <level>` from a Panel-wide
  setting (L). The Panel never changes a node's level any other way.

## Version history

- v1 (2026-11-15): sections R and L. R adds the optional `target` field; Wings must start ignoring entries
  whose `target` is `"panel"`.
- v2 (2026-11-15): section A from the Wings implementation. R: unknown `target` values are ignored too.
  L: `full` still respects the node's own `require_signatures`. Wings now ignores `target: "panel"` entries.
- v3 (2026-11-15, owner approved): R: Panel archives are zip only; credentials go to the same origin
  (scheme, host, port); require-signatures covers every install path, offered signatures are carried
  through, and a malformed `signature` is invalid; size caps apply while streaming. A: `panel_access` always
  present and `enabled` always `true`; 404 meaning split between the list and per-plugin calls; setting keys
  without dots, and unsupported types/keys shown read-only; new `GET /api/plugins/available` and
  `GET /api/plugins/{id}/update-check`; `signed` means actually verified; approval re-checks the current
  signature policy and the destination (403/422/409).
- v3.1 (2026-11-15, owner approved): the Wings plugin name `available` is reserved; `update-check` refuses
  untrusted external plugins (409 `reason: "untrusted"`) and never fetches their URL; 409s may carry
  `reason` (`token`, `destination_changed`, `untrusted`); R2.6 records that the Hub's signing key ships as a
  default trusted key once the Hub signs releases.

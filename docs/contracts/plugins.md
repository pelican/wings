# Contract: plugins across Wings and Panel

**Contract v1, 2026-11-15.** Source of truth for plugin interfaces shared by pelican/wings and pelican/panel.
Owner: Alexandru Vlad (Scai). Sections: R (registry format, settled), L (Panel access levels, settled),
A (Wings plugin HTTP API for the Panel, added in v2).

Work against this contract, as of v1:
- pelican/wings, stacked on pelican/wings PR 250: branch `scai/plugin-panel-api` (node side and HTTP API).
- pelican/panel: plugin registries for Panel plugins (section R).
- pelican/panel: Wings plugin management, building on pelican/panel PR 2701 (sections L and A, from v2).

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
| `name` | string | yes | Plugin identifier. Must equal the `name` (Wings `plugin.yaml`) or `id` (Panel `plugin.json`) inside the archive; install fails otherwise. Must not be empty, start with `.`, contain `/`, `\` or `..`. |
| `version` | string | yes | Version of the archive at `download`. Compared numerically per dot-separated part, leading `v` ignored (`1.10.0` > `1.9.2`). |
| `target` | `"wings"` \| `"panel"` | no | Which side installs this entry. **Missing means `"wings"`** (registries written before v1). Each side ignores entries for the other side, including for bare-name lookups (R3). |
| `capability` | string | no | Wings only (`"events"` today). When present it must match the archive's manifest. |
| `author`, `description` | string | no | Shown to the admin before install. |
| `download` | string | yes | https URL of the archive. `.tar.gz` or `.zip`; the format is read from the content, not the name. |
| `sha256` | string | yes | Lowercase hex SHA-256 of the archive bytes. Install fails on mismatch. |
| `signature` | string | no | Base64 Ed25519 signature over the **32 raw bytes** of the archive's SHA-256 digest. |
| `minWings` | string | no | Wings only. Minimum Wings version. |
| `panel_version` | string | no | Panel only. Same meaning as `panel_version` in the Panel's `plugin.json`. |

Unknown fields are ignored by both sides (forward compatible). Maximum document size: 1 MiB.

### R2. Fetching and downloading

1. https only, on every hop. A redirect to a non-https URL is refused. At most 10 redirects.
2. Credentials: a registry may carry a bearer token (sent as `Authorization: Bearer <token>`) and extra
   headers. They are sent **only** to the registry's own host: to fetch the document, and to download an
   archive whose host is that same host. They are dropped on any request or redirect to another host.
3. Credential values may reference environment variables (`${NAME}`) so secrets stay out of config files.
4. The archive is verified against `sha256` before anything is extracted; extraction refuses absolute paths,
   `..`, links leaving the plugin directory and hard links, and caps total size and entry count.
5. Signatures: if `signature` is present it must verify against one of the admin's trusted keys (base64
   Ed25519 public keys, optionally `id:key`); a signature that fails is always an error. If it is absent, the
   install proceeds unless the admin requires signatures.

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
| `full` | also install from any URL or an uploaded archive, signed or not | |

At every level the Panel cannot change the node's registries, trusted keys, signature requirement or grants.
Ways to set it: `wings configure --plugin-access <level>` (the Panel's auto-deploy command can add this flag
for new nodes), `wings plugins panel-access <level>` on an existing node, or the environment variable.

## A. Wings plugin HTTP API for the Panel

Added in v2, from the Wings implementation on `scai/plugin-panel-api`.

## Version history

- v1 (2026-11-15): sections R and L. R adds the optional `target` field; Wings must start ignoring entries
  whose `target` is `"panel"`.

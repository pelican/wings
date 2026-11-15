# Panel plugin API

The Panel manages the plugins on a node through these endpoints. They sit
with the other node routes: every request needs the node token
(`Authorization: Bearer <token>`), as `/api/system` does. The code is in
[`router/router_plugins.go`](../router/router_plugins.go).

The Panel and `wings plugins` (the CLI) share the same code and locks: the
config file lock for enable and disable, the plugins directory's install
lock for installs, updates and removals, and the trust store's lock for
approvals. A CLI command and a Panel request at the same time don't undo each
other.

## Access levels

The node operator decides how much the Panel may do, with
`plugins.panel_access` in `config.yml` (or `WINGS_PLUGIN_PANEL_ACCESS`, which
wins). A Panel configuration push can't change it. An unknown value counts as
`off`, and so does `WINGS_PLUGIN_PANEL_ACCESS` set to an empty string; only a
missing `plugins.panel_access` means the default, `view`.

| level | the Panel may |
|---|---|
| `off` | nothing; every endpoint answers 403 |
| `view` (default) | list plugins and read their files; enable, disable and configure plugins the node already trusts. No new code reaches the node. |
| `signed` | also list what the registries offer, and install, update and uninstall from the node's configured registries, when the archive is signed by a key in `plugins.trusted_keys`, even if `require_signatures` is off |
| `full` | also install from any https URL or an uploaded archive, signed or not (the node's own `require_signatures` still applies). Panel admins can then run code on the node. |

Registries, trusted keys, `require_signatures` and grants stay CLI-only at
every level.

A request the level does not allow gets:

```http
HTTP/1.1 403 Forbidden

{"error": "plugins.panel_access is \"view\" on this node; this request needs \"signed\" or higher. ...",
 "panel_access": "view", "required": "signed"}
```

## Endpoints

| method and path | level | what it does |
|---|---|---|
| `GET /api/plugins` | view | every plugin, native and external |
| `GET /api/plugins/available` | signed | what the configured registries offer Wings |
| `GET /api/plugins/{id}` | view | one plugin |
| `GET /api/plugins/{id}/update-check` | view | whether the plugin's update URL has a newer version; stages nothing |
| `GET /api/plugins/{id}/source[?file=path]` | view | list the plugin's files, or read one |
| `POST /api/plugins/{id}/enable` | view | enable an installed, trusted (or native) plugin |
| `POST /api/plugins/{id}/disable` | view | disable a plugin |
| `PUT /api/plugins/{id}/settings` | view | replace the plugin's setting values |
| `POST /api/plugins/order` | view | 501: Wings has no load order |
| `POST /api/plugins/{id}/install` | signed | install from a configured registry, in two steps |
| `POST /api/plugins/{id}/update` | signed | update from the plugin's update URL, in two steps |
| `POST /api/plugins/{id}/uninstall?delete=true\|false` | signed | disable, revoke trust, and delete the files if asked |
| `POST /api/plugins/import` | full | install from a URL or an upload, in two steps |

`{id}` is the plugin's name. A name that can't be a plugin directory (empty,
starting with `.`, holding `/`, `\` or `..`) or is reserved (`available`, the
path of the available-plugins endpoint) is a 400; so `/api/plugins//uninstall`
never reaches the plugins directory itself. An unknown name is a 404 with a
JSON `{"error"}` body. Wings refuses to install or discover a plugin named
`available`.

Every change is logged with the action, the plugin and the remote address.

Errors are JSON, `{"error": "..."}`, with these statuses:

| status | when |
|---|---|
| 400 | a bad id, body or query |
| 403 | the access level does not allow it |
| 404 | on `GET /api/plugins`: this Wings has no plugin API. On `/api/plugins/{id}...`: no such plugin, or no such file in `/source`. Decide "API missing" from the list call only. |
| 409 | the plugin is in the wrong state (enable an untrusted plugin, install one that is already installed, check updates for an untrusted plugin), an approval token is unknown, used, expired or sent to another endpoint, or the plugin's directory changed between staging and approval. Some 409s add `"reason"`: `"token"`, `"destination_changed"` or `"untrusted"` |
| 413 | a body, source file or settings file over its cap |
| 422 | settings that don't validate; a registry, download, checksum, signature or archive that was refused |
| 429 | 16 installs are already waiting for approval |
| 501 | `/api/plugins/order` |

### Listing

`GET /api/plugins`:

```json
{
  "enabled": true,
  "directory": "/var/lib/pelican/plugins",
  "panel_access": "signed",
  "data": [ { "id": "state-log", "...": "one plugin, below" } ],
  "error": "only when discovery found a bad manifest; the plugins that loaded are still listed"
}
```

One plugin (also the body of `GET /api/plugins/{id}`):

```json
{
  "id": "state-log",
  "name": "state-log",
  "kind": "external",
  "author": "Pelican",
  "version": "1.2.0",
  "description": "Logs power state changes.",
  "category": "events",
  "capability": "events",
  "url": "https://github.com/example/state-log",
  "update_url": "https://hub.example.test/plugins/state-log/update.json",
  "wings_version": "1.0.0",
  "api_version": 1,
  "transport": "stdio",
  "events": ["server.state"],
  "entrypoint": ["bun", "index.ts"],
  "host_access": ["server.logs"],
  "settings": [
    {"key": "webhook_url", "label": "Webhook URL", "type": "string", "required": true, "secret": true},
    {"key": "level", "type": "select", "options": [{"value": "info", "label": "info"}], "default": "info", "required": false, "secret": false}
  ],
  "meta": {
    "status": "enabled",
    "load_order": 0,
    "registered": {"hooks": ["server.state"]},
    "values": {"webhook_url": "********", "level": "info"},
    "enabled": true,
    "enable_reason": "autoload",
    "trust": "trusted",
    "is_trusted": true,
    "can_enable": false,
    "can_disable": true,
    "can_configure": true,
    "can_update": true,
    "can_uninstall": true
  }
}
```

- `category` is the capability (`plugin` for a native plugin that only adds
  routes or tasks). `url` is the manifest's `source`, `wings_version` its
  `minWings`, `api_version` the number in its `apiVersion`.
- `host_access` is what the manifest requests (`requires.hostAPI`). The
  operator's `plugins.grants` may narrow it.
- `meta.status` is `enabled`, `disabled` or `errored` (then `meta.error` and
  `meta.status_message` say why). An untrusted plugin is `disabled`, with a
  `status_message` saying the operator must approve it.
- `meta.trust` is `native`, `trusted`, `untrusted`, or `changed` (approved
  once, but its files or manifest changed since, so it must be approved
  again).
- `meta.values` are the stored setting values. A key not listed has no value
  and its `default` applies. Secrets show as `********`.
- The `can_*` flags combine the plugin's state with this node's access level.
  `can_update` means the plugin has an update URL and the level allows
  updates; whether a newer version exists comes from `update-check`.
  `load_order` is always 0.
- `panel_access` is always present, and `enabled` is always `true`
  (reserved). `url`, `update_url` and `wings_version` are left out when
  empty, and `entrypoint` for native plugins. Every list that is present is
  an array, never `null`.

### Available plugins

`GET /api/plugins/available` (`signed`) reads every registry in
`plugins.registries`, each with its own credentials, and lists the entries
Wings installs (`target` missing or `"wings"`):

```json
{
  "data": [
    {"name": "state-log", "version": "1.2.0", "registry": "pelican", "author": "Pelican",
     "description": "Log server state changes", "capability": "events",
     "has_signature": true, "installed": null},
    {"name": "audit", "version": "2.0.0", "registry": "acme", "has_signature": false, "installed": "1.4.0"}
  ],
  "errors": {"backup": "plugin: fetching https://backup.example.test/registry.json: 503 Service Unavailable"}
}
```

- `installed` is the installed version, or `null`.
- `has_signature` says the entry carries a signature. It is verified only
  when the plugin is staged, so at `signed` an entry without one can't be
  installed.
- A registry that can't be read is listed under its namespace in `errors`;
  the call still answers 200. A registry URL's user info and query are left
  out of the message. `errors` is `{}` when every registry was read.
- Entries with an invalid name, or a `signature` field that is not a
  non-empty string, are left out: they can't be installed.

### Update check

`GET /api/plugins/{id}/update-check` (`view`) fetches the plugin's update URL
and compares versions, without downloading or staging anything:

```json
{"installed": "1.2.0", "latest": "1.3.0", "available": true}
```

A plugin without an update URL (or a native one) gives `"latest": null,
"available": false`. A fetch that fails is still a 200, with `error` set and
`"available": false`. An external plugin the node doesn't trust is a 409 with
`"reason": "untrusted"`, and its update URL is never fetched; the same goes for
`POST /api/plugins/{id}/update`.

### Source

`GET /api/plugins/{id}/source` lists the plugin's own directory:

```json
{"files": ["plugin.yaml", "index.ts", "lib/util.ts"], "truncated": false}
```

At most 5000 files are listed (`truncated` says if there were more).
`?file=lib/util.ts` returns one file, up to 1 MiB (413 above that):

```json
{"file": "lib/util.ts", "size": 812, "binary": false, "content": "..."}
```

A file that is not valid UTF-8 comes back with `"binary": true` and no
`content`. Everything is read through an `os.Root` opened on the plugin's
directory: `..`, absolute paths and symlinks leading out of it are refused.
Settings live in the data directory, not here, so they never show up.

### Enable and disable

`POST /api/plugins/{id}/enable` and `/disable` take no body. They change
`plugins.enabled` and `plugins.disabled` in `config.yml` the way `wings
plugins enable/disable` does, and apply the new block to the running daemon
at once. Enabling an external plugin the node does not trust, or one with a
manifest error, is a 409. Any listed plugin can be disabled.

```json
{"status": "disabled", "plugin": { "...": "the plugin, as listed" }}
```

### Settings

`PUT /api/plugins/{id}/settings` takes the values object itself:

```json
{"webhook_url": "********", "level": "warn", "retries": 3}
```

- The body replaces the stored values. A key left out, or sent as `null`, is
  unset, so its default applies.
- For a secret setting, `********` keeps the stored value, so a form that
  showed the masked value can send it back unchanged.
- Unknown keys, wrong types (`"3"` for a number), select values that are not
  an option, and a required setting with neither a value nor a default are
  refused with 422 and nothing is written. Values that are each valid but
  together would make `settings.json` larger than 1 MiB are a 413, and the
  stored values are kept:

```json
{"error": "plugin: invalid settings: ...", "fields": {"retries": "setting \"retries\" must be a number"}}
```

- Only installed, trusted external plugins can be configured (409 otherwise).
  The schema comes from `settings:` in the plugin's manifest. Each `key`
  matches `^[A-Za-z0-9_][A-Za-z0-9_-]*$` (no dots: form libraries read them
  as nesting); a manifest with any other key is refused. Types are
  `string`, `number`, `boolean` and `select`, sent as JSON of that type. A
  client that meets a type or key it doesn't support should show that
  setting read-only and send its stored value back unchanged. The values are
  written to `settings.json` in the plugin's data directory, which is where
  the plugin reads them; see [`sdk/SPEC.md`](sdk/SPEC.md#settings).

```json
{"status": "saved", "values": {"webhook_url": "********", "level": "warn", "retries": 3}}
```

### Two-step installs

`install`, `update` and `import` never put code on the node in one call. The
first call downloads and checks the archive, unpacks it into a hidden staging
directory, and answers 202 with what would be installed:

```http
POST /api/plugins/state-log/install
{}

HTTP/1.1 202 Accepted
{
  "status": "approval_required",
  "action": "install",
  "plugin": {"name": "state-log", "version": "1.2.0", "author": "Pelican", "description": "...",
             "capability": "events", "source": "https://...", "entrypoint": ["bun", "index.ts"],
             "events": ["server.state"], "settings": []},
  "host_access": ["server.logs"],
  "replaces": null,
  "signed": true,
  "approval_token": "5f2c...e9",
  "expires_at": "2026-11-15T12:10:00Z",
  "approve_with": {"method": "POST", "path": "/api/plugins/state-log/install", "body": {"approve": "5f2c...e9"}}
}
```

Show the admin `plugin` and `host_access`. If they approve, send the token
back to the same endpoint:

```http
POST /api/plugins/state-log/install
{"approve": "5f2c...e9"}

HTTP/1.1 200 OK
{"status": "installed", "plugin": { "...": "the plugin, as listed" }}
```

The second call does what `wings plugins add` does after the operator types
`y`: it moves the plugin into place, records the approval in the trust store
(rolling the install back if that fails), and asks the daemon to load newly
trusted plugins.

- The token is 32 random bytes, used once, and valid for 10 minutes. After
  that the staged files are deleted. An unknown, used or expired token is a
  409, and so is one sent to another endpoint or plugin (the stage then stays
  waiting for the right call).
- The token is tied to the staged files: if anything in the staging
  directory changed after the token was issued, the approval is refused.
- A plugin of the same name installed under a differently named directory
  (say `notify` in `notify-main`) makes install and import answer 409 at
  staging: a second copy would make discovery refuse both.
- A new stage of the same plugin replaces the one waiting. At most 16 stages
  wait at once (429 after that).
- `signed` is `true` only when the archive's signature verified against a
  key in `plugins.trusted_keys`, at any level. Below `full` that is
  required.
- At approval, under the install lock, Wings checks the node as it is now,
  not as it was when staged:
  - the access level: a stage that wasn't `signed` (or an import) needs
    `full`; if the node no longer has it, 403;
  - the signature policy: the archive's signature against the current
    `trusted_keys` and `require_signatures` (and, below `full`, a trusted
    signature); if it no longer passes, 422;
  - the destination: if the plugin was installed, removed or changed since
    staging (also under another directory name), 409. Stage again.
- `replaces` is the version being replaced (`update`, or an `import` over an
  installed plugin), or `null`.

**Install** (`POST /api/plugins/{id}/install`): `{id}` is looked up in the
node's configured registries (`plugins.registries`). It must be in exactly
one; to pick one, send `{"registry": "<namespace>"}`. `url#name` references,
which `wings plugins add` also accepts, are refused here. Installing a plugin
that is already installed is a 409; use update. The body `[]` (what the
Panel's HTTP client sends for no data) is treated as `{}`.

**Update** (`POST /api/plugins/{id}/update`): checks the plugin's `update`
URL. If there is nothing newer:

```json
{"status": "up_to_date", "installed": "1.2.0", "latest": "1.2.0"}
```

Otherwise it stages the new version (202, as above) and the approval answers
`{"status": "updated", ...}`. The staged manifest must have the same name and
the advertised version.

**Import** (`POST /api/plugins/import`, `full` only): either JSON

```json
{"url": "https://example.test/my-plugin.tar.gz", "sha256": "optional hex digest", "signature": "optional base64 signature"}
```

or a `multipart/form-data` upload with the archive in the field `plugin`
(and an optional `signature` field). The plugin's name comes from the
archive's `plugin.yaml`; an installed plugin of that name is replaced.
Archives are `.tar.gz` or `.zip` with `plugin.yaml` at the top, at most
200 MiB. Approve with `POST /api/plugins/import` and `{"approve": "<token>"}`.

### Uninstall

`POST /api/plugins/{id}/uninstall?delete=false` disables the plugin (adds it
to `plugins.disabled`) and revokes its approval, so it stops loading.
`?delete=true` also removes the directory its manifest is in (which need not
be named after the plugin), under the install lock; a manifest that isn't in
its own directory directly under the plugins directory is a 409 and nothing
changes. The
plugin's data directory (settings and state) is kept either way, and the
plugin stays in `plugins.disabled`, so installing it again later does not
turn it on by itself. Native plugins can't be uninstalled (400); disable them
instead.

```json
{"status": "uninstalled", "deleted": true}
```

### Changes take effect without a restart

Enable, disable, install, update and uninstall through this API apply to the
running daemon at once; the same changes through `wings plugins` within
about 5 seconds. External plugin processes are started or stopped to match.
A plugin whose files are edited after approval is stopped (within a poll for
its manifest and entrypoints, within about a minute for other files) and is
never started again until the operator approves it. Plugins read their
settings from `settings.json` in their data directory when it changes.

### Load order

`POST /api/plugins/order` answers 501. Each external plugin is its own
process and every event goes to every plugin subscribed to it, so there is no
order to set.

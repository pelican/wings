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
`plugins.panel_access` in `config.yml` (or `WINGS_PLUGIN_PANEL_ACCESS`). A
Panel configuration push can't change it. An unknown value counts as `off`.

| level | the Panel may |
|---|---|
| `off` | nothing; every endpoint answers 403 |
| `view` (default) | list plugins and read their files; enable, disable and configure plugins the node already trusts. No new code reaches the node. |
| `signed` | also install, update and uninstall from the node's configured registries, when the archive is signed by a key in `plugins.trusted_keys`, even if `require_signatures` is off |
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
| `GET /api/plugins/{id}` | view | one plugin |
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
starting with `.`, holding `/`, `\` or `..`) is a 400; so
`/api/plugins//uninstall` never reaches the plugins directory itself. An
unknown name is a 404.

Every change is logged with the action, the plugin and the remote address.

Errors are JSON, `{"error": "..."}`, with these statuses:

| status | when |
|---|---|
| 400 | a bad id, body or query |
| 403 | the access level does not allow it |
| 404 | no such plugin, or no such file in `/source` |
| 409 | the plugin is in the wrong state (enable an untrusted plugin, install one that is already installed), or an approval token is unknown, used or expired |
| 413 | a body or source file over its cap |
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
  `load_order` is always 0.

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
  refused with 422 and nothing is written:

```json
{"error": "plugin: invalid settings: ...", "fields": {"retries": "setting \"retries\" must be a number"}}
```

- Only installed, trusted external plugins can be configured (409 otherwise).
  The schema comes from `settings:` in the plugin's manifest. The values are
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
- A new stage of the same plugin replaces the one waiting. At most 16 stages
  wait at once (429 after that).
- Below `full`, the archive must be signed by a key in
  `plugins.trusted_keys` (`"signed": true`). A stage made at `full` without
  that signature can't be approved once the node is moved down to `signed`
  (403).
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
`?delete=true` also removes its directory, under the install lock. The
plugin's data directory (settings and state) is kept either way, and the
plugin stays in `plugins.disabled`, so installing it again later does not
turn it on by itself. Native plugins can't be uninstalled (400); disable them
instead.

```json
{"status": "uninstalled", "deleted": true}
```

### Load order

`POST /api/plugins/order` answers 501. Each external plugin is its own
process and every event goes to every plugin subscribed to it, so there is no
order to set.

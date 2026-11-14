# Wings External Plugin Wire Protocol (v1)

An external plugin is any executable Wings launches. It speaks **JSON-RPC 2.0**
over its **stdio**: it reads requests from `stdin` and writes responses to
`stdout`. Any language works — this document is the contract; the Go SDK
(`plugin/sdk`) is one implementation of it.

## Framing

- Each JSON-RPC message is a single JSON object.
- Messages are newline-delimited. Write compact JSON followed by `\n`.
- Request ids may be numbers or strings, as JSON-RPC 2.0 allows. Wings echoes a
  plugin's id back exactly as sent, and its own ids are numbers.
- `stderr` is not part of the protocol; Wings captures it and logs each line at
  warn level. Use it for diagnostics only.

## Lifecycle

1. Wings spawns the plugin with the working directory set to the plugin's own
   directory and the environment variable `WINGS_PLUGIN=1`.
2. Wings sends an `initialize` request. The plugin must respond before it will
   receive any capability calls.
3. Wings delivers capability traffic (e.g. `event` notifications) as needed.
4. When Wings closes `stdin`, the plugin should exit. A plugin that crashes is
   respawned lazily on the next call, subject to a short backoff.

The plugin process is spawned **lazily**, on first use — not at discovery. All
trust decisions (whether to run it, what it may access) are made earlier from
the manifest, without executing the plugin.

## Configuration

Config splits by ownership:

- **Per-plugin settings** live **inside the plugin's own directory** (e.g.
  `config.yaml` next to the manifest). Wings launches the plugin with its
  working directory set to that directory, so the plugin reads its own config
  itself. **Wings never parses per-plugin config** — a plugin may use any
  format it likes. This keeps a dropped-in plugin self-contained.
- **Host policy** (whether a plugin is enabled/disabled and which host
  capabilities it is granted) is **not** in the plugin directory — it lives in
  Wings' `config.yml` under `plugins:` (`autoload`, `disabled`, `grants`). A
  plugin cannot enable or grant itself; the manifest only *requests*
  capabilities and the operator's policy decides.

## Methods

### `initialize` (host → plugin, request)

```jsonc
// params
{ "host": "wings", "version": 1 }
// result
{
  "name": "state-log",
  "capability": "events",
  "provides": { "events": ["server.state"] }   // "*" = every event type
}
```

The Go SDK (`sdk.ServeEventListener`) answers `initialize` and dispatches `event`
notifications for you; any other language implements the two messages directly
(see `examples/plugins/state-log`).

## Host callbacks (plugin → host, request)

The connection is bidirectional: a plugin may call host-provided methods (e.g.
`host.*`). Wings answers only the capabilities granted to that plugin in its
manifest (`requires.hostAPI`) intersected with operator policy. Ungranted calls
return a JSON-RPC error.

## Notifications

Either side may send a notification (a message with `method` but no `id`); no
response is returned.

## Event stream (subscribe)

An `events`-capability plugin subscribes by listing event types in its manifest
`provides.events` (or `["*"]` for all). Wings pushes each matching event to the
plugin as a JSON-RPC **notification** (`method: "event"`):

```jsonc
{"jsonrpc":"2.0","method":"event","params":{"type":"server.stats","server":"<uuid>","data":{ }}}
```

Emitted event types:

| type | when | data |
|---|---|---|
| `server.state` | power state changes | `{state}` |
| `server.stats` | each resource tick | `{state, memory_bytes, memory_limit_bytes, cpu_absolute, disk_bytes, uptime}` |
| `server.install` | install start/finish | `{status: "started" \| "completed"}` |

Delivery is best-effort and asynchronous. Each plugin has its own queue of 256 events, delivered in order; if a plugin falls that far behind, newer events for it are dropped (and logged) rather than held. A message Wings cannot write within 10 seconds, because the plugin stopped reading stdin, closes the connection and stops the process. Events are node-wide today — a plugin
subscribed to `"*"` sees them for every server; per-plugin server scoping is a
follow-up. High-volume console streaming is intentionally not on this bus yet
(needs backpressure).

## Errors

Standard JSON-RPC error object: `{ "code": <int>, "message": <string> }`. Use
`-32601` for an unknown method and `-32602` for invalid params.

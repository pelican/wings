# state-log — reference Wings external plugin

A minimal, real Wings **events** plugin written in **Bun + TypeScript**, with no
runtime dependencies. It speaks the Wings external-plugin wire protocol
(JSON-RPC 2.0 over stdio) documented in
[`plugin/sdk/SPEC.md`](../../../plugin/sdk/SPEC.md).

It shows both directions of the connection:

- **Wings → plugin:** an `event` notification for every `server.state` change.
- **Plugin → Wings:** when a server goes `offline`, a `host.server.logs` call for
  the last 20 console lines. That call only works because the manifest requests
  the `server.logs` grant and the operator approved it.

Output goes to stderr, which Wings logs under the plugin's name.

## Layout

| File | Purpose |
|------|---------|
| `plugin.yaml` | Manifest. Wings reads it during discovery **without executing anything**. |
| `index.ts` | The plugin. Handles `initialize` and `event`, and makes one host call. |
| `package.json` / `tsconfig.json` | Bun project + TypeScript config (types only). |

## Run modes

Two supported entrypoints — set the one you want in `plugin.yaml`.

### 1. From source (default)

```yaml
entrypoint: ["bun", "index.ts"]
```

Bun runs the TypeScript directly — no build step. Requires Bun on the node.

### 2. Standalone binary (no runtime on the node)

```bash
bun run compile        # -> ./wings-plugin-state-log (embeds Bun)
```

```yaml
entrypoint: ["./wings-plugin-state-log"]
```

The binary is platform-specific, so build it on (or for) the target OS/arch.
It is git-ignored; build it as part of packaging.

## Install into Wings

Drop this directory under the configured plugins directory (default
`/var/lib/pelican/plugins/`), then:

```bash
wings plugins list              # should show: state-log  external  stdio
wings plugins doctor            # shows the manifest path + enable decision
wings plugins trust state-log   # review and approve before Wings can load it
```

Wings launches the entrypoint lazily on the first matching event, with
`WINGS_PLUGIN=1` set, the working directory set to this folder, and
`WINGS_PLUGIN_DATA` pointing at its data directory for any settings or state
(this example keeps none).

## Manual smoke test

```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"host":"wings","version":1}}' \
  '{"jsonrpc":"2.0","method":"event","params":{"type":"server.state","server":"abc","data":{"state":"running"}}}' \
  | WINGS_PLUGIN=1 bun index.ts
```

`initialize` prints the plugin's capability on stdout, and the event is logged on
stderr as `server abc is now running`.

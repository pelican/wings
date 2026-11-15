# python-install-log — external plugin example (Python)

The smallest useful external plugin: about 40 lines of Python with only the standard library. It shows that a
plugin is any program that reads JSON lines on stdin and writes JSON lines on stdout
([plugin/sdk/SPEC.md](../../../plugin/sdk/SPEC.md)).

It listens for `server.install` events and writes one line per install start or finish to stderr (Wings logs
it under the plugin's name) and to `installs.log` in its data directory (`$WINGS_PLUGIN_DATA`).

## Install

```bash
cp -r . /var/lib/pelican/plugins/install-log
wings plugins trust install-log   # review and approve
```

Restart Wings. The plugin starts on the first install event. The node needs `python3`.

## Run it by hand

```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"host":"wings","version":1}}' \
  '{"jsonrpc":"2.0","method":"event","params":{"type":"server.install","server":"abc","data":{"status":"started"}}}' \
  | WINGS_PLUGIN_DATA=/tmp python3 plugin.py
```

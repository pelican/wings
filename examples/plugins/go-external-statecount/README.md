# go-external-statecount — external plugin example (Go)

An external plugin written in Go with the SDK (`plugin/sdk`). Wings runs it as its own process and sends it
`server.state` events. It counts how many times each server changed state and keeps the counts in its data
directory (`$WINGS_PLUGIN_DATA/counts.json`), which survives restarts and plugin updates.

Compare with [go-native-auditlog](../go-native-auditlog): same language, but this one is a separate process
the operator installs and approves, instead of code compiled into Wings.

## A compiled plugin is a binary plugin

Go has to be compiled, so this plugin ships an executable, and Wings shows it to the operator as **binary**
when they approve it, listing `wings-plugin-statecount`: they can't read what they are approving, only decide
whether they trust you. If people should be able to read your plugin, write it in an interpreted language
(see [python-install-log](../python-install-log) or [state-log](../state-log)). If you ship a compiled one,
publish its source (`source:` in `plugin.yaml`) and sign your releases (`wings plugins sign`).

## Build and install

```bash
go build -o wings-plugin-statecount .        # inside this repo; elsewhere, a module that requires github.com/pelican/wings
cp -r . /var/lib/pelican/plugins/statecount  # or package it and publish it to a registry
wings plugins trust statecount               # shows what it is (binary, runs as system.user) and asks to approve
```

Restart Wings. The plugin starts on the first state change.

Outside this repo, `go get github.com/pelican/wings/plugin/sdk` is the only dependency. It adds no third-party
modules, but the SDK lives in the Wings module, so your module needs at least the Go version in Wings' `go.mod`
(`go mod tidy` raises your `go` line to match).

## Run it by hand

```bash
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"host":"wings","version":1}}' \
  '{"jsonrpc":"2.0","method":"event","params":{"type":"server.state","server":"abc","data":{"state":"running"}}}' \
  | WINGS_PLUGIN_DATA=/tmp ./wings-plugin-statecount
```

stdout has the `initialize` answer, stderr logs `abc is now running (1 times)`, and `/tmp/counts.json` holds the
count.

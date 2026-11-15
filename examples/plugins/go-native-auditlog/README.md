# go-native-auditlog — native plugin example (Go, compiled in)

A native plugin is an ordinary Go package that registers itself from `init()`. This one shows all three
native hooks:

| Hook | What it does here |
|---|---|
| `plugin.RegisterEventListener` | Logs every `server.state` change and remembers it for an hour |
| `plugin.RegisterRoutes` | `GET /api/ext/auditlog/recent` (node token) returns the recent changes |
| `plugin.RegisterTask` | Every minute, drops changes older than an hour |

## Use it

Add one line to `plugin/builtin/builtin.go` and rebuild Wings:

```go
import _ "github.com/pelican/wings/examples/plugins/go-native-auditlog"
```

That is the only change to Wings' own files. A fork keeps its plugins in its own packages the same way, so
merging a new Wings release touches one import list.

Native plugins are trusted like the rest of Wings (they are part of the binary). They still follow the
operator's policy: `wings plugins disable auditlog` stops the listener, the route and the task.

## Test

```bash
go test ./examples/plugins/go-native-auditlog/
```

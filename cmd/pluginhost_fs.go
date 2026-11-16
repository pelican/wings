package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"

	"github.com/pelican/wings/server"
	"github.com/pelican/wings/server/filesystem"
)

// maxHostReadBytes / maxHostWriteBytes cap host.fs.read / host.fs.write so a
// plugin cannot move an unbounded file through the control plane. Larger
// transfers belong on a streaming method.
const (
	maxHostReadBytes  = 1 << 20 // 1 MiB
	maxHostWriteBytes = 1 << 20 // 1 MiB
)

// fsMethod adapts a filesystem operation to a server-scoped host method. The
// operations take the server's Filesystem rather than the server so they can be
// tested against a filesystem directly.
func fsMethod(fn func(fs *filesystem.Filesystem, params json.RawMessage) (any, error)) hostFn {
	return func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		return fn(s.Filesystem(), params)
	}
}

// The checks below match the HTTP file API in router/router_server_files.go, so
// a plugin cannot reach a file that the server's denylist hides from its user:
// reads, writes, copies and chmods check IsIgnored, renames check
// IsIgnoredNoFollow on both ends, and deletes go through
// SafeDeleteRecursively, which leaves denylisted entries in place.
func init() {
	// --- read ---

	register("fs.list", true, fsMethod(fsList))
	register("fs.read", true, fsMethod(fsRead))
	register("fs.diskUsage", true, fsMethod(func(fs *filesystem.Filesystem, _ json.RawMessage) (any, error) {
		used, err := fs.DiskUsage(true)
		if err != nil {
			return nil, fsError(err)
		}
		return map[string]any{"disk_bytes": used}, nil
	}))

	// --- mutating (grant-gated in plugin.HostHandler) ---

	register("fs.write", true, fsMethod(fsWrite))
	register("fs.mkdir", true, fsMethod(fsMkdir))
	register("fs.delete", true, fsMethod(fsDelete))
	register("fs.rename", true, fsMethod(fsRename))
	register("fs.copy", true, fsMethod(fsCopy))
	register("fs.chmod", true, fsMethod(fsChmod))
}

func fsList(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[pathArgs](params)
	if err != nil {
		return nil, err
	}
	if args.Path == "" {
		args.Path = "/"
	}
	entries, err := fs.ListDirectory(args.Path)
	if err != nil {
		return nil, fsError(err)
	}
	return entries, nil
}

// fsRead returns a capped, base64-encoded read of a server-relative file.
func fsRead(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[pathArgs](params)
	if err != nil {
		return nil, err
	}
	if err := fs.IsIgnored(args.Path); err != nil {
		return nil, fsError(err)
	}
	f, _, err := fs.File(args.Path)
	if err != nil {
		return nil, fsError(err)
	}
	defer f.Close()

	data, err := io.ReadAll(io.LimitReader(f, maxHostReadBytes+1))
	if err != nil {
		return nil, fsError(err)
	}
	truncated := false
	if len(data) > maxHostReadBytes {
		data = data[:maxHostReadBytes]
		truncated = true
	}
	return map[string]any{
		"content_base64": base64.StdEncoding.EncodeToString(data),
		"bytes":          len(data),
		"truncated":      truncated,
	}, nil
}

// fsWrite writes a capped, base64-encoded body to a server-relative file.
func fsWrite(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[writeArgs](params)
	if err != nil {
		return nil, err
	}
	data, derr := base64.StdEncoding.DecodeString(args.ContentBase64)
	if derr != nil {
		return nil, rpcErr(-32602, "content_base64 is not valid base64")
	}
	if len(data) > maxHostWriteBytes {
		return nil, rpcErr(-32602, "content exceeds the write cap; use a streaming write")
	}
	if err := fs.IsIgnored(args.Path); err != nil {
		return nil, fsError(err)
	}
	mode := uint64(0o644)
	if args.Mode != 0 {
		mode = uint64(args.Mode)
	}
	if err := fs.Write(args.Path, bytes.NewReader(data), int64(len(data)), filesystem.FileModeFromUnix(mode)); err != nil {
		return nil, fsError(err)
	}
	return map[string]any{"bytes": len(data)}, nil
}

func fsMkdir(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[mkdirArgs](params)
	if err != nil {
		return nil, err
	}
	if err := fs.CreateDirectory(args.Name, args.Path); err != nil {
		return nil, fsError(err)
	}
	return okResult, nil
}

func fsDelete(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[pathArgs](params)
	if err != nil {
		return nil, err
	}
	if args.Path == "" || args.Path == "/" || args.Path == "." {
		return nil, rpcErr(-32602, "refusing to delete the server root")
	}
	if err := fs.IsIgnoredNoFollow(args.Path); err != nil {
		return nil, fsError(err)
	}
	if err := fs.SafeDeleteRecursively(args.Path); err != nil {
		return nil, fsError(err)
	}
	return okResult, nil
}

func fsRename(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[renameArgs](params)
	if err != nil {
		return nil, err
	}
	if err := fs.IsIgnoredNoFollow(args.From, args.To); err != nil {
		return nil, fsError(err)
	}
	if err := fs.Rename(args.From, args.To); err != nil {
		return nil, fsError(err)
	}
	return okResult, nil
}

func fsCopy(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[pathArgs](params)
	if err != nil {
		return nil, err
	}
	if err := fs.IsIgnored(args.Path); err != nil {
		return nil, fsError(err)
	}
	if err := fs.Copy(args.Path); err != nil {
		return nil, fsError(err)
	}
	return okResult, nil
}

// fsChmod takes a Unix mode (e.g. 0o4755) and converts it the way the HTTP
// chmod endpoint does; os.FileMode(n) would misplace the setuid, setgid and
// sticky bits.
func fsChmod(fs *filesystem.Filesystem, params json.RawMessage) (any, error) {
	args, err := decode[chmodArgs](params)
	if err != nil {
		return nil, err
	}
	if err := fs.IsIgnored(args.Path); err != nil {
		return nil, fsError(err)
	}
	if err := fs.Chmod(args.Path, filesystem.FileModeFromUnix(uint64(args.Mode))); err != nil {
		return nil, fsError(err)
	}
	return okResult, nil
}

// fsError wraps a filesystem error as a JSON-RPC error. Filesystem errors are
// already server-relative (paths resolve inside the server's os.Root), so they
// do not leak host layout.
func fsError(err error) error { return rpcErr(-32000, err.Error()) }

type pathArgs struct {
	Path string `json:"path"`
}

type mkdirArgs struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type renameArgs struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type chmodArgs struct {
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type writeArgs struct {
	Path          string `json:"path"`
	ContentBase64 string `json:"content_base64"`
	Mode          uint32 `json:"mode"`
}

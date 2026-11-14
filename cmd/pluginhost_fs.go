package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"

	"github.com/pelican/wings/server"
)

// maxHostReadBytes / maxHostWriteBytes cap host.fs.read / host.fs.write so a
// plugin cannot move an unbounded file through the control plane. Larger
// transfers belong on a streaming method.
const (
	maxHostReadBytes  = 1 << 20 // 1 MiB
	maxHostWriteBytes = 1 << 20 // 1 MiB
)

func init() {
	// --- read ---

	register("fs.list", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[pathArgs](params)
		if err != nil {
			return nil, err
		}
		if args.Path == "" {
			args.Path = "/"
		}
		entries, err := s.Filesystem().ListDirectory(args.Path)
		if err != nil {
			return nil, fsError(err)
		}
		return entries, nil
	})

	register("fs.read", true, fsRead)

	register("fs.diskUsage", true, func(_ pluginHost, _ context.Context, s *server.Server, _ json.RawMessage) (any, error) {
		used, err := s.Filesystem().DiskUsage(true)
		if err != nil {
			return nil, fsError(err)
		}
		return map[string]any{"disk_bytes": used}, nil
	})

	// --- mutating (grant-gated in plugin.HostHandler) ---

	register("fs.write", true, fsWrite)

	register("fs.mkdir", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[mkdirArgs](params)
		if err != nil {
			return nil, err
		}
		if err := s.Filesystem().CreateDirectory(args.Name, args.Path); err != nil {
			return nil, fsError(err)
		}
		return okResult, nil
	})

	register("fs.delete", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[pathArgs](params)
		if err != nil {
			return nil, err
		}
		if args.Path == "" || args.Path == "/" || args.Path == "." {
			return nil, rpcErr(-32602, "refusing to delete the server root")
		}
		if err := s.Filesystem().Delete(args.Path); err != nil {
			return nil, fsError(err)
		}
		return okResult, nil
	})

	register("fs.rename", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[renameArgs](params)
		if err != nil {
			return nil, err
		}
		if err := s.Filesystem().Rename(args.From, args.To); err != nil {
			return nil, fsError(err)
		}
		return okResult, nil
	})

	register("fs.copy", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[pathArgs](params)
		if err != nil {
			return nil, err
		}
		if err := s.Filesystem().Copy(args.Path); err != nil {
			return nil, fsError(err)
		}
		return okResult, nil
	})

	register("fs.chmod", true, func(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
		args, err := decode[chmodArgs](params)
		if err != nil {
			return nil, err
		}
		if err := s.Filesystem().Chmod(args.Path, os.FileMode(args.Mode)); err != nil {
			return nil, fsError(err)
		}
		return okResult, nil
	})
}

// fsRead returns a capped, base64-encoded read of a server-relative file.
func fsRead(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
	args, err := decode[pathArgs](params)
	if err != nil {
		return nil, err
	}
	f, _, err := s.Filesystem().File(args.Path)
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
func fsWrite(_ pluginHost, _ context.Context, s *server.Server, params json.RawMessage) (any, error) {
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
	mode := os.FileMode(0o644)
	if args.Mode != 0 {
		mode = os.FileMode(args.Mode)
	}
	if err := s.Filesystem().Write(args.Path, bytes.NewReader(data), int64(len(data)), mode); err != nil {
		return nil, fsError(err)
	}
	return map[string]any{"bytes": len(data)}, nil
}

// fsError wraps a filesystem error as a JSON-RPC error. Filesystem errors are
// already server-relative (paths resolve inside the server's os.Root), so they do not leak
// host layout.
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

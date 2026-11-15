package plugins

import (
	"reflect"
	"strings"

	"github.com/traefik/yaegi/stdlib"
)

// Symbols is the symbol table exposed to interpreted plugin source.
//
// The generated github_com-pelican-wings-plugins-api.go file registers the
// plugin API package into it. Regenerate that file with "make plugin-symbols"
// after changing anything in plugins/api, or interpreted plugins will still be
// compiled against the previous contract.
var Symbols = map[string]map[string]reflect.Value{}

// safeStdlib is the set of standard library packages a plugin may import.
//
// yaegi's own Unrestricted option is not the control it sounds like: with the
// full stdlib symbol table loaded, it gates only os/exec and unsafe, leaving a
// plugin free to call os.RemoveAll, open its own sockets, or reach into the
// daemon through reflect. Since Wings decides what goes in the table, the
// smaller table is the actual boundary.
//
// What is listed here is the part of the standard library that computes without
// reaching outside the process: text handling, encoding, hashing, math, time,
// sorting, and the pure-parsing halves of net and path. Everything a plugin
// needs to touch the world outside itself, meaning files, servers, the network
// and the Panel, arrives through api.Host instead, where it is scoped to the
// server in question and shows up in the logs attributed to the plugin.
//
// Deliberately absent, and the reason for each:
//
//   - os, os/user, os/signal: unscoped read and write over the whole host
//     filesystem. api.Files covers a server's own directory with the path
//     checks and disk accounting a user's request gets.
//   - net, net/http and the rest of net's clients: outbound connections that
//     nothing could account for. api.Host's fetcher does the same job and can
//     be restricted to an allowlist by the node operator.
//   - reflect, unsafe: both defeat the boundary entirely, since either one can
//     reach a value the plugin was never handed.
//   - runtime, runtime/debug, debug/*: process introspection and control,
//     including the ability to stop the daemon.
//   - syscall, plugin, testing, log: direct kernel access, native code
//     loading, and a logger writing somewhere other than the Wings log.
var safeStdlib = []string{
	"bufio", "bytes", "cmp", "container/heap", "container/list", "container/ring",
	"context",
	"crypto", "crypto/aes", "crypto/cipher", "crypto/des", "crypto/hmac",
	"crypto/md5", "crypto/rand", "crypto/rc4", "crypto/sha1", "crypto/sha256",
	"crypto/sha512", "crypto/subtle",
	"encoding", "encoding/ascii85", "encoding/base32", "encoding/base64",
	"encoding/binary", "encoding/csv", "encoding/gob", "encoding/hex",
	"encoding/json", "encoding/pem", "encoding/xml",
	"errors", "fmt",
	"hash", "hash/adler32", "hash/crc32", "hash/crc64", "hash/fnv", "hash/maphash",
	"html", "html/template",
	"io", "io/fs", "iter",
	"maps", "math", "math/big", "math/bits", "math/cmplx", "math/rand", "math/rand/v2",
	"mime", "mime/multipart", "mime/quotedprintable",
	"net/mail", "net/netip", "net/textproto", "net/url",
	"path", "path/filepath",
	"regexp", "regexp/syntax",
	"slices", "sort", "strconv", "strings",
	"sync", "sync/atomic",
	"text/tabwriter", "text/template",
	"time",
	"unicode", "unicode/utf16", "unicode/utf8",
}

// buildSymbols returns the symbol table for a plugin.
//
// When trusted is set the plugin gets the full yaegi standard library instead
// of the curated set. That is an operator decision made in the Wings config
// file, never something a plugin can ask for in its own manifest, because a
// plugin that could grant itself trust would make the distinction pointless.
func buildSymbols(trusted bool) map[string]map[string]reflect.Value {
	out := make(map[string]map[string]reflect.Value, len(safeStdlib)+len(Symbols))

	if trusted {
		for k, v := range stdlib.Symbols {
			out[k] = v
		}
	} else {
		for _, pkg := range safeStdlib {
			// yaegi keys its table by import path plus package name, so "fmt"
			// is stored as "fmt/fmt" and "net/url" as "net/url/url".
			key := pkg + "/" + pkg[strings.LastIndex(pkg, "/")+1:]
			if syms, ok := stdlib.Symbols[key]; ok {
				out[key] = syms
			}
		}
	}

	// The plugin API always goes in, trusted or not: it is the contract.
	for k, v := range Symbols {
		out[k] = v
	}

	return out
}

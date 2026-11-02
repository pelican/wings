package api

import "time"

// Host is the node, as seen by a plugin. A plugin receives one in Init and can
// hold onto it for as long as it is enabled.
//
// Every method is scoped to what the plugin is allowed to touch. A call that
// names a server the plugin has no business with, or that the node's
// configuration forbids, returns an error rather than doing the work partially.
type Host interface {
	// Logger writes to the Wings log with the plugin's id already attached, so
	// an operator can tell whose line it is.
	Logger() Logger

	// Node describes the Wings instance the plugin is running on.
	Node() Node

	// Settings returns the plugin's own settings, as the operator has them set
	// on the Panel. Keys are the ones declared in the manifest.
	Settings() Settings

	// Store is durable per-plugin key/value storage, kept in the Wings
	// database and surviving restarts and upgrades.
	Store() Store

	// Servers lists and controls the servers on this node.
	Servers() Servers

	// Files reads and writes inside a server's data directory.
	Files() Files

	// Panel calls the Panel API as this node.
	Panel() Panel

	// HTTP performs outbound HTTP requests on the plugin's behalf.
	HTTP() Fetcher

	// Events publishes onto a server's event bus, which is what the Panel's
	// console and status displays are listening to.
	Events() Events
}

// Logger writes to the Wings log.
type Logger interface {
	Debug(msg string)
	Info(msg string)
	Warn(msg string)
	Error(msg string)

	// With returns a logger that adds the given fields to every line. Use it
	// rather than formatting values into the message.
	With(fields map[string]any) Logger
}

// Settings is a plugin's configuration, as set by the operator on the Panel.
//
// The available keys are declared in the plugin's manifest, which is what lets
// the Panel render a form for them without the plugin running. Reads always
// reflect the current values: when an operator saves new settings, Wings
// updates them in place and calls the plugin's SettingsChanged hook.
type Settings interface {
	// String returns a string setting, or fallback when it is unset or empty.
	String(key, fallback string) string

	// Int returns an integer setting, or fallback when unset or not a number.
	Int(key string, fallback int) int

	// Bool returns a boolean setting, or fallback when unset.
	Bool(key string, fallback bool) bool

	// Duration parses a setting as a Go duration such as "30s", returning
	// fallback when unset or unparseable.
	Duration(key string, fallback time.Duration) time.Duration

	// StringSlice returns a list setting, or nil when unset.
	StringSlice(key string) []string

	// All returns every setting, for a plugin that would rather decode the
	// whole map itself.
	All() map[string]any
}

// Store is durable key/value storage private to one plugin.
//
// Keys are namespaced to the plugin, so two plugins using the key "state" do
// not collide, and uninstalling a plugin discards its keys. Values are opaque
// bytes; encode structured data yourself.
type Store interface {
	Get(key string) ([]byte, bool, error)
	Set(key string, value []byte) error
	Delete(key string) error

	// Keys lists the plugin's keys carrying the given prefix. Pass an empty
	// prefix for all of them.
	Keys(prefix string) ([]string, error)

	// SetWithTTL stores a value that Wings discards once ttl has elapsed. Use
	// it for caches rather than for state you cannot rebuild.
	SetWithTTL(key string, value []byte, ttl time.Duration) error
}

// Servers lists and controls the servers on this node.
type Servers interface {
	// List returns a snapshot of every server on the node.
	List() []Server

	// Get returns one server by UUID. The second result reports whether it
	// exists on this node.
	Get(uuid string) (Server, bool)

	// Power requests a power action, blocking until Wings has accepted it
	// rather than until the server finishes transitioning. A plugin calling
	// this from inside a power hook for the same server deadlocks, so don't.
	Power(uuid string, action PowerAction) error

	// SendCommand writes a command to a running server's console.
	SendCommand(uuid, command string) error

	// ConsoleWrite prints a line to a server's console as coming from the
	// plugin, so users can see that something automated acted.
	ConsoleWrite(uuid, line string) error

	// ReadLog returns the last lines of a server's console output, newest
	// last, at most 500 lines.
	ReadLog(uuid string, lines int) ([]string, error)

	// SaveActivity records an entry in the server's activity log on the Panel,
	// attributed to the plugin. Use it for actions an operator would want to
	// find later.
	SaveActivity(uuid, event string, meta map[string]any) error

	// Exec runs a command inside a running server's container and returns its
	// output and exit code.
	//
	// This is not the console. SendCommand writes to whatever process is
	// already running and leaves you guessing at the reply from console
	// output; an exec gets its own stdout, stderr and exit status, which is
	// what makes a health check or a one-off query possible.
	//
	// The command runs as the same user the container runs as, never as root,
	// and only while the server is running. There is no option to change
	// either: the container's user is the boundary that keeps a server's files
	// from being the host's.
	Exec(uuid string, command []string, opts ExecOptions) (ExecResult, error)
}

// Files reads and writes inside a server's data directory.
//
// Every path is relative to that directory and cannot escape it: a path that
// resolves outside, or that the egg's file denylist covers, returns an error.
// Writes count against the server's disk limit exactly as a user's would.
type Files interface {
	Read(uuid, path string) ([]byte, error)
	Write(uuid, path string, content []byte) error
	Delete(uuid, path string) error
	Rename(uuid, from, to string) error
	CreateDirectory(uuid, path string) error
	Chmod(uuid, path string, mode uint32) error
	List(uuid, path string) ([]DirEntry, error)

	// Exists reports whether a path exists, without reading it.
	Exists(uuid, path string) (bool, error)

	// Stat describes one path without listing its parent, which is the cheap
	// way to ask how big something is or when it changed.
	Stat(uuid, path string) (DirEntry, error)

	// Copy duplicates a file or directory. Wings picks a free name beside the
	// original, the same way the file manager's copy does, and returns where
	// it put it.
	Copy(uuid, path string) (string, error)

	// Compress packs the named entries of a directory into a new archive in
	// that same directory, and returns the archive's path.
	//
	// Entries the egg's denylist covers are skipped, and an archive with
	// nothing left to pack is an error rather than an empty file.
	Compress(uuid, directory string, names []string, opts CompressOptions) (string, error)

	// Space reports the server's disk usage against its limit, so a plugin
	// can decide whether something large is worth starting.
	Space(uuid string) (DiskSpace, error)

	// Download fetches a URL straight onto the server's disk.
	//
	// The bytes never pass through the plugin or the Panel: Wings streams them
	// from the source to the server's data directory, with the same disk
	// accounting a user's upload gets. That is the difference that matters for
	// anything bulky, where routing a file through the Panel means moving it
	// twice.
	//
	// Wings refuses a URL that resolves to the local network, follows a bounded
	// number of redirects, and declines entirely when the node has remote
	// downloads switched off.
	Download(uuid string, req DownloadRequest) (DownloadResult, error)

	// DownloadMany fetches several URLs at once, returning a result per
	// request in the order they were given.
	//
	// One failure does not sink the batch: each result carries its own error,
	// because a modpack with one dead link should still install the other two
	// hundred mods and say which one was missing.
	//
	// Concurrency is how many run at once, clamped by Wings to something the
	// node can stand. Pass zero for the default.
	DownloadMany(uuid string, reqs []DownloadRequest, concurrency int) ([]DownloadResult, error)

	// Decompress extracts an archive that is already on the server's disk,
	// into the directory the archive itself is in.
	//
	// There is no separate destination because Wings has none: extraction
	// always lands beside the archive. Move or rename afterwards if the
	// contents belong somewhere else.
	//
	// The expanded size is checked against the server's disk limit before
	// anything is written, so an archive cannot fill the disk on the way out.
	Decompress(uuid, archive string) error

	// Digest hashes a file on the node and returns the results, without the
	// contents ever reaching the plugin.
	//
	// Use this rather than reading a file to hash it yourself. Plugin code is
	// interpreted, so hashing a few dozen files that way runs at a small
	// fraction of native speed and is the slowest thing a plugin is likely to
	// do; here the work happens in Wings itself, and the file is streamed
	// rather than held in memory.
	//
	// Results are keyed by each request's Name, falling back to its
	// Algorithm.
	Digest(uuid, path string, requests []DigestRequest) (map[string]string, error)
}

// CompressOptions shapes a new archive. The zero value is a gzipped tar with
// a generated, timestamped name.
type CompressOptions struct {
	// Name is the archive's name without an extension, which Wings appends to
	// match the format. Empty gets a generated name.
	Name string

	// Format is one of zip, tar.gz, tar.bz2 or tar.xz. Anything else, empty
	// included, is a gzipped tar.
	Format string
}

// DiskSpace is a server's disk usage.
type DiskSpace struct {
	// UsedBytes is what the server's directory currently occupies.
	UsedBytes int64

	// LimitBytes is what it is allowed, or zero when it is unlimited. Check
	// this before dividing one by the other.
	LimitBytes int64
}

// Unlimited reports whether the server has no disk limit.
func (d DiskSpace) Unlimited() bool { return d.LimitBytes <= 0 }

// FreeBytes is how much room is left, or -1 when the server is unlimited.
func (d DiskSpace) FreeBytes() int64 {
	if d.Unlimited() {
		return -1
	}
	if d.UsedBytes >= d.LimitBytes {
		return 0
	}
	return d.LimitBytes - d.UsedBytes
}

// HasRoomFor reports whether n more bytes would fit. An unlimited server
// always has room.
func (d DiskSpace) HasRoomFor(n int64) bool {
	return d.Unlimited() || d.UsedBytes+n <= d.LimitBytes
}

// DownloadRequest is one file to fetch onto a server.
type DownloadRequest struct {
	// URL is what to fetch. Only http and https are accepted.
	URL string

	// Directory is where to put it, relative to the server root.
	Directory string

	// FileName names the file. Left empty, Wings derives it from the URL.
	FileName string

	// UseHeader names the file from the response instead, which is what you
	// want when the URL is a redirect to a CDN and its path says nothing
	// useful about what is being downloaded.
	UseHeader bool
}

// DownloadResult is what became of one request.
type DownloadResult struct {
	URL string

	// Path is where the file landed, relative to the server root.
	Path string

	// Bytes is how much was written.
	Bytes int64

	// Error is why this one failed, and is empty when it did not. The rest of
	// a batch is unaffected.
	Error string
}

// DigestRequest asks for one hash of a file.
//
// It describes a primitive and how to feed it, rather than naming a
// higher-level format, so that knowledge of what a particular hash means stays
// in the plugin that cares. A plugin needing some ecosystem's file
// fingerprint composes it here and names the result whatever it likes; Wings
// only supplies the arithmetic.
type DigestRequest struct {
	// Algorithm is the hash to compute, one of:
	//
	//	md5, sha1, sha256, sha512   lowercase hex
	//	crc32                       decimal, IEEE polynomial
	//	murmur2                     decimal, MurmurHash2 32-bit, Austin
	//	                            Appleby's original variant
	//
	// An unrecognised name is an error rather than a missing key, so a typo
	// surfaces instead of quietly producing nothing.
	Algorithm string

	// Name is the key this result appears under. Defaults to Algorithm, and
	// is what lets a plugin ask for the same algorithm twice with different
	// options, or label a hash by what it means rather than how it is
	// computed.
	Name string

	// IgnoreWhitespace drops every tab, newline, carriage return and space
	// from the content before hashing. Applies to any algorithm.
	IgnoreWhitespace bool

	// Seed initialises the hash, for algorithms that take one. Only murmur2
	// uses it, and it is not defaulted: a zero seed is a legitimate choice, so
	// a plugin that needs a particular one has to say so.
	Seed uint32
}

// Panel calls the Panel API using the node's own credentials.
//
// Wings does not interpret the response, so a plugin can reach endpoints its
// Panel-side counterpart added. Requests go to the Panel this node is attached
// to; path is relative to the Panel root, such as "/api/application/servers".
type Panel interface {
	// Request performs an HTTP call against the Panel. It returns the status
	// code and body. A non-2xx status is returned rather than turned into an
	// error, so the caller decides what counts as failure.
	Request(method, path string, body []byte, header map[string]string) (int, []byte, error)
}

// Fetcher performs outbound HTTP requests for a plugin.
//
// A plugin cannot open a socket of its own: neither net nor net/http is in the
// symbol table its source is interpreted against. This is the way out, which
// means every call a plugin makes off the node is attributable to it in the
// logs, and an operator can narrow what is reachable with the plugins
// allowed_hosts setting in the Wings config.
type Fetcher interface {
	// Do performs the request and returns the status code and body. A non-2xx
	// status is returned rather than turned into an error, so the caller
	// decides what counts as failure. Redirects are followed; the body is
	// capped, and a response over the cap is an error rather than a truncated
	// body that would parse as something else.
	Do(method, url string, body []byte, header map[string]string) (int, []byte, error)
}

// Events publishes onto a server's event bus.
//
// These are the events the Panel's websocket forwards to a browser, so a plugin
// publishing here can drive its own UI in the Panel without adding an endpoint.
type Events interface {
	// Publish sends an event on a server's bus. Prefix the topic with the
	// plugin's id so it cannot collide with a Wings event; Wings rejects a
	// topic that shadows a built-in one.
	Publish(uuid, topic string, data any) error
}

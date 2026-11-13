package plugins

import "github.com/pelican/wings/plugins/api"

// Bridge is how the plugin subsystem reaches the rest of Wings.
//
// It exists to keep the dependency arrow pointing one way. The server package,
// the router and the Docker environment all call into this package to dispatch
// hooks, so this package cannot import them back without a cycle. Instead
// cmd wires in an implementation at boot, and everything a plugin does to the
// node goes through here.
//
// Every method is safe to call concurrently, and every one that names a server
// returns an error rather than panicking when that server is not on this node.
type Bridge interface {
	// Node describes the daemon, for api.Host.Node.
	Node() api.Node

	ListServers() []api.Server
	GetServer(uuid string) (api.Server, bool)

	Power(uuid string, action api.PowerAction) error
	SendCommand(uuid, command string) error
	ConsoleWrite(uuid, line string) error
	ReadLog(uuid string, lines int) ([]string, error)

	// SaveActivity records an activity entry against a server, attributed to
	// the named plugin.
	SaveActivity(uuid, pluginID, event string, meta map[string]any) error

	// Exec runs a command inside a running server's container.
	Exec(uuid string, command []string, opts api.ExecOptions) (api.ExecResult, error)

	// PublishEvent pushes onto a server's event bus.
	PublishEvent(uuid, topic string, data any) error

	FileRead(uuid, path string) ([]byte, error)
	FileWrite(uuid, path string, content []byte) error
	FileDelete(uuid, path string) error
	FileRename(uuid, from, to string) error
	FileCreateDirectory(uuid, path string) error
	FileChmod(uuid, path string, mode uint32) error
	FileList(uuid, path string) ([]api.DirEntry, error)
	FileExists(uuid, path string) (bool, error)

	// FileStat describes one path on a server.
	FileStat(uuid, path string) (api.DirEntry, error)

	// FileCopy duplicates a file or directory beside itself.
	FileCopy(uuid, path string) (string, error)

	// FileCompress packs entries of a directory into a new archive there.
	FileCompress(uuid, directory string, names []string, opts api.CompressOptions) (string, error)

	// FileSpace reports a server's disk usage against its limit.
	FileSpace(uuid string) (api.DiskSpace, error)

	// FileDownload fetches a URL onto a server's disk.
	FileDownload(uuid string, req api.DownloadRequest) (api.DownloadResult, error)

	// FileDownloadMany fetches several URLs concurrently.
	FileDownloadMany(uuid string, reqs []api.DownloadRequest, concurrency int) ([]api.DownloadResult, error)

	// FileDecompress extracts an archive already on a server's disk, beside
	// the archive itself.
	FileDecompress(uuid, archive string) error

	// FileDigest hashes a file on the node, computing every requested digest
	// without handing the contents to the caller.
	FileDigest(uuid, path string, requests []api.DigestRequest) (map[string]string, error)

	// PanelRequest calls the Panel API with the node's credentials.
	PanelRequest(method, path string, body []byte, header map[string]string) (int, []byte, error)
}

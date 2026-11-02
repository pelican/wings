// Package pluginbridge connects the plugin subsystem to the rest of Wings.
//
// It exists to break a dependency cycle. The server package, the router and the
// Docker environment all dispatch plugin hooks, so the plugins package cannot
// import them back. This package sits above both and is wired in at boot, which
// is why it is the only place that knows how a plugin's request to, say, read a
// file becomes an actual call on a server's filesystem.
package pluginbridge

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	gopath "path"
	"runtime"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/environment/docker"
	"github.com/pelican/wings/internal/models"
	"github.com/pelican/wings/internal/ufs"
	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/server"
	"github.com/pelican/wings/system"

	"github.com/shirou/gopsutil/v3/mem"
)

// Bridge implements plugins.Bridge over the live server manager.
type Bridge struct {
	manager *server.Manager

	// http is used for the Panel calls plugins make. It is separate from the
	// remote client because a plugin addresses arbitrary Panel paths, which
	// the typed client deliberately does not expose.
	http *http.Client
}

var _ plugins.Bridge = (*Bridge)(nil)

// New returns a bridge over the given server manager.
func New(m *server.Manager) *Bridge {
	return &Bridge{
		manager: m,
		http: &http.Client{
			Timeout: time.Duration(config.Get().RemoteQuery.Timeout) * time.Second,
		},
	}
}

// Node describes this daemon for api.Host.Node.
func (b *Bridge) Node() api.Node {
	cfg := config.Get()

	node := api.Node{
		UUID:         cfg.Uuid,
		Version:      system.Version,
		PanelURL:     cfg.PanelLocation,
		Architecture: runtime.GOARCH,
		OS:           runtime.GOOS,
		CPUThreads:   runtime.NumCPU(),
	}

	node.MemoryBytes = totalMemory()

	return node
}

var (
	memoryOnce  sync.Once
	memoryTotal int64
)

// totalMemory reads the host's total memory once.
//
// Node() is cheap enough that a plugin may well call it per hook, and the
// host's installed memory does not change while the daemon is running, so
// reading it repeatedly would be waste. A failure leaves it at zero, which
// reads as "not known" rather than as an error a plugin has to handle.
func totalMemory() int64 {
	memoryOnce.Do(func() {
		if v, err := mem.VirtualMemory(); err == nil {
			memoryTotal = int64(v.Total)
		}
	})
	return memoryTotal
}

// --- servers ---

func (b *Bridge) ListServers() []api.Server {
	all := b.manager.All()

	out := make([]api.Server, 0, len(all))
	for _, s := range all {
		out = append(out, s.PluginSnapshot())
	}
	return out
}

func (b *Bridge) GetServer(uuid string) (api.Server, bool) {
	s, ok := b.manager.Get(uuid)
	if !ok {
		return api.Server{}, false
	}
	return s.PluginSnapshot(), true
}

// find resolves a server or returns an error naming the uuid, which is what a
// plugin needs to see when it has stale state.
func (b *Bridge) find(uuid string) (*server.Server, error) {
	s, ok := b.manager.Get(uuid)
	if !ok {
		return nil, errors.Errorf("pluginbridge: no server %s on this node", uuid)
	}
	return s, nil
}

func (b *Bridge) Power(uuid string, action api.PowerAction) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	return s.HandlePowerAction(server.PowerAction(action))
}

func (b *Bridge) SendCommand(uuid, command string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	if !s.IsRunning() {
		return errors.Errorf("pluginbridge: server %s is not running", uuid)
	}
	return s.Environment.SendCommand(command)
}

func (b *Bridge) ConsoleWrite(uuid, line string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	s.PublishConsoleOutputFromDaemon(line)
	return nil
}

func (b *Bridge) ReadLog(uuid string, lines int) ([]string, error) {
	s, err := b.find(uuid)
	if err != nil {
		return nil, err
	}
	return s.ReadLogfile(lines)
}

func (b *Bridge) SaveActivity(uuid, pluginID, event string, meta map[string]any) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}

	m := models.ActivityMeta{}
	for k, v := range meta {
		m[k] = v
	}
	// Record which plugin was responsible, so an entry in the activity log
	// that nobody recognises can be traced back to the plugin that wrote it.
	m["plugin"] = pluginID

	// Plugin activity has no user and no remote address behind it, so it is
	// attributed to the daemon the same way Wings' own actions are.
	s.SaveActivity(s.NewRequestActivity("", "127.0.0.1"), models.Event(event), m)
	return nil
}

func (b *Bridge) PublishEvent(uuid, topic string, data any) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	s.Events().Publish(topic, data)
	return nil
}

// --- files ---

// maxPluginReadBytes caps a single file read by a plugin. The content is
// assembled in memory and handed to interpreted code, so an unbounded read of a
// multi-gigabyte world file would be charged to the daemon's heap.
const maxPluginReadBytes = 8 * 1024 * 1024

func (b *Bridge) FileRead(uuid, path string) ([]byte, error) {
	s, err := b.find(uuid)
	if err != nil {
		return nil, err
	}

	f, st, err := s.Filesystem().File(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	if st.IsDir() {
		return nil, errors.Errorf("pluginbridge: %s is a directory", path)
	}
	if st.Size() > maxPluginReadBytes {
		return nil, errors.Errorf("pluginbridge: %s is %d bytes, over the %d byte limit for plugin reads", path, st.Size(), maxPluginReadBytes)
	}

	return io.ReadAll(io.LimitReader(f, maxPluginReadBytes))
}

func (b *Bridge) FileWrite(uuid, path string, content []byte) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	// Goes through the same Write the file manager uses, so the server's disk
	// limit and ownership are applied to a plugin's write exactly as they are
	// to a user's.
	return s.Filesystem().Write(path, bytes.NewReader(content), int64(len(content)), 0o644)
}

func (b *Bridge) FileDelete(uuid, path string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	return s.Filesystem().Delete(path)
}

func (b *Bridge) FileRename(uuid, from, to string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	return s.Filesystem().Rename(from, to)
}

func (b *Bridge) FileCreateDirectory(uuid, path string) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}

	path = strings.TrimSuffix(path, "/")
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return s.Filesystem().CreateDirectory(path, "/")
	}
	return s.Filesystem().CreateDirectory(path[idx+1:], path[:idx])
}

func (b *Bridge) FileChmod(uuid, path string, mode uint32) error {
	s, err := b.find(uuid)
	if err != nil {
		return err
	}
	return s.Filesystem().Chmod(path, ufs.FileMode(mode))
}

func (b *Bridge) FileList(uuid, path string) ([]api.DirEntry, error) {
	s, err := b.find(uuid)
	if err != nil {
		return nil, err
	}

	stats, err := s.Filesystem().ListDirectory(path)
	if err != nil {
		return nil, err
	}

	out := make([]api.DirEntry, 0, len(stats))
	for _, st := range stats {
		out = append(out, api.DirEntry{
			Name:         st.Name(),
			Size:         st.Size(),
			Mode:         uint32(st.Mode().Perm()),
			Directory:    st.IsDir(),
			Symlink:      st.Mode()&ufs.ModeSymlink != 0,
			MimeType:     st.Mimetype,
			ModifiedUnix: st.ModTime().Unix(),
		})
	}
	return out, nil
}

func (b *Bridge) FileExists(uuid, path string) (bool, error) {
	s, err := b.find(uuid)
	if err != nil {
		return false, err
	}

	if _, err := s.Filesystem().UnixFS().Stat(path); err != nil {
		// A path that is not there is the answer, not a failure. Anything else
		// is a real problem the plugin should hear about rather than read as
		// "the file is missing".
		if errors.Is(err, ufs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// --- panel ---

func (b *Bridge) PanelRequest(method, path string, body []byte, header map[string]string) (int, []byte, error) {
	cfg := config.Get()

	url := strings.TrimSuffix(cfg.PanelLocation, "/") + path

	var reader io.Reader
	if len(body) > 0 {
		reader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(context.Background(), strings.ToUpper(method), url, reader)
	if err != nil {
		return 0, nil, errors.Wrap(err, "pluginbridge: could not build the Panel request")
	}

	for k, v := range header {
		req.Header.Set(k, v)
	}

	// The node's own credentials, set last so a plugin cannot replace them
	// with a header of its own and call the Panel as something else.
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s.%s", cfg.AuthenticationTokenId, cfg.AuthenticationToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", fmt.Sprintf("Pelican Wings/v%s (id:%s)", system.Version, cfg.AuthenticationTokenId))
	if req.Header.Get("Content-Type") == "" && len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := b.http.Do(req)
	if err != nil {
		return 0, nil, errors.Wrap(err, "pluginbridge: the Panel request failed")
	}
	defer res.Body.Close()

	content, err := io.ReadAll(io.LimitReader(res.Body, maxPluginReadBytes))
	if err != nil {
		return res.StatusCode, nil, errors.Wrap(err, "pluginbridge: could not read the Panel response")
	}

	return res.StatusCode, content, nil
}

// ToAPIMounts converts environment mounts for the container hook.
func ToAPIMounts(mounts []environment.Mount) []api.Mount {
	out := make([]api.Mount, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, api.Mount{
			Source:   m.Source,
			Target:   m.Target,
			ReadOnly: m.ReadOnly,
			Default:  m.Default,
		})
	}
	return out
}

// execEnvironment is the part of a process environment that can run a command
// inside it.
//
// Declared here rather than added to environment.ProcessEnvironment so that
// anything else implementing that interface, including the test fakes, does
// not have to grow a method it has no use for. An environment that cannot exec
// simply says so.
type execEnvironment interface {
	Exec(ctx context.Context, cmd []string, opts docker.ExecOptions) (docker.ExecResult, error)
}

// Exec runs a command inside a server's container.
func (b *Bridge) Exec(uuid string, command []string, opts api.ExecOptions) (api.ExecResult, error) {
	s, err := b.find(uuid)
	if err != nil {
		return api.ExecResult{}, err
	}

	env, ok := s.Environment.(execEnvironment)
	if !ok {
		return api.ExecResult{}, errors.New("pluginbridge: this server's environment cannot run commands")
	}

	timeout := time.Duration(opts.TimeoutSeconds) * time.Second
	if opts.TimeoutSeconds <= 0 {
		timeout = 30 * time.Second
	}

	result, err := env.Exec(s.Context(), command, docker.ExecOptions{
		Timeout:        timeout,
		MaxOutputBytes: opts.MaxOutputBytes,
		WorkingDir:     opts.WorkingDir,
	})

	out := api.ExecResult{
		Stdout:    result.Stdout,
		Stderr:    result.Stderr,
		ExitCode:  result.ExitCode,
		Truncated: result.Truncated,
	}
	return out, err
}

// FileStat describes one path on a server.
func (b *Bridge) FileStat(uuid, path string) (api.DirEntry, error) {
	s, err := b.find(uuid)
	if err != nil {
		return api.DirEntry{}, err
	}

	st, err := s.Filesystem().Stat(path)
	if err != nil {
		return api.DirEntry{}, err
	}

	return api.DirEntry{
		Name:         st.Name(),
		Size:         st.Size(),
		Mode:         uint32(st.Mode().Perm()),
		Directory:    st.IsDir(),
		Symlink:      st.Mode()&ufs.ModeSymlink != 0,
		MimeType:     st.Mimetype,
		ModifiedUnix: st.ModTime().Unix(),
	}, nil
}

// FileCopy duplicates a file or directory beside itself, returning where the
// copy landed.
//
// Wings picks the name, appending a suffix the way the file manager's copy
// does, so two plugins copying the same file cannot collide.
func (b *Bridge) FileCopy(uuid, path string) (string, error) {
	s, err := b.find(uuid)
	if err != nil {
		return "", err
	}

	before, err := siblingNames(s, path)
	if err != nil {
		return "", err
	}

	if err := s.Filesystem().Copy(path); err != nil {
		return "", err
	}

	// Wings does not report the name it chose, so it is found by diffing the
	// directory. Doing it here means a plugin is not left guessing at the
	// suffix rule.
	after, err := siblingNames(s, path)
	if err != nil {
		return "", err
	}

	dir := gopath.Dir(path)
	for name := range after {
		if !before[name] {
			return gopath.Join(dir, name), nil
		}
	}

	return "", errors.New("pluginbridge: the copy was made but could not be located")
}

// siblingNames lists the entry names in a path's directory.
func siblingNames(s *server.Server, path string) (map[string]bool, error) {
	entries, err := s.Filesystem().ListDirectory(gopath.Dir(path))
	if err != nil {
		return nil, err
	}

	out := make(map[string]bool, len(entries))
	for _, e := range entries {
		out[e.Name()] = true
	}
	return out, nil
}

// FileCompress packs entries of a directory into a new archive in that same
// directory, and returns the archive's path.
func (b *Bridge) FileCompress(uuid, directory string, names []string, opts api.CompressOptions) (string, error) {
	s, err := b.find(uuid)
	if err != nil {
		return "", err
	}

	if directory == "" {
		directory = "/"
	}

	// Wings owns naming and format selection, including falling back to a
	// gzipped tar for anything it does not recognise, so both are passed
	// straight through rather than second-guessed here.
	info, _, err := s.Filesystem().CompressFiles(directory, opts.Name, names, opts.Format)
	if err != nil {
		return "", err
	}

	return gopath.Join(directory, info.Name()), nil
}

// FileSpace reports a server's disk usage against its limit.
func (b *Bridge) FileSpace(uuid string) (api.DiskSpace, error) {
	s, err := b.find(uuid)
	if err != nil {
		return api.DiskSpace{}, err
	}

	used, err := s.Filesystem().DiskUsage(false)
	if err != nil {
		return api.DiskSpace{}, err
	}

	return api.DiskSpace{
		UsedBytes:  used,
		LimitBytes: s.Filesystem().MaxDisk(),
	}, nil
}

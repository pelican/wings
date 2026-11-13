package plugins

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"

	"github.com/pelican/wings/plugins/api"
)

// host is the api.Host handed to one plugin. Everything it exposes is scoped to
// that plugin: its own log fields, its own settings, its own storage rows.
type host struct {
	instance *Instance
	bridge   Bridge
}

var _ api.Host = (*host)(nil)

func (h *host) Logger() api.Logger {
	return &logger{
		entry: log.WithField("subsystem", "plugin").WithField("plugin", h.instance.ID()),
	}
}

func (h *host) Node() api.Node { return h.bridge.Node() }

func (h *host) Settings() api.Settings { return &settings{instance: h.instance} }

func (h *host) Store() api.Store { return h.instance.store }

func (h *host) Servers() api.Servers { return &servers{host: h} }

func (h *host) Files() api.Files { return &files{host: h} }

func (h *host) Panel() api.Panel { return &panel{host: h} }

func (h *host) HTTP() api.Fetcher { return h.instance.fetcher }

func (h *host) Events() api.Events { return &busEvents{host: h} }

// --- logging ---

type logger struct {
	entry *log.Entry
}

var _ api.Logger = (*logger)(nil)

func (l *logger) Debug(msg string) { l.entry.Debug(msg) }
func (l *logger) Info(msg string)  { l.entry.Info(msg) }
func (l *logger) Warn(msg string)  { l.entry.Warn(msg) }
func (l *logger) Error(msg string) { l.entry.Error(msg) }

func (l *logger) With(fields map[string]any) api.Logger {
	f := make(log.Fields, len(fields))
	for k, v := range fields {
		f[k] = v
	}
	return &logger{entry: l.entry.WithFields(f)}
}

// --- settings ---

// settings reads through to the instance's manifest every time rather than
// caching, so a plugin that holds onto its api.Settings across a settings
// change sees the new values without having to re-read anything.
type settings struct {
	instance *Instance
}

var _ api.Settings = (*settings)(nil)

func (s *settings) All() map[string]any { return s.instance.settingsSnapshot() }

func (s *settings) value(key string) (any, bool) {
	v, ok := s.instance.settingValue(key)
	return v, ok
}

func (s *settings) String(key, fallback string) string {
	v, ok := s.value(key)
	if !ok || v == nil {
		return fallback
	}
	if str, ok := v.(string); ok {
		if str == "" {
			return fallback
		}
		return str
	}
	// A setting an operator typed into a number or boolean field arrives as
	// the corresponding JSON type. Rendering it is more useful than pretending
	// the setting is unset.
	return fmt.Sprint(v)
}

func (s *settings) Int(key string, fallback int) int {
	v, ok := s.value(key)
	if !ok || v == nil {
		return fallback
	}
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		// JSON numbers decode as float64, which is how every number in a
		// manifest arrives.
		return int(n)
	case string:
		if parsed, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
			return parsed
		}
	}
	return fallback
}

func (s *settings) Bool(key string, fallback bool) bool {
	v, ok := s.value(key)
	if !ok || v == nil {
		return fallback
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		if parsed, err := strconv.ParseBool(strings.TrimSpace(b)); err == nil {
			return parsed
		}
	case float64:
		return b != 0
	}
	return fallback
}

func (s *settings) Duration(key string, fallback time.Duration) time.Duration {
	v, ok := s.value(key)
	if !ok || v == nil {
		return fallback
	}
	switch d := v.(type) {
	case string:
		if parsed, err := time.ParseDuration(strings.TrimSpace(d)); err == nil {
			return parsed
		}
	case float64:
		// A bare number in a duration setting is read as seconds, which is
		// what an operator typing "30" into a number field meant.
		return time.Duration(d) * time.Second
	}
	return fallback
}

func (s *settings) StringSlice(key string) []string {
	v, ok := s.value(key)
	if !ok || v == nil {
		return nil
	}
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, item := range l {
			out = append(out, fmt.Sprint(item))
		}
		return out
	case string:
		// Accept a comma separated string too, since that is what a plain text
		// field gives us.
		if strings.TrimSpace(l) == "" {
			return nil
		}
		parts := strings.Split(l, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				out = append(out, trimmed)
			}
		}
		return out
	}
	return nil
}

// --- servers ---

type servers struct {
	host *host
}

var _ api.Servers = (*servers)(nil)

func (s *servers) List() []api.Server { return s.host.bridge.ListServers() }

func (s *servers) Get(uuid string) (api.Server, bool) { return s.host.bridge.GetServer(uuid) }

func (s *servers) Power(uuid string, action api.PowerAction) error {
	if !validPowerAction(action) {
		return errors.Errorf("plugins: %q is not a power action", action)
	}
	return s.host.bridge.Power(uuid, action)
}

// validPowerAction reports whether the action is one Wings knows how to
// perform. A plugin passing something else is a bug in the plugin, so it gets
// an error rather than a silently ignored call.
func validPowerAction(action api.PowerAction) bool {
	switch action {
	case api.PowerStart, api.PowerStop, api.PowerRestart, api.PowerKill:
		return true
	}
	return false
}

func (s *servers) SendCommand(uuid, command string) error {
	return s.host.bridge.SendCommand(uuid, command)
}

func (s *servers) ConsoleWrite(uuid, line string) error {
	return s.host.bridge.ConsoleWrite(uuid, line)
}

// maxReadLogLines caps how much console history a plugin can pull at once. The
// lines are held in memory while being assembled, and this is the same ceiling
// the diagnostics endpoint uses.
const maxReadLogLines = 500

func (s *servers) ReadLog(uuid string, lines int) ([]string, error) {
	if lines <= 0 {
		return nil, nil
	}
	if lines > maxReadLogLines {
		lines = maxReadLogLines
	}
	return s.host.bridge.ReadLog(uuid, lines)
}

func (s *servers) Exec(uuid string, command []string, opts api.ExecOptions) (api.ExecResult, error) {
	if len(command) == 0 {
		return api.ExecResult{}, errors.New("plugins: no command was given to run")
	}
	return s.host.bridge.Exec(uuid, command, opts)
}

func (s *servers) SaveActivity(uuid, event string, meta map[string]any) error {
	if strings.TrimSpace(event) == "" {
		return errors.New("plugins: activity event cannot be empty")
	}
	return s.host.bridge.SaveActivity(uuid, s.host.instance.ID(), event, meta)
}

// --- files ---

type files struct {
	host *host
}

var _ api.Files = (*files)(nil)

func (f *files) Read(uuid, path string) ([]byte, error) {
	return f.host.bridge.FileRead(uuid, path)
}

func (f *files) Write(uuid, path string, content []byte) error {
	return f.host.bridge.FileWrite(uuid, path, content)
}

func (f *files) Delete(uuid, path string) error {
	return f.host.bridge.FileDelete(uuid, path)
}

func (f *files) Rename(uuid, from, to string) error {
	return f.host.bridge.FileRename(uuid, from, to)
}

func (f *files) CreateDirectory(uuid, path string) error {
	return f.host.bridge.FileCreateDirectory(uuid, path)
}

func (f *files) Chmod(uuid, path string, mode uint32) error {
	return f.host.bridge.FileChmod(uuid, path, mode)
}

func (f *files) List(uuid, path string) ([]api.DirEntry, error) {
	return f.host.bridge.FileList(uuid, path)
}

func (f *files) Exists(uuid, path string) (bool, error) {
	return f.host.bridge.FileExists(uuid, path)
}

func (f *files) Stat(uuid, path string) (api.DirEntry, error) {
	return f.host.bridge.FileStat(uuid, path)
}

func (f *files) Copy(uuid, path string) (string, error) {
	return f.host.bridge.FileCopy(uuid, path)
}

func (f *files) Compress(uuid, directory string, names []string, opts api.CompressOptions) (string, error) {
	if len(names) == 0 {
		return "", errors.New("plugins: no entries were named to compress")
	}
	return f.host.bridge.FileCompress(uuid, directory, names, opts)
}

func (f *files) Space(uuid string) (api.DiskSpace, error) {
	return f.host.bridge.FileSpace(uuid)
}

func (f *files) Download(uuid string, req api.DownloadRequest) (api.DownloadResult, error) {
	return f.host.bridge.FileDownload(uuid, req)
}

func (f *files) DownloadMany(uuid string, reqs []api.DownloadRequest, concurrency int) ([]api.DownloadResult, error) {
	if len(reqs) == 0 {
		return nil, nil
	}
	return f.host.bridge.FileDownloadMany(uuid, reqs, concurrency)
}

func (f *files) Decompress(uuid, archive string) error {
	return f.host.bridge.FileDecompress(uuid, archive)
}

func (f *files) Digest(uuid, path string, requests []api.DigestRequest) (map[string]string, error) {
	if len(requests) == 0 {
		return nil, errors.New("plugins: no digests were requested")
	}
	return f.host.bridge.FileDigest(uuid, path, requests)
}

// --- panel ---

type panel struct {
	host *host
}

var _ api.Panel = (*panel)(nil)

func (p *panel) Request(method, path string, body []byte, header map[string]string) (int, []byte, error) {
	if !strings.HasPrefix(path, "/") {
		return 0, nil, errors.New("plugins: panel request path must begin with a slash")
	}
	return p.host.bridge.PanelRequest(method, path, body, header)
}

// --- events ---

type busEvents struct {
	host *host
}

var _ api.Events = (*busEvents)(nil)

// Publish pushes an event onto a server's bus under a topic namespaced to the
// plugin.
//
// The namespacing is applied here rather than trusted to the plugin, because
// the bus is what drives the Panel's console and status views: a plugin
// publishing "status" would make every watching browser believe the server had
// changed state. Prefixing with "<plugin-id>." is enough to make that
// impossible, since no Wings topic contains a dot.
func (e *busEvents) Publish(uuid, topic string, data any) error {
	topic = strings.TrimSpace(topic)
	if topic == "" {
		return errors.New("plugins: event topic cannot be empty")
	}

	prefix := e.host.instance.ID() + "."
	if !strings.HasPrefix(topic, prefix) {
		topic = prefix + topic
	}

	return e.host.bridge.PublishEvent(uuid, topic, data)
}

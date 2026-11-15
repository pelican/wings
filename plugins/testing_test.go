package plugins

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"emperror.dev/errors"
	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/internal/models"
	"github.com/pelican/wings/plugins/api"
)

// fakeBridge stands in for the rest of Wings. Files live in a map rather than
// on disk so a test can assert on exactly what a plugin did, and so a plugin
// that tries to escape a server's directory fails here the way it would in the
// real bridge.
type fakeBridge struct {
	mu sync.Mutex

	servers map[string]api.Server

	// files maps server uuid to a path/content map. Directories are recorded
	// as entries with a nil value, which is enough to make Exists and List
	// behave.
	files map[string]map[string][]byte

	// calls records what the plugin asked the node to do, in order, so a test
	// can assert on the sequence rather than only the end state.
	calls []string

	powered  []string
	commands []string
	execs    []string

	// execResult and execErr let a test decide what a container "returns".
	execResult *api.ExecResult
	execErr    error

	downloads []string

	// diskLimit is what FileSpace reports as the ceiling. Zero means the
	// server is unlimited, which is a case plugins have to handle.
	diskLimit int64

	// downloadErrors makes a named URL fail, for testing a partial batch.
	downloadErrors map[string]string

	// archives says what a given archive expands to, keyed by the archive's
	// path and then by the name of each file inside it.
	archives map[string]map[string][]byte

	console   []string
	activity  []string
	published []string
}

func newFakeBridge() *fakeBridge {
	return &fakeBridge{
		servers:        map[string]api.Server{},
		files:          map[string]map[string][]byte{},
		downloadErrors: map[string]string{},
		archives:       map[string]map[string][]byte{},
	}
}

func (b *fakeBridge) addServer(s api.Server) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.servers[s.UUID] = s
	if _, ok := b.files[s.UUID]; !ok {
		b.files[s.UUID] = map[string][]byte{}
	}
}

func (b *fakeBridge) putFile(uuid, p string, content []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.files[uuid] == nil {
		b.files[uuid] = map[string][]byte{}
	}

	key := normalize(p)
	b.files[uuid][key] = content

	// Record the parent directories too, the way a real filesystem has them.
	// Without this, writing /mods/a.jar left /mods non-existent, so listing it
	// failed and a plugin that walks directories saw nothing.
	parts := strings.Split(strings.Trim(key, "/"), "/")
	for i := 1; i < len(parts); i++ {
		dir := "/" + strings.Join(parts[:i], "/")
		if _, exists := b.files[uuid][dir]; !exists {
			b.files[uuid][dir] = nil
		}
	}
}

func (b *fakeBridge) paths(uuid string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]string, 0, len(b.files[uuid]))
	for p, content := range b.files[uuid] {
		if content == nil {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

func (b *fakeBridge) record(call string) {
	b.mu.Lock()
	b.calls = append(b.calls, call)
	b.mu.Unlock()
}

// normalize gives every path a single leading slash and no trailing one, so
// "/a/b", "a/b" and "/a/b/" are one key.
func normalize(p string) string {
	p = path.Clean("/" + strings.TrimSpace(p))
	return p
}

func (b *fakeBridge) Node() api.Node {
	return api.Node{UUID: "node-1", Version: "1.0.0", OS: "linux", Architecture: "amd64"}
}

func (b *fakeBridge) ListServers() []api.Server {
	b.mu.Lock()
	defer b.mu.Unlock()

	out := make([]api.Server, 0, len(b.servers))
	for _, s := range b.servers {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UUID < out[j].UUID })
	return out
}

func (b *fakeBridge) GetServer(uuid string) (api.Server, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, ok := b.servers[uuid]
	return s, ok
}

func (b *fakeBridge) Power(uuid string, action api.PowerAction) error {
	b.record("power " + uuid + " " + string(action))
	b.mu.Lock()
	b.powered = append(b.powered, uuid+":"+string(action))
	b.mu.Unlock()
	return nil
}

func (b *fakeBridge) SendCommand(uuid, command string) error {
	b.record("command " + uuid + " " + command)
	b.mu.Lock()
	b.commands = append(b.commands, command)
	b.mu.Unlock()
	return nil
}

func (b *fakeBridge) ConsoleWrite(uuid, line string) error {
	b.mu.Lock()
	b.console = append(b.console, line)
	b.mu.Unlock()
	return nil
}

func (b *fakeBridge) ReadLog(uuid string, lines int) ([]string, error) {
	return []string{"log line"}, nil
}

// Exec records the command rather than running one, so a plugin under test can
// be checked on what it asked the container to do.
func (b *fakeBridge) Exec(uuid string, command []string, opts api.ExecOptions) (api.ExecResult, error) {
	b.record("exec " + uuid + " " + strings.Join(command, " "))

	b.mu.Lock()
	defer b.mu.Unlock()

	b.execs = append(b.execs, strings.Join(command, " "))

	if b.execResult != nil {
		return *b.execResult, b.execErr
	}
	return api.ExecResult{}, b.execErr
}

func (b *fakeBridge) SaveActivity(uuid, pluginID, event string, meta map[string]any) error {
	b.mu.Lock()
	b.activity = append(b.activity, pluginID+":"+event)
	b.mu.Unlock()
	return nil
}

func (b *fakeBridge) PublishEvent(uuid, topic string, data any) error {
	b.mu.Lock()
	b.published = append(b.published, topic)
	b.mu.Unlock()
	return nil
}

func (b *fakeBridge) FileRead(uuid, p string) ([]byte, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	content, ok := b.files[uuid][normalize(p)]
	if !ok || content == nil {
		return nil, os.ErrNotExist
	}
	return content, nil
}

func (b *fakeBridge) FileWrite(uuid, p string, content []byte) error {
	b.record("write " + p)
	b.putFile(uuid, p, content)
	return nil
}

func (b *fakeBridge) FileDelete(uuid, p string) error {
	b.record("delete " + p)

	b.mu.Lock()
	defer b.mu.Unlock()

	key := normalize(p)
	if _, ok := b.files[uuid][key]; !ok {
		return os.ErrNotExist
	}
	// Delete the entry and anything beneath it, the way a recursive delete
	// behaves.
	for existing := range b.files[uuid] {
		if existing == key || strings.HasPrefix(existing, key+"/") {
			delete(b.files[uuid], existing)
		}
	}
	return nil
}

func (b *fakeBridge) FileRename(uuid, from, to string) error {
	b.record("rename " + from + " -> " + to)

	b.mu.Lock()
	defer b.mu.Unlock()

	src, dst := normalize(from), normalize(to)
	content, ok := b.files[uuid][src]
	if !ok {
		return os.ErrNotExist
	}
	if _, taken := b.files[uuid][dst]; taken {
		return os.ErrExist
	}

	delete(b.files[uuid], src)
	b.files[uuid][dst] = content
	return nil
}

func (b *fakeBridge) FileCreateDirectory(uuid, p string) error {
	b.record("mkdir " + p)

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.files[uuid] == nil {
		b.files[uuid] = map[string][]byte{}
	}
	// Record every ancestor, matching the recursive create the real
	// filesystem performs.
	parts := strings.Split(strings.Trim(normalize(p), "/"), "/")
	for i := range parts {
		b.files[uuid]["/"+strings.Join(parts[:i+1], "/")] = nil
	}
	return nil
}

func (b *fakeBridge) FileChmod(uuid, p string, mode uint32) error { return nil }

func (b *fakeBridge) FileList(uuid, p string) ([]api.DirEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	dir := normalize(p)
	if _, ok := b.files[uuid][dir]; !ok && dir != "/" {
		return nil, os.ErrNotExist
	}

	seen := map[string]bool{}
	var out []api.DirEntry

	prefix := dir
	if prefix != "/" {
		prefix += "/"
	}

	for existing, content := range b.files[uuid] {
		if existing == dir || !strings.HasPrefix(existing, prefix) {
			continue
		}
		name := strings.SplitN(strings.TrimPrefix(existing, prefix), "/", 2)[0]
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true

		out = append(out, api.DirEntry{
			Name:      name,
			Size:      int64(len(content)),
			Directory: content == nil,
		})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (b *fakeBridge) FileExists(uuid, p string) (bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.files[uuid][normalize(p)]
	return ok, nil
}

func (b *fakeBridge) FileStat(uuid, p string) (api.DirEntry, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	content, ok := b.files[uuid][normalize(p)]
	if !ok {
		return api.DirEntry{}, os.ErrNotExist
	}

	return api.DirEntry{
		Name:      path.Base(normalize(p)),
		Size:      int64(len(content)),
		Directory: content == nil,
	}, nil
}

func (b *fakeBridge) FileCopy(uuid, p string) (string, error) {
	b.record("copy " + p)

	content, err := b.FileRead(uuid, p)
	if err != nil {
		return "", err
	}

	// Mirror the real one's contract: Wings picks a free name beside the
	// original and reports where it went.
	dir, base := path.Dir(normalize(p)), path.Base(normalize(p))
	ext := path.Ext(base)
	target := path.Join(dir, strings.TrimSuffix(base, ext)+" copy"+ext)

	b.putFile(uuid, target, content)
	return target, nil
}

func (b *fakeBridge) FileCompress(uuid, directory string, names []string, opts api.CompressOptions) (string, error) {
	b.record("compress " + directory + " " + strings.Join(names, ","))

	if len(names) == 0 {
		return "", errors.New("fake bridge: nothing to compress")
	}

	name := opts.Name
	if name == "" {
		name = "archive"
	}

	ext := ".tar.gz"
	switch opts.Format {
	case "zip":
		ext = ".zip"
	case "tar.bz2":
		ext = ".tar.bz2"
	case "tar.xz":
		ext = ".tar.xz"
	}

	target := path.Join(directory, name+ext)
	b.putFile(uuid, target, []byte("archive of "+strings.Join(names, ",")))
	return target, nil
}

func (b *fakeBridge) FileSpace(uuid string) (api.DiskSpace, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var used int64
	for _, content := range b.files[uuid] {
		used += int64(len(content))
	}

	return api.DiskSpace{UsedBytes: used, LimitBytes: b.diskLimit}, nil
}

// FileDownload records the request and writes a stand-in file, so a plugin
// under test can be checked on what it asked the node to fetch without this
// reaching the network.
func (b *fakeBridge) FileDownload(uuid string, req api.DownloadRequest) (api.DownloadResult, error) {
	b.record("download " + req.URL)

	b.mu.Lock()
	b.downloads = append(b.downloads, req.URL)
	failure := b.downloadErrors[req.URL]
	b.mu.Unlock()

	if failure != "" {
		return api.DownloadResult{URL: req.URL}, errors.New(failure)
	}

	name := req.FileName
	if name == "" {
		name = path.Base(req.URL)
	}

	dir := req.Directory
	if dir == "" {
		dir = "/"
	}

	full := path.Join(dir, name)
	body := []byte("downloaded:" + req.URL)
	b.putFile(uuid, full, body)

	return api.DownloadResult{URL: req.URL, Path: full, Bytes: int64(len(body))}, nil
}

// FileDownloadMany mirrors the real one's contract: a result per request, in
// order, each carrying its own error so one failure does not sink the batch.
func (b *fakeBridge) FileDownloadMany(uuid string, reqs []api.DownloadRequest, concurrency int) ([]api.DownloadResult, error) {
	results := make([]api.DownloadResult, len(reqs))

	for i, req := range reqs {
		result, err := b.FileDownload(uuid, req)
		if err != nil {
			result.URL = req.URL
			result.Error = err.Error()
		}
		results[i] = result
	}
	return results, nil
}

func (b *fakeBridge) FileDecompress(uuid, archive string) error {
	b.record("decompress " + archive)

	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.files[uuid][normalize(archive)]; !ok {
		return os.ErrNotExist
	}

	// Stand in for extraction by writing whatever the test said the archive
	// contains, beside it, which is where Wings extracts.
	dir := path.Dir(normalize(archive))
	for name, body := range b.archives[normalize(archive)] {
		b.files[uuid][normalize(path.Join(dir, name))] = body
	}
	return nil
}

// FileDigest mirrors what the real bridge computes natively, so a plugin under
// test exercises the same contract.
func (b *fakeBridge) FileDigest(uuid, p string, requests []api.DigestRequest) (map[string]string, error) {
	content, err := b.FileRead(uuid, p)
	if err != nil {
		return nil, err
	}

	out := map[string]string{}
	for _, req := range requests {
		name := strings.TrimSpace(req.Name)
		algorithm := strings.ToLower(strings.TrimSpace(req.Algorithm))
		if name == "" {
			name = algorithm
		}

		data := content
		if req.IgnoreWhitespace {
			data = stripWhitespace(content)
		}

		switch algorithm {
		case "sha1":
			sum := sha1.Sum(data)
			out[name] = hex.EncodeToString(sum[:])
		case "sha256":
			sum := sha256.Sum256(data)
			out[name] = hex.EncodeToString(sum[:])
		case "sha512":
			sum := sha512.Sum512(data)
			out[name] = hex.EncodeToString(sum[:])
		case "md5":
			sum := md5.Sum(data)
			out[name] = hex.EncodeToString(sum[:])
		case "crc32":
			out[name] = strconv.FormatUint(uint64(crc32.ChecksumIEEE(data)), 10)
		case "murmur2":
			out[name] = strconv.FormatUint(uint64(fakeMurmur2(data, req.Seed)), 10)
		default:
			return nil, errors.Errorf("fake bridge: unknown digest %q", req.Algorithm)
		}
	}
	return out, nil
}

func stripWhitespace(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for _, b := range data {
		switch b {
		case 0x09, 0x0a, 0x0d, 0x20:
			continue
		}
		out = append(out, b)
	}
	return out
}

// fakeMurmur2 is an independent, obvious implementation. The production one
// streams and carries a tail across chunk boundaries, so having the harness
// compute it the simple way means the two have to agree.
func fakeMurmur2(data []byte, seed uint32) uint32 {
	const m uint32 = 0x5bd1e995
	const r = 24

	length := len(data)
	h := seed ^ uint32(length)

	i := 0
	for length >= 4 {
		k := uint32(data[i]) | uint32(data[i+1])<<8 | uint32(data[i+2])<<16 | uint32(data[i+3])<<24
		k *= m
		k ^= k >> r
		k *= m
		h *= m
		h ^= k
		i += 4
		length -= 4
	}

	switch length {
	case 3:
		h ^= uint32(data[i+2]) << 16
		fallthrough
	case 2:
		h ^= uint32(data[i+1]) << 8
		fallthrough
	case 1:
		h ^= uint32(data[i])
		h *= m
	}

	h ^= h >> 13
	h *= m
	h ^= h >> 15

	return h
}

func (b *fakeBridge) PanelRequest(method, p string, body []byte, header map[string]string) (int, []byte, error) {
	b.record("panel " + method + " " + p)
	return 200, []byte(`{}`), nil
}

var _ Bridge = (*fakeBridge)(nil)

// testDB returns an in-memory database with the plugin tables migrated.
func testDB(t *testing.T) *gorm.DB {
	t.Helper()

	// A database per test. "file::memory:" with a shared cache is one database
	// for the whole process, so without a distinct name every test would see
	// the plugin store rows written by the ones before it, and assertions on
	// what a plugin has stored would depend on test order.
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_pragma=foreign_keys(1)",
		strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))

	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("could not open the test database: %v", err)
	}
	if err := db.AutoMigrate(&models.PluginStore{}); err != nil {
		t.Fatalf("could not migrate the test database: %v", err)
	}
	// One connection, so the shared in-memory database is not torn down
	// between statements.
	if sql, err := db.DB(); err == nil {
		sql.SetMaxOpenConns(1)
	}
	return db
}

// writePlugin puts a plugin on disk in dir and returns the plugin directory.
func writePlugin(t *testing.T, root, id, manifest, source string) string {
	t.Helper()

	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("could not create the plugin directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestName), []byte(manifest), 0o644); err != nil {
		t.Fatalf("could not write plugin.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, id+".go"), []byte(source), 0o644); err != nil {
		t.Fatalf("could not write the plugin source: %v", err)
	}
	return dir
}

// newTestManager builds a manager over a fresh temporary plugin directory.
func newTestManager(t *testing.T, bridge Bridge) (*Manager, string) {
	t.Helper()

	root := t.TempDir()

	// These mirror the defaults the struct tags give a real node. Building the
	// struct literally skips defaults.Set, so anything left out here would be
	// a zero value rather than the documented default, and a test would be
	// exercising a configuration no node ever runs.
	m := NewManager(config.PluginConfiguration{
		Enabled:           true,
		Directory:         root,
		GateTimeout:       2,
		RequestTimeout:    5,
		MaxResponseSize:   1,
		MaxInstallSize:    4,
		AllowDependencies: true,
		ModuleProxy:       "https://proxy.golang.org",
		MaxDependencySize: 32,
		MaxDependencies:   50,
	}, bridge, testDB(t))

	// Dispatch reads a package level pointer, so a test that dispatches has
	// to install its manager and put it back afterwards.
	SetManager(m)
	t.Cleanup(func() { SetManager(nil) })

	return m, root
}

// testServer is the server every fixture plugin sees.
func testServer() api.Server {
	return api.Server{
		UUID:  "11111111-1111-1111-1111-111111111111",
		ID:    1,
		Name:  "Survival",
		State: api.StateRunning,
		Image: "ghcr.io/pelican-eggs/java:21",
		EggID: "egg-1",
	}
}

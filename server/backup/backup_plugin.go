package backup

import (
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"emperror.dev/errors"

	"github.com/pelican/wings/plugins"
	"github.com/pelican/wings/plugins/api"
	"github.com/pelican/wings/remote"
	"github.com/pelican/wings/server/filesystem"
)

// PluginBackup stores a backup through an adapter a plugin registered.
//
// Wings keeps ownership of the parts that must not vary between adapters:
// walking the server's files, honouring the ignore list, producing the archive
// and reporting the result to the Panel. The plugin is handed a finished
// archive and asked only where to put it. That way an adapter for some object
// store is a few lines of plugin code rather than a reimplementation of
// backups, and a bug in one cannot produce an archive the Panel misreads.
type PluginBackup struct {
	Backup

	// adapterName is the name the plugin registered, which is also what the
	// Panel asks for.
	adapterName string
}

var _ BackupInterface = (*PluginBackup)(nil)

// NewPlugin returns a backup backed by a plugin adapter.
func NewPlugin(client remote.Client, adapter, uuid, suuid, ignore string) *PluginBackup {
	return &PluginBackup{
		Backup: Backup{
			client:     client,
			Uuid:       uuid,
			ServerUuid: suuid,
			Ignore:     ignore,
			adapter:    AdapterType(adapter),
		},
		adapterName: adapter,
	}
}

// PluginAdapterExists reports whether a plugin currently provides an adapter
// under this name, which is how the backup endpoint decides between a built-in
// adapter, a plugin one, and an unknown one.
func PluginAdapterExists(name string) bool {
	m := plugins.Active()
	if m == nil || !m.Enabled() {
		return false
	}
	_, _, ok := m.BackupAdapter(name)
	return ok
}

// resolve looks up the plugin and adapter backing this backup.
//
// It is resolved per call rather than held on the struct, because a backup can
// outlive the plugin being disabled, and continuing to write into an adapter
// whose plugin an operator has switched off would be wrong.
func (p *PluginBackup) resolve() (*plugins.Instance, *api.BackupAdapter, error) {
	m := plugins.Active()
	if m == nil || !m.Enabled() {
		return nil, nil, errors.New("backup: the plugin subsystem is not available on this node")
	}

	inst, adapter, ok := m.BackupAdapter(p.adapterName)
	if !ok {
		return nil, nil, errors.Errorf("backup: no enabled plugin provides the %q backup adapter", p.adapterName)
	}
	return inst, adapter, nil
}

// WithLogContext attaches additional context to the log output for this backup.
func (p *PluginBackup) WithLogContext(c map[string]interface{}) {
	p.logContext = c
}

// backupDescriptor is what the plugin's hooks receive.
func (p *PluginBackup) backupDescriptor() api.Backup {
	descriptor := api.Backup{
		UUID:   p.Uuid,
		Ignore: p.Ignore,
	}
	if s, ok := plugins.ServerSnapshot(p.ServerUuid); ok {
		descriptor.Server = s
	} else {
		// A backup can be taken for a server the manager cannot resolve during
		// a transfer. The uuid is the part an adapter actually needs, so it is
		// filled in rather than handing over an empty server.
		descriptor.Server = api.Server{UUID: p.ServerUuid}
	}
	return descriptor
}

// Generate builds the archive locally and hands it to the plugin to store.
func (p *PluginBackup) Generate(ctx context.Context, fsys *filesystem.Filesystem, ignore string) (*ArchiveDetails, error) {
	inst, adapter, err := p.resolve()
	if err != nil {
		return nil, err
	}

	if err := p.validateIdentifier(); err != nil {
		return nil, err
	}

	// The local archive is scratch space: the plugin is responsible for where
	// the backup really lives, so this copy goes away either way.
	defer p.Remove()

	a := &filesystem.Archive{
		Filesystem: fsys,
		Ignore:     ignore,
	}

	p.log().WithField("path", p.Path()).Info("creating backup for server")

	if _, err := os.Stat(filepath.Dir(p.Path())); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(p.Path()), 0o700); err != nil {
			return nil, err
		}
	}
	if err := a.Create(ctx, p.Path()); err != nil {
		return nil, err
	}

	size, err := p.Size()
	if err != nil {
		return nil, err
	}

	checksum, err := p.Checksum()
	if err != nil {
		return nil, err
	}

	payload := api.BackupPayload{
		Path:      p.Path(),
		SizeBytes: size,
		Checksum:  hex.EncodeToString(checksum),
	}

	var (
		result    api.BackupResult
		createErr error
	)

	// No deadline: storing a backup means uploading potentially many gigabytes,
	// and the request that started this has already been answered with a 202.
	if !inst.InvokeRoute(func() {
		result, createErr = adapter.Create(p.backupDescriptor(), payload)
	}) {
		return nil, errors.Errorf("backup: the %q plugin adapter panicked while storing the backup", p.adapterName)
	}
	if createErr != nil {
		return nil, errors.Wrapf(createErr, "backup: the %q plugin adapter could not store the backup", p.adapterName)
	}

	p.log().Info("created backup successfully")

	// An adapter that recomputed the size or checksum is believed over what
	// Wings measured locally, since what it stored is what will be restored.
	if result.SizeBytes > 0 {
		size = result.SizeBytes
	}
	reportedChecksum := payload.Checksum
	if result.Checksum != "" {
		reportedChecksum = result.Checksum
	}
	checksumType := result.ChecksumType
	if checksumType == "" {
		checksumType = "sha1"
	}

	return &ArchiveDetails{
		Checksum:     reportedChecksum,
		ChecksumType: checksumType,
		Size:         size,
		Parts:        []remote.BackupPart{},
	}, nil
}

// Restore asks the plugin to put a backup's contents back.
//
// The reader Wings passes to other adapters is not used here: a plugin adapter
// knows where its own data is, so it is given a writer instead and streams
// files back through it, which keeps the whole archive off the node's disk.
func (p *PluginBackup) Restore(ctx context.Context, _ io.Reader, callback RestoreCallback) error {
	inst, adapter, err := p.resolve()
	if err != nil {
		return err
	}

	writer := &pluginRestoreWriter{ctx: ctx, callback: callback}

	var restoreErr error
	if !inst.InvokeRoute(func() {
		restoreErr = adapter.Restore(p.backupDescriptor(), writer)
	}) {
		return errors.Errorf("backup: the %q plugin adapter panicked while restoring", p.adapterName)
	}

	return restoreErr
}

// Remove deletes the local scratch archive, and asks the plugin to delete the
// stored backup.
//
// Deleting a backup that is already gone has to succeed, or a retried delete
// would leave the Panel unable to clear the record.
func (p *PluginBackup) Remove() error {
	if err := p.validateIdentifier(); err != nil {
		return err
	}

	// The local copy is removed whether or not the adapter is reachable, since
	// it is only ever scratch space.
	if err := os.Remove(p.Path()); err != nil && !os.IsNotExist(err) {
		p.log().WithField("error", err).Warn("could not remove the local backup archive")
	}

	inst, adapter, err := p.resolve()
	if err != nil {
		return err
	}

	var removeErr error
	if !inst.InvokeRoute(func() {
		removeErr = adapter.Delete(p.backupDescriptor())
	}) {
		return errors.Errorf("backup: the %q plugin adapter panicked while deleting", p.adapterName)
	}

	return removeErr
}

// Size reports the stored backup's size, preferring what the adapter says over
// the local scratch copy, which may already be gone.
func (p *PluginBackup) Size() (int64, error) {
	inst, adapter, err := p.resolve()
	if err == nil && adapter.Size != nil {
		var (
			size    int64
			sizeErr error
		)
		if inst.InvokeRoute(func() { size, sizeErr = adapter.Size(p.backupDescriptor()) }) && sizeErr == nil {
			return size, nil
		}
	}

	return p.Backup.Size()
}

// pluginRestoreWriter is the api.RestoreWriter handed to a plugin's Restore
// hook. It funnels every file back through the same callback Wings' own
// adapters use, so a plugin restore gets the identical path checks, ownership
// and disk accounting.
type pluginRestoreWriter struct {
	ctx      context.Context
	callback RestoreCallback
}

var _ api.RestoreWriter = (*pluginRestoreWriter)(nil)

func (w *pluginRestoreWriter) Write(path string, content []byte) error {
	// Respect cancellation so a restore stops when the server is deleted or
	// the daemon is shutting down, rather than writing files into a directory
	// that is going away.
	select {
	case <-w.ctx.Done():
		return w.ctx.Err()
	default:
	}

	return w.callback(path, &pluginRestoreFileInfo{name: path, size: int64(len(content))}, io.NopCloser(newBytesReader(content)))
}

func (w *pluginRestoreWriter) Progress(done, total int64) {
	// Progress is advisory. Wings reports restore progress from its own
	// accounting of bytes written, so this exists for adapters that want to
	// report it and costs nothing when they do not.
	_ = done
	_ = total
}

// pluginRestoreFileInfo is the fs.FileInfo a restore callback needs. A plugin
// hands back a path and its contents; everything else the callback consults is
// derived from those.
type pluginRestoreFileInfo struct {
	name string
	size int64
}

func (f *pluginRestoreFileInfo) Name() string { return filepath.Base(f.name) }
func (f *pluginRestoreFileInfo) Size() int64  { return f.size }

// Mode is a plain file with owner read/write, matching what Wings applies to
// files restored from its own archives.
func (f *pluginRestoreFileInfo) Mode() fs.FileMode  { return 0o644 }
func (f *pluginRestoreFileInfo) ModTime() time.Time { return time.Now() }
func (f *pluginRestoreFileInfo) IsDir() bool        { return false }
func (f *pluginRestoreFileInfo) Sys() any           { return nil }

func newBytesReader(b []byte) io.Reader { return bytes.NewReader(b) }

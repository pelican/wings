package config

import (
	"fmt"
	"os"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/creasty/defaults"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v2"
)

// The configuration file has more than one writer: the daemon (at boot and on
// a Panel push), `wings configure`, and `wings plugins enable/disable`, which
// run as separate processes while the daemon is up. Every write takes
// _writeLock (writers in this process) and lockFile (writers in other
// processes), and code that changes one part of the file re-reads the rest
// under those locks, so no writer puts back an old copy over another's change.
//
// The plugins block belongs to the node operator: only `wings plugins`
// commands, or the operator editing the file, change it. The daemon and
// `wings configure` keep whatever block is in the file.

// lockFile takes the cross-process lock for the configuration file at path,
// an flock on "<path>.lock", and returns the function that releases it.
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("config: opening lock file: %w", err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("config: locking configuration file: %w", err)
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// withFileLock runs fn holding both write locks for c's file.
func withFileLock(c *Configuration, fn func() error) error {
	_writeLock.Lock()
	defer _writeLock.Unlock()
	if c.path == "" {
		return errors.New("cannot write configuration, no path defined in struct")
	}
	unlock, err := lockFile(c.path)
	if err != nil {
		return err
	}
	defer unlock()
	return fn()
}

// writeFile writes c to its file. Callers hold the write locks.
func writeFile(c *Configuration) error {
	//goland:noinspection GoVetCopyLock
	ccopy := *c
	// If debugging is set with the flag, don't save that to the configuration file,
	// otherwise you'll always end up in debug mode.
	if _debugViaFlag {
		ccopy.Debug = false
	}
	b, err := yaml.Marshal(&ccopy)
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, b, 0o600)
}

// pluginsFromFile reads the plugins block of the configuration file at path.
// Fields the file does not set get their defaults, as at startup. It decodes
// into a fresh value: yaml.v2 merges into maps it is given, so starting from
// an existing block would keep registries or grants the file no longer has.
func pluginsFromFile(path string) (PluginConfiguration, error) {
	var fresh PluginConfiguration
	if err := defaults.Set(&fresh); err != nil {
		return fresh, err
	}
	current := struct {
		Plugins PluginConfiguration `yaml:"plugins"`
	}{Plugins: fresh}
	b, err := os.ReadFile(path)
	if err != nil {
		return fresh, err
	}
	if err := yaml.Unmarshal(b, &current); err != nil {
		return fresh, err
	}
	return current.Plugins, nil
}

// WriteToDisk writes the configuration to the disk, one writer at a time
// across processes. It writes c's plugins block as it is; the daemon and
// `wings configure` use WriteToDiskKeepingPlugins instead.
func WriteToDisk(c *Configuration) error {
	return withFileLock(c, func() error { return writeFile(c) })
}

// WriteToDiskKeepingPlugins writes c to its file, but with the plugins block
// the file has, read under the same lock: a `wings plugins` change made while
// c was in memory survives. c.Plugins is updated to match what was written.
// If the file does not exist yet c.Plugins is written; if it can't be read,
// that is logged and c.Plugins is written.
func WriteToDiskKeepingPlugins(c *Configuration) error {
	return withFileLock(c, func() error {
		p, err := pluginsFromFile(c.path)
		switch {
		case err == nil:
			c.Plugins = p
		case errors.Is(err, os.ErrNotExist):
		default:
			log.WithField("error", err).Warn("could not re-read the plugins block from the config file; writing the one in memory")
		}
		return writeFile(c)
	})
}

// UpdatePluginsOnDisk applies change to the plugins block in c's
// configuration file and returns the block it wrote. The whole file is read
// again under the write locks and written back with only that block changed,
// so concurrent `wings plugins` commands, or a daemon writing at the same
// time, don't undo each other. The running configuration's plugins block is
// set to the result under the same locks, so it can't be overtaken by an
// older copy (see ReloadPluginsFromDisk).
func UpdatePluginsOnDisk(c *Configuration, change func(*PluginConfiguration)) (PluginConfiguration, error) {
	var out PluginConfiguration
	err := withFileLock(c, func() error {
		fresh, err := NewAtPath(c.path)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(c.path)
		if err != nil {
			return err
		}
		if err := yaml.Unmarshal(b, fresh); err != nil {
			return err
		}
		change(&fresh.Plugins)
		out = fresh.Plugins
		if err := writeFile(fresh); err != nil {
			return err
		}
		setRunningPlugins(out)
		return nil
	})
	return out, err
}

// ReloadPluginsFromDisk reads the plugins block from c's configuration file
// and makes it the running configuration's block, both under the write
// locks. Every change to the running block made through this package happens
// together with the file read or write it belongs to, so the running block
// always ends up as the file's latest one; a reader that applied its copy after
// releasing the lock could put back an older block.
func ReloadPluginsFromDisk(c *Configuration) (PluginConfiguration, error) {
	var out PluginConfiguration
	err := withFileLock(c, func() error {
		p, err := pluginsFromFile(c.path)
		if err != nil {
			return err
		}
		out = p
		setRunningPlugins(p)
		return nil
	})
	return out, err
}

// setRunningPlugins sets the plugins block of the running configuration, if
// this process has one (Set was called).
func setRunningPlugins(p PluginConfiguration) {
	mu.Lock()
	defer mu.Unlock()
	if _config != nil {
		_config.Plugins = p
	}
}

// PluginsOnDisk returns the plugins block as it is in c's configuration file
// now. It reads under the write locks: writers truncate the file before
// writing it, and a read in between would see an empty block, which decodes
// to the defaults (autoload on, nothing disabled).
func PluginsOnDisk(c *Configuration) (PluginConfiguration, error) {
	var out PluginConfiguration
	err := withFileLock(c, func() error {
		p, err := pluginsFromFile(c.path)
		out = p
		return err
	})
	return out, err
}

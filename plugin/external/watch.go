package external

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/apex/log"
	"gopkg.in/yaml.v2"

	"github.com/pelican/wings/config"
	"github.com/pelican/wings/plugin"
)

// fileStamp is what WatchPolicy compares to notice a file changed.
type fileStamp struct {
	mod  time.Time
	size int64
	ok   bool
}

func stampOf(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), ok: true}
}

// approvals returns the trust store's approvals, or nil if it can't be read.
func approvals() map[string]string {
	ts, err := plugin.LoadTrustStore()
	if err != nil {
		return nil
	}
	return ts.Approved
}

// WatchPolicy keeps the running daemon in step with plugin changes other
// processes make: `wings plugins enable/disable/trust/untrust/add/update`,
// `wings plugins panel-access`, the operator editing config.yml, or anyone
// editing an installed plugin's files. Every interval it reads the plugins
// block from the configuration file at path into the running configuration,
// under the file lock (config.ReloadPluginsFromDisk), and looks at the
// plugins directory (modification time, which changes when a plugin is added
// or removed), the approvals in the trust store and the installed plugins'
// files (pluginFiles). When the block or any of those changed it runs
// plugin.PolicyChanged, whose reconcile stops a plugin whose files no longer
// match its approval.
// It returns when ctx is done.
//
// At the start it applies the block on disk and reconciles once, so a change
// made while the daemon was starting is not missed.
func WatchPolicy(ctx context.Context, path string, interval time.Duration) error {
	c, err := config.NewAtPath(path)
	if err != nil {
		return err
	}
	w := policyWatch{path: path, c: c}
	w.start()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			w.poll()
		}
	}
}

type policyWatch struct {
	path      string
	c         *config.Configuration // only its path is used, for the file lock
	dir       fileStamp
	approved  map[string]string
	block     config.PluginConfiguration // the block last read from the file
	haveBlock bool
	// quick and files fingerprint the installed plugins' files: quick their
	// top level and entrypoints, every poll; files every entry, every
	// deepScanEvery polls or when quick changed. See pluginFiles.
	quick, files string
	polls        int
}

func (w *policyWatch) start() {
	before := config.Get().Plugins
	p, err := config.ReloadPluginsFromDisk(w.c)
	if err != nil {
		log.WithField("error", err).Warn("could not read the plugins block from the config file; will retry")
		w.block = config.Get().Plugins
	} else {
		w.block, w.haveBlock = p, true
	}
	w.dir = stampOf(w.block.Directory)
	w.approved = approvals()
	w.quick = pluginFiles(w.block.Directory, false)
	w.files = pluginFiles(w.block.Directory, true)
	// Reconcile once whatever changed: a trust revocation, install or
	// removal between the daemon's first reconcile and here is already in
	// the baseline above, so no poll would ever see it.
	reason := "watcher started"
	if w.haveBlock && !samePolicy(w.block, before) {
		reason = "the plugins block on disk differs from the one in memory"
	}
	w.apply(reason)
}

func (w *policyWatch) poll() {
	var reasons []string
	// The block is read on every poll, not when the file's time or size
	// changes: the file is rewritten in place, Linux keeps file times to a
	// clock tick, and two writes within one tick can leave the same size.
	if p, err := config.ReloadPluginsFromDisk(w.c); err != nil {
		log.WithField("error", err).Warn("could not read the plugins block from the config file; will retry")
	} else {
		if !w.haveBlock || !samePolicy(p, w.block) {
			reasons = append(reasons, "config file")
		}
		w.block, w.haveBlock = p, true
	}
	if s := stampOf(w.block.Directory); s != w.dir {
		w.dir = s
		reasons = append(reasons, "plugins directory")
	}
	if a := approvals(); !maps.Equal(a, w.approved) {
		w.approved = a
		reasons = append(reasons, "trust store")
	}
	w.polls++
	quick := pluginFiles(w.block.Directory, false)
	if quick != w.quick || w.polls%deepScanEvery == 0 {
		w.quick = quick
		if files := pluginFiles(w.block.Directory, true); files != w.files {
			w.files = files
			reasons = append(reasons, "plugin files")
		}
	}
	if len(reasons) > 0 && w.haveBlock {
		w.apply(reasons...)
	}
}

func (w *policyWatch) apply(reasons ...string) {
	defer func() {
		if r := recover(); r != nil {
			log.WithField("panic", r).Error("applying the plugin policy panicked")
		}
	}()
	log.WithField("changed", reasons).Info("plugin policy changed on disk; applying it")
	plugin.PolicyChanged()
}

// samePolicy reports whether a and b would be written to the file the same
// way. A block decoded from YAML and one built in memory differ only in empty
// versus nil lists and maps, which is not a change.
func samePolicy(a, b config.PluginConfiguration) bool {
	ya, errA := yaml.Marshal(a)
	yb, errB := yaml.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ya, yb)
}

// deepScanEvery is how many polls pass between full scans of the plugins'
// files (every 12 polls is a minute at the daemon's 5 seconds). A variable so
// tests can shorten it.
var deepScanEvery = 12

// pluginFiles fingerprints the files of every plugin directory in dir (its
// non-hidden sub directories, as discovery sees them) from metadata alone:
// no file is read. Each entry adds its path, type and mode, size, modification
// and change time and inode, so an edit, a replacement by rename or a
// touched-back modification time all change it.
//
// The quick form (deep false) covers each plugin directory itself, its top
// level entries (the manifest and most entrypoints) and every entrypoint
// argument that names a file inside it, such as dist/index.js: about one
// stat per top level file. The deep form covers every entry in the tree.
// Neither proves the files are the approved ones; a change only makes the
// watcher reconcile, and Reconcile and the client check the content against
// the approval.
func pluginFiles(dir string, deep bool) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	h := sha256.New()
	add := func(rel string, fi fs.FileInfo) {
		fmt.Fprintf(h, "%s\x00%v\x00%d\x00%d\x00%d\x00%d\n", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano(), changeTime(fi), inode(fi))
	}
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		pd := filepath.Join(dir, e.Name())
		if deep {
			_ = filepath.WalkDir(pd, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					fmt.Fprintf(h, "%s\x00error\n", p)
					return nil
				}
				if fi, err := os.Lstat(p); err == nil {
					add(p, fi)
				}
				return nil
			})
			continue
		}
		if fi, err := os.Lstat(pd); err == nil {
			add(pd, fi)
		}
		top, err := os.ReadDir(pd)
		if err != nil {
			fmt.Fprintf(h, "%s\x00error\n", pd)
			continue
		}
		for _, t := range top {
			if fi, err := t.Info(); err == nil {
				add(filepath.Join(pd, t.Name()), fi)
			}
		}
		m, err := plugin.LoadManifest(filepath.Join(pd, plugin.ManifestFile))
		if err != nil {
			continue
		}
		for _, arg := range m.Entrypoint {
			if arg == "" || filepath.IsAbs(arg) {
				continue
			}
			p := filepath.Join(pd, arg)
			if rel, err := filepath.Rel(pd, p); err != nil || rel == "." || strings.HasPrefix(rel, "..") || !strings.ContainsRune(rel, filepath.Separator) {
				continue // outside, or a top level entry already counted
			}
			if fi, err := os.Lstat(p); err == nil {
				add(p, fi)
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// inode returns fi's inode number, or 0 where the platform has none.
func inode(fi fs.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}

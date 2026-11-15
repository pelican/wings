package external

import (
	"bytes"
	"context"
	"maps"
	"os"
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
// `wings plugins panel-access`, or the operator editing config.yml. Every
// interval it looks at the configuration file at path (modification time and
// size), the plugins directory (modification time, which changes when a plugin
// is added or removed) and the approvals in the trust store. When any of them
// changed it reads the plugins block from the file, under the file lock, and
// applies it with plugin.ApplyPolicy, which runs plugin.AfterPolicyChange.
// It returns when ctx is done.
//
// At the start it applies the block on disk if it differs from the one in
// memory, so a change made while the daemon was starting is not missed.
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
	cfg, dir  fileStamp
	approved  map[string]string
	block     config.PluginConfiguration // the block last read from the file
	haveBlock bool
}

func (w *policyWatch) start() {
	w.cfg = stampOf(w.path)
	p, err := config.PluginsOnDisk(w.c)
	if err != nil {
		log.WithField("error", err).Warn("could not read the plugins block from the config file; will retry")
		w.cfg = fileStamp{} // read it again on the next poll
		w.block = config.Get().Plugins
	} else {
		w.block, w.haveBlock = p, true
	}
	w.dir = stampOf(w.block.Directory)
	w.approved = approvals()
	if w.haveBlock && !samePolicy(w.block, config.Get().Plugins) {
		w.apply("the plugins block on disk differs from the one in memory")
	}
}

func (w *policyWatch) poll() {
	var reasons []string
	if s := stampOf(w.path); s != w.cfg {
		p, err := config.PluginsOnDisk(w.c)
		if err != nil {
			// Keep the old stamp so the next poll tries again.
			log.WithField("error", err).Warn("could not read the plugins block from the config file; will retry")
		} else {
			w.cfg = s
			if !w.haveBlock || !samePolicy(p, w.block) {
				reasons = append(reasons, "config file")
			}
			w.block, w.haveBlock = p, true
		}
	}
	if s := stampOf(w.block.Directory); s != w.dir {
		w.dir = s
		reasons = append(reasons, "plugins directory")
	}
	if a := approvals(); !maps.Equal(a, w.approved) {
		w.approved = a
		reasons = append(reasons, "trust store")
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
	plugin.ApplyPolicy(w.block)
}

// samePolicy reports whether a and b would be written to the file the same
// way. A block decoded from YAML and one built in memory differ only in empty
// versus nil lists and maps, which is not a change.
func samePolicy(a, b config.PluginConfiguration) bool {
	ya, errA := yaml.Marshal(a)
	yb, errB := yaml.Marshal(b)
	return errA == nil && errB == nil && bytes.Equal(ya, yb)
}

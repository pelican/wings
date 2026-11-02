package plugins

import (
	"strings"
	"time"

	"github.com/apex/log"

	"github.com/pelican/wings/plugins/api"
)

// JobScheduler is the node's scheduler, as the plugin subsystem needs it.
//
// It is an interface rather than a direct gocron dependency so that enabling a
// plugin can add jobs to the already-running scheduler, and disabling one can
// take them off again, without this package knowing how the scheduler works.
type JobScheduler interface {
	// AddPluginJob schedules run every interval and returns a handle for
	// removing it again.
	AddPluginJob(pluginID, name string, interval time.Duration, run func()) (handle string, err error)

	// RemovePluginJob cancels a previously added job.
	RemovePluginJob(handle string) error
}

// SetJobScheduler gives the manager somewhere to put plugin jobs.
//
// Wings builds its scheduler after the manager, so this is called in between
// discovery and loading. Jobs registered while there is no scheduler are still
// recorded, and a warning is logged, so a misordered boot shows up as a log
// line rather than as jobs that quietly never run.
func (m *Manager) SetJobScheduler(s JobScheduler) {
	m.mu.Lock()
	m.scheduler = s
	m.mu.Unlock()
}

// registerExtensions publishes the routes, adapters, parsers and jobs a freshly
// loaded plugin declared.
//
// Registration is last-one-wins on a name collision, and the collision is
// logged. Refusing to load the second plugin instead would be defensible, but
// it would mean one plugin claiming a common parser name could stop an
// unrelated plugin from ever enabling.
func (m *Manager) registerExtensions(inst *Instance) {
	reg := inst.Registration()
	id := inst.ID()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.routes == nil {
		m.routes = map[string][]api.Route{}
	}
	if len(reg.Routes) > 0 {
		m.routes[id] = reg.Routes
	}

	if reg.Backup != nil {
		if m.backups == nil {
			m.backups = map[string]backupRegistration{}
		}
		name := strings.ToLower(reg.Backup.Name)
		if existing, ok := m.backups[name]; ok && existing.pluginID != id {
			m.log().WithFields(log.Fields{
				"adapter":  name,
				"plugin":   id,
				"replaces": existing.pluginID,
			}).Warn("two plugins registered the same backup adapter name")
		}
		m.backups[name] = backupRegistration{pluginID: id, adapter: reg.Backup}
	}

	if len(reg.ConfigParsers) > 0 {
		if m.parsers == nil {
			m.parsers = map[string]parserRegistration{}
		}
		for format, fn := range reg.ConfigParsers {
			format = strings.ToLower(format)
			if existing, ok := m.parsers[format]; ok && existing.pluginID != id {
				m.log().WithFields(log.Fields{
					"format":   format,
					"plugin":   id,
					"replaces": existing.pluginID,
				}).Warn("two plugins registered a parser for the same format")
			}
			m.parsers[format] = parserRegistration{pluginID: id, parse: fn}
		}
	}

	for _, job := range reg.Jobs {
		m.scheduleJob(inst, job)
	}
}

// scheduleJob puts one plugin job on the scheduler. Called with the manager
// lock held.
func (m *Manager) scheduleJob(inst *Instance, job api.Job) {
	id := inst.ID()

	if m.scheduler == nil {
		m.log().WithFields(log.Fields{"plugin": id, "job": job.Name}).
			Warn("no scheduler available yet, so this plugin job will not run")
		return
	}

	// A floor on the interval keeps a plugin from asking for a job every
	// microsecond and starving the scheduler.
	interval := job.Interval
	if interval < time.Second {
		interval = time.Second
	}

	run := job.Run
	name := job.Name

	handle, err := m.scheduler.AddPluginJob(id, name, interval, func() {
		// Jobs go through the same panic containment as any other hook, and
		// take the plugin's hook lock, so a job cannot run at the same time as
		// a console hook from the same plugin.
		var err error
		if inst.invoke("job "+name, 0, func() { err = run() }) && err != nil {
			inst.log().WithFields(log.Fields{"job": name, "error": err}).
				Warn("plugin job returned an error")
		}
	})
	if err != nil {
		m.log().WithFields(log.Fields{"plugin": id, "job": name, "error": err}).
			Error("could not schedule plugin job")
		return
	}

	if m.jobs == nil {
		m.jobs = map[string][]string{}
	}
	m.jobs[id] = append(m.jobs[id], handle)
}

// unregisterExtensions withdraws everything a plugin published, so that
// disabling it takes effect straight away rather than at the next restart.
func (m *Manager) unregisterExtensions(inst *Instance) {
	id := inst.ID()

	m.mu.Lock()
	handles := m.jobs[id]
	delete(m.jobs, id)
	delete(m.routes, id)

	for name, reg := range m.backups {
		if reg.pluginID == id {
			delete(m.backups, name)
		}
	}
	for format, reg := range m.parsers {
		if reg.pluginID == id {
			delete(m.parsers, format)
		}
	}
	scheduler := m.scheduler
	m.mu.Unlock()

	// Removing jobs happens outside the lock: the scheduler may block briefly
	// waiting for a running job, and holding the manager lock through that
	// would stall hook dispatch for every plugin.
	if scheduler == nil {
		return
	}
	for _, h := range handles {
		if err := scheduler.RemovePluginJob(h); err != nil {
			m.log().WithFields(log.Fields{"plugin": id, "error": err}).
				Warn("could not remove a plugin job from the scheduler")
		}
	}
}

type backupRegistration struct {
	pluginID string
	adapter  *api.BackupAdapter
}

type parserRegistration struct {
	pluginID string
	parse    func(api.ConfigFile) ([]byte, error)
}

// BackupAdapter looks up a plugin-provided backup adapter by the name the Panel
// asked for.
func (m *Manager) BackupAdapter(name string) (*Instance, *api.BackupAdapter, bool) {
	m.mu.RLock()
	reg, ok := m.backups[strings.ToLower(name)]
	m.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}

	inst, ok := m.Get(reg.pluginID)
	if !ok {
		return nil, nil, false
	}
	return inst, reg.adapter, true
}

// BackupAdapterNames lists the adapters plugins currently provide, which is
// what the Panel needs in order to offer them.
func (m *Manager) BackupAdapterNames() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	out := make([]string, 0, len(m.backups))
	for name := range m.backups {
		out = append(out, name)
	}
	return out
}

// ConfigParser looks up a plugin-provided egg configuration file parser.
func (m *Manager) ConfigParser(format string) (*Instance, func(api.ConfigFile) ([]byte, error), bool) {
	m.mu.RLock()
	reg, ok := m.parsers[strings.ToLower(format)]
	m.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}

	inst, ok := m.Get(reg.pluginID)
	if !ok {
		return nil, nil, false
	}
	return inst, reg.parse, true
}

// FindRoute resolves an incoming request to a plugin route.
//
// Wings mounts one catch-all per scope at boot rather than a Gin route per
// plugin route, because Gin's router cannot be changed once it is serving and
// enabling a plugin has to work without a restart. That trade moves path
// matching here, which is why [matchRoutePath] exists.
func (m *Manager) FindRoute(pluginID, method, path string, serverScoped bool) (*Instance, api.Route, map[string]string, bool) {
	inst, ok := m.Get(pluginID)
	if !ok {
		return nil, api.Route{}, nil, false
	}

	// Only an enabled, loaded plugin answers. A disabled plugin's routes are
	// already gone from the registry, but checking here too means a plugin
	// that errored mid-request stops answering immediately.
	if inst.Status() != StatusEnabled || !inst.Loaded() {
		return nil, api.Route{}, nil, false
	}

	m.mu.RLock()
	routes := m.routes[inst.ID()]
	m.mu.RUnlock()

	method = strings.ToUpper(method)
	if path == "" {
		path = "/"
	}

	for _, r := range routes {
		if r.ServerScoped != serverScoped {
			continue
		}
		if !strings.EqualFold(r.Method, method) {
			continue
		}
		if params, ok := matchRoutePath(r.Path, path); ok {
			return inst, r, params, true
		}
	}

	return nil, api.Route{}, nil, false
}

// matchRoutePath matches a request path against a route pattern, returning any
// captured parameters.
//
// It supports the two forms a plugin author would expect from Gin: ":name"
// captures one path segment, and "*name" captures the rest of the path.
func matchRoutePath(pattern, path string) (map[string]string, bool) {
	if pattern == path {
		return nil, true
	}

	patternParts := splitPath(pattern)
	pathParts := splitPath(path)

	var params map[string]string

	for i, p := range patternParts {
		if strings.HasPrefix(p, "*") {
			// A wildcard swallows everything left, including nothing at all.
			if params == nil {
				params = map[string]string{}
			}
			params[strings.TrimPrefix(p, "*")] = strings.Join(pathParts[i:], "/")
			return params, true
		}

		if i >= len(pathParts) {
			return nil, false
		}

		if strings.HasPrefix(p, ":") {
			// A named parameter must actually match something, so that
			// "/players/" does not match "/players/:id" with an empty id.
			if pathParts[i] == "" {
				return nil, false
			}
			if params == nil {
				params = map[string]string{}
			}
			params[strings.TrimPrefix(p, ":")] = pathParts[i]
			continue
		}

		if p != pathParts[i] {
			return nil, false
		}
	}

	if len(pathParts) != len(patternParts) {
		return nil, false
	}
	return params, true
}

// splitPath breaks a path into segments, ignoring the leading slash and any
// trailing one, so "/a/b/" and "/a/b" are the same path.
func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

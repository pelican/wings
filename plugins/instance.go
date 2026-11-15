package plugins

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"emperror.dev/errors"
	"github.com/apex/log"
	"github.com/traefik/yaegi/interp"

	"github.com/pelican/wings/plugins/api"
)

// DefaultGateTimeout bounds how long a gating hook may take before Wings stops
// waiting for it.
//
// Gating hooks sit in front of things a user is waiting on, most importantly
// power actions, so a plugin that blocks must not be able to hold one open. On
// timeout the hook is treated as having abstained and the plugin is marked
// errored, which takes it out of the dispatch path until an operator looks at
// it. That is deliberately unforgiving: a plugin that cannot answer in two
// seconds is broken, and leaving it in the path would mean every power action
// on the node pays its timeout.
const DefaultGateTimeout = 2 * time.Second

// Instance is one plugin loaded into the daemon: its manifest, the interpreter
// holding its source, the hooks it registered, and the state Wings keeps about
// how it is behaving.
type Instance struct {
	// mu guards the manifest and the registration. Hook dispatch takes it for
	// reading, so saving settings does not have to wait behind a slow hook.
	mu sync.RWMutex

	manifest *Manifest
	reg      api.Registration

	// interpreter holds the plugin's interpreted source. It is nil until the
	// plugin is loaded and after it is unloaded.
	interpreter *interp.Interpreter

	// hookMu serialises hook execution for this plugin.
	//
	// yaegi's interpreter is not safe to enter from several goroutines at
	// once, and beyond that it gives plugin authors a guarantee worth having:
	// two hooks from the same plugin never run concurrently, so a plugin needs
	// no locking of its own. The cost is that one slow hook delays that
	// plugin's other hooks. It does not delay other plugins, which each have
	// their own interpreter and their own lock.
	hookMu sync.Mutex

	store   *store
	host    *host
	bridge  Bridge
	fetcher api.Fetcher

	gateTimeout time.Duration

	// onStatusChange lets the manager persist a status the instance decided on
	// its own, such as after a panic.
	onStatusChange func(*Instance, Status, string)
}

// ID is the plugin's identifier, which is also its directory name.
func (i *Instance) ID() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.ID
}

// Manifest returns a copy of the plugin's manifest. It is a copy so a caller
// cannot mutate the plugin's state by holding onto it.
func (i *Instance) Manifest() Manifest {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return *i.manifest
}

// Status is the plugin's current status.
func (i *Instance) Status() Status {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.Meta.Status
}

// Loaded reports whether the plugin's source is interpreted and its hooks are
// live.
func (i *Instance) Loaded() bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.interpreter != nil
}

// Registration returns the hooks the plugin registered. The returned value is
// only meaningful while the plugin is loaded.
func (i *Instance) Registration() api.Registration {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.reg
}

func (i *Instance) log() *log.Entry {
	return log.WithField("subsystem", "plugins").WithField("plugin", i.ID())
}

// settingValue returns one effective setting value.
func (i *Instance) settingValue(key string) (any, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.SettingValue(key)
}

// settingsSnapshot returns every effective setting value.
func (i *Instance) settingsSnapshot() map[string]any {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.manifest.ResolvedSettings()
}

// sourceFiles lists the Go files making up the plugin, in a stable order.
//
// Only the top level of the plugin directory is read. Go source in
// subdirectories is ignored rather than interpreted, because a plugin is one
// package by design: keeping it flat is what makes "read the directory" a
// complete audit of what the plugin does.
func sourceFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, errors.Wrapf(err, "plugins: could not read plugin directory %s", dir)
	}

	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		// Test files are skipped so a plugin shipping its tests does not have
		// them interpreted, which would pull in the testing package the symbol
		// table does not carry.
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}

	if len(files) == 0 {
		return nil, errors.Errorf("plugins: %s contains no .go files", dir)
	}

	// Sort so load order does not depend on how the filesystem returns
	// entries. Plugins that happen to depend on declaration order across
	// files then behave the same on every node.
	sort.Strings(files)
	return files, nil
}

// load interprets the plugin's source, calls its entrypoint, and runs Init.
//
// A failure at any step leaves the instance unloaded rather than partly
// loaded: the interpreter is discarded, so a plugin whose Init returned an
// error has no lingering goroutines holding a Host.
func (i *Instance) load(trusted bool, goPath string) error {
	i.mu.Lock()
	manifest := i.manifest
	i.mu.Unlock()

	files, err := sourceFiles(manifest.Directory())
	if err != nil {
		return err
	}

	// Anything the plugin prints goes to the Wings log attributed to the
	// plugin, rather than to the daemon's own stdout where it would look like
	// Wings output.
	out := &logWriter{entry: i.log(), level: log.InfoLevel}
	errOut := &logWriter{entry: i.log(), level: log.WarnLevel}

	in := interp.New(interp.Options{
		Stdout: out,
		Stderr: errOut,

		// Where the plugin's declared Go modules were laid out as source.
		// Empty when it declared none, which is the common case. This is the
		// whole mechanism behind dependency support: the interpreter resolves
		// an import by reading source from here, so a library needs no
		// compilation and the node needs no toolchain.
		GoPath: goPath,

		// A plugin gets no process environment and no command line. Neither is
		// reachable through the curated symbol table anyway, but an empty slice
		// rather than nil means yaegi does not fall back to the daemon's.
		Env:  []string{},
		Args: []string{},

		// Left off deliberately. It would add os/exec and unsafe, and the
		// curated symbol table is what actually bounds a plugin; see
		// safeStdlib for why.
		Unrestricted: false,
	})

	if err := in.Use(buildSymbols(trusted)); err != nil {
		return errors.Wrap(err, "plugins: could not prepare the interpreter")
	}

	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			return errors.Wrapf(err, "plugins: could not read %s", f)
		}
		if _, err := in.Eval(string(src)); err != nil {
			// The interpreter's error already carries the line and column,
			// but not which file, which is the first thing a plugin author
			// wants to know.
			return errors.Wrapf(err, "plugins: %s", filepath.Base(f))
		}
	}

	entry := manifest.Package + "." + manifest.EffectiveEntrypoint()
	v, err := in.Eval(entry)
	if err != nil {
		return errors.Wrapf(err, "plugins: could not find entrypoint %s", entry)
	}

	reg, err := callEntrypoint(entry, v)
	if err != nil {
		return err
	}

	if err := validateRegistration(manifest, reg); err != nil {
		return err
	}

	i.mu.Lock()
	i.interpreter = in
	i.reg = reg
	i.mu.Unlock()

	// Init is the plugin's first chance to fail, and it runs under the same
	// panic containment as every other hook.
	if reg.Init != nil {
		var initErr error
		if !i.invoke("Init", 0, func() { initErr = reg.Init(i.host) }) {
			i.discard()
			return errors.New("plugins: the plugin panicked during Init")
		}
		if initErr != nil {
			i.discard()
			return errors.Wrap(initErr, "plugins: the plugin's Init returned an error")
		}
	}

	return nil
}

// callEntrypoint invokes the plugin's New function, accepting either of the two
// shapes a plugin author would reasonably write.
func callEntrypoint(name string, v reflect.Value) (api.Registration, error) {
	switch fn := v.Interface().(type) {
	case func() api.Registration:
		return fn(), nil
	case func() (api.Registration, error):
		reg, err := fn()
		if err != nil {
			return api.Registration{}, errors.Wrap(err, "plugins: the entrypoint returned an error")
		}
		return reg, nil
	default:
		return api.Registration{}, errors.Errorf(
			"plugins: %s must be func() api.Registration or func() (api.Registration, error), but is %s",
			name, v.Type().String(),
		)
	}
}

// validateRegistration rejects a registration Wings cannot mount, before any
// of it goes live.
//
// These are checked up front rather than at dispatch time so a plugin with a
// malformed route fails to enable with a clear reason, instead of enabling and
// then returning 404s an operator has to work out for themselves.
func validateRegistration(m *Manifest, reg api.Registration) error {
	var problems []string

	if reg.Backup != nil {
		switch {
		case reg.Backup.Name == "":
			problems = append(problems, "backup adapter is missing a name")
		case reg.Backup.Name == "wings" || reg.Backup.Name == "s3":
			problems = append(problems, "backup adapter cannot be named "+reg.Backup.Name+", which is built in")
		case reg.Backup.Create == nil:
			problems = append(problems, "backup adapter "+reg.Backup.Name+" has no Create function")
		case reg.Backup.Restore == nil:
			problems = append(problems, "backup adapter "+reg.Backup.Name+" has no Restore function")
		case reg.Backup.Delete == nil:
			problems = append(problems, "backup adapter "+reg.Backup.Name+" has no Delete function")
		}
	}

	seenRoutes := map[string]bool{}
	for idx, r := range reg.Routes {
		switch {
		case r.Handler == nil:
			problems = append(problems, routeLabel(idx, r)+" has no handler")
		case r.Method == "":
			problems = append(problems, routeLabel(idx, r)+" is missing a method")
		case !strings.HasPrefix(r.Path, "/"):
			problems = append(problems, routeLabel(idx, r)+" path must begin with a slash")
		}

		key := strings.ToUpper(r.Method) + " " + r.Path
		if r.ServerScoped {
			key = "server " + key
		}
		if seenRoutes[key] {
			problems = append(problems, routeLabel(idx, r)+" is registered more than once")
		}
		seenRoutes[key] = true
	}

	seenJobs := map[string]bool{}
	for idx, j := range reg.Jobs {
		switch {
		case j.Name == "":
			problems = append(problems, "jobs["+strconv.Itoa(idx)+"] is missing a name")
		case j.Run == nil:
			problems = append(problems, "job "+j.Name+" has no Run function")
		case seenJobs[j.Name]:
			problems = append(problems, "job "+j.Name+" is registered more than once")
		}
		seenJobs[j.Name] = true
	}

	seenSettings := map[string]bool{}
	for idx, setting := range reg.Settings {
		switch {
		case setting.Key == "":
			problems = append(problems, "settings["+strconv.Itoa(idx)+"] is missing a key")
			continue
		case seenSettings[setting.Key]:
			problems = append(problems, "setting "+setting.Key+" is declared more than once")
		}
		seenSettings[setting.Key] = true

		switch setting.Type {
		case "", api.SettingString, api.SettingText, api.SettingNumber,
			api.SettingBoolean, api.SettingSelect, api.SettingSecret:
		default:
			problems = append(problems, "setting "+setting.Key+" has unknown type "+string(setting.Type))
		}

		if setting.Type == api.SettingSelect && len(setting.Options) == 0 {
			problems = append(problems, "setting "+setting.Key+" is a select and needs at least one option")
		}
	}

	for format, fn := range reg.ConfigParsers {
		if format == "" {
			problems = append(problems, "a config parser is registered under an empty format name")
		}
		if fn == nil {
			problems = append(problems, "config parser "+format+" is nil")
		}
	}

	if len(problems) > 0 {
		return errors.New("plugins: " + m.ID + " registered something unusable: " + strings.Join(problems, "; "))
	}
	return nil
}

func routeLabel(idx int, r api.Route) string {
	if r.Path != "" {
		return "route " + strings.ToUpper(r.Method) + " " + r.Path
	}
	return "routes[" + strconv.Itoa(idx) + "]"
}

// unload runs the plugin's Shutdown hook and discards its interpreter.
func (i *Instance) unload() {
	reg := i.Registration()

	if reg.Shutdown != nil {
		var err error
		// Shutdown gets a longer deadline than a gate: it is not in front of a
		// user, and flushing state is exactly the kind of thing worth waiting
		// a moment for.
		if i.invoke("Shutdown", 10*time.Second, func() { err = reg.Shutdown() }) && err != nil {
			i.log().WithField("error", err).Warn("plugin reported an error while shutting down")
		}
	}

	i.discard()
}

// discard drops the interpreter and hooks without running Shutdown.
func (i *Instance) discard() {
	i.mu.Lock()
	i.interpreter = nil
	i.reg = api.Registration{}
	i.mu.Unlock()
}

// invoke runs a hook with panic containment and, when timeout is positive, a
// deadline.
//
// It reports whether the hook completed. A false result means the hook either
// panicked or overran, and in both cases the plugin has been marked errored, so
// the caller should behave as though the hook had not been registered rather
// than as though it had denied anything.
//
// A hook that overruns is not stopped, because nothing can stop a running
// goroutine. It keeps the plugin's hook lock, which is why the plugin is taken
// out of the dispatch path: every later hook would otherwise queue behind it.
func (i *Instance) invoke(hook string, timeout time.Duration, fn func()) bool {
	res := make(chan bool, 1)

	go func() {
		i.hookMu.Lock()
		defer i.hookMu.Unlock()

		completed := false
		defer func() {
			if r := recover(); r != nil {
				i.log().WithFields(log.Fields{
					"hook":  hook,
					"panic": r,
					"stack": string(debug.Stack()),
				}).Error("plugin panicked; taking it out of the dispatch path")

				i.markErrored("panicked in " + hook + " hook")
			}
			res <- completed
		}()

		fn()
		completed = true
	}()

	if timeout <= 0 {
		return <-res
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case ok := <-res:
		return ok
	case <-timer.C:
		i.log().WithFields(log.Fields{
			"hook":    hook,
			"timeout": timeout.String(),
		}).Error("plugin hook exceeded its deadline; taking it out of the dispatch path")

		i.markErrored(hook + " hook did not return within " + timeout.String())
		return false
	}
}

// invokeAsync runs an observing hook without waiting for it.
//
// Observing hooks cannot change an outcome, so making a caller wait for one
// would be paying latency for nothing. They still run under the plugin's hook
// lock, so they stay ordered with respect to that plugin's other hooks.
func (i *Instance) invokeAsync(hook string, fn func()) {
	go i.invoke(hook, 0, fn)
}

// markErrored records that the plugin misbehaved and stops it being dispatched
// to. It does not unload the interpreter: an operator may want to look at the
// plugin, and a half-unloaded plugin is harder to reason about than a stopped
// one.
func (i *Instance) markErrored(reason string) {
	i.mu.Lock()
	alreadyErrored := i.manifest.Meta.Status == StatusErrored
	i.mu.Unlock()

	// Only the first failure is recorded. A plugin panicking on every console
	// line would otherwise rewrite its manifest thousands of times a second.
	if alreadyErrored {
		return
	}

	if i.onStatusChange != nil {
		i.onStatusChange(i, StatusErrored, reason)
	}
}

// logWriter turns whatever a plugin writes to stdout or stderr into Wings log
// lines, splitting on newlines so one print is one line.
type logWriter struct {
	entry *log.Entry
	level log.Level

	mu  sync.Mutex
	buf []byte
}

func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buf = append(w.buf, p...)

	for {
		idx := strings.IndexByte(string(w.buf), '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(w.buf[:idx]), "\r")
		w.buf = w.buf[idx+1:]

		if line == "" {
			continue
		}
		switch w.level {
		case log.WarnLevel:
			w.entry.Warn(line)
		default:
			w.entry.Info(line)
		}
	}

	// A plugin printing without a trailing newline would otherwise never have
	// its last line flushed. Cap what we hold so a plugin writing an unbounded
	// string without newlines cannot grow this buffer without limit.
	if len(w.buf) > 8192 {
		w.entry.Warn(strings.TrimRight(string(w.buf), "\r"))
		w.buf = w.buf[:0]
	}

	return len(p), nil
}

// InvokeRoute runs a plugin's HTTP handler with panic containment.
//
// It is exported because the router mounts plugin routes, and a handler is the
// one hook where a caller outside this package needs to run plugin code. No
// deadline is applied: an HTTP request already has the client's own timeout in
// front of it, and cutting a handler off mid-response would produce a truncated
// body rather than a clean error.
func (i *Instance) InvokeRoute(fn func()) bool {
	return i.invoke("route handler", 0, fn)
}

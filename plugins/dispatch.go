package plugins

import (
	"sync"
	"sync/atomic"

	"github.com/pelican/wings/plugins/api"
)

// dispatchSet is the precomputed answer to "which plugins care about this
// hook", rebuilt whenever a plugin is loaded, unloaded or reordered.
//
// Console output runs this gauntlet for every line every server prints, so
// dispatch cannot afford to take a lock and walk every plugin asking whether it
// registered a hook. Instead the manager keeps this snapshot behind an atomic
// pointer: a dispatch reads one pointer, finds an empty slice, and returns.
// That is what makes an unhooked node pay nothing for having the subsystem
// compiled in.
type dispatchSet struct {
	powerGate    []*Instance
	installGate  []*Instance
	deleteGate   []*Instance
	transferGate []*Instance
	lifecycle    []*Instance
	console      []*Instance
	command      []*Instance
	fileGate     []*Instance
	fileObserve  []*Instance
	container    []*Instance
	websocket    []*Instance
	wsOpen       []*Instance
	wsClose      []*Instance
	startup      []*Instance
	crashGate    []*Instance
	stats        []*Instance
	backupGate   []*Instance
	backupDone   []*Instance
	sftpAuth     []*Instance
	installOut   []*Instance
	diagnostics  []*Instance
}

// emptyDispatch is what dispatch reads before any manager is installed.
var emptyDispatch = &dispatchSet{}

var (
	// current is the live dispatch set. It is read on every hook and written
	// only when the set of loaded plugins changes.
	current atomic.Pointer[dispatchSet]

	managerMu sync.RWMutex
	manager   *Manager
)

func init() {
	current.Store(emptyDispatch)
}

// SetManager installs the node's plugin manager, after which hook dispatch
// starts reaching plugins. Wings calls this once at boot.
func SetManager(m *Manager) {
	managerMu.Lock()
	manager = m
	managerMu.Unlock()

	if m != nil {
		m.refreshDispatch()
	} else {
		current.Store(emptyDispatch)
	}
}

// Active returns the node's plugin manager, or nil when there is none. The HTTP
// layer uses it; hook call sites should use the dispatch functions instead,
// which are safe to call whether or not a manager exists.
func Active() *Manager {
	managerMu.RLock()
	defer managerMu.RUnlock()
	return manager
}

// refreshDispatch recomputes the dispatch set from the currently loaded
// plugins.
func (m *Manager) refreshDispatch() {
	set := &dispatchSet{}

	for _, inst := range m.active() {
		reg := inst.Registration()

		if reg.BeforePowerAction != nil {
			set.powerGate = append(set.powerGate, inst)
		}
		if reg.BeforeServerInstall != nil {
			set.installGate = append(set.installGate, inst)
		}
		if reg.BeforeServerDelete != nil {
			set.deleteGate = append(set.deleteGate, inst)
		}
		if reg.BeforeServerTransfer != nil {
			set.transferGate = append(set.transferGate, inst)
		}
		if reg.OnServerLifecycle != nil {
			set.lifecycle = append(set.lifecycle, inst)
		}
		if reg.OnConsoleLine != nil {
			set.console = append(set.console, inst)
		}
		if reg.OnCommand != nil {
			set.command = append(set.command, inst)
		}
		if reg.BeforeFileAction != nil {
			set.fileGate = append(set.fileGate, inst)
		}
		if reg.OnFileAction != nil {
			set.fileObserve = append(set.fileObserve, inst)
		}
		if reg.MutateContainer != nil {
			set.container = append(set.container, inst)
		}
		if reg.OnWebsocketMessage != nil {
			set.websocket = append(set.websocket, inst)
		}
		if reg.OnWebsocketOpen != nil {
			set.wsOpen = append(set.wsOpen, inst)
		}
		if reg.OnWebsocketClose != nil {
			set.wsClose = append(set.wsClose, inst)
		}
		if reg.MutateStartup != nil {
			set.startup = append(set.startup, inst)
		}
		if reg.BeforeCrashRestart != nil {
			set.crashGate = append(set.crashGate, inst)
		}
		if reg.OnStats != nil {
			set.stats = append(set.stats, inst)
		}
		if reg.BeforeBackup != nil {
			set.backupGate = append(set.backupGate, inst)
		}
		if reg.OnBackupCompleted != nil {
			set.backupDone = append(set.backupDone, inst)
		}
		if reg.OnSftpAuth != nil {
			set.sftpAuth = append(set.sftpAuth, inst)
		}
		if reg.OnInstallOutput != nil {
			set.installOut = append(set.installOut, inst)
		}
		if reg.Diagnostics != nil {
			set.diagnostics = append(set.diagnostics, inst)
		}
	}

	current.Store(set)
}

// --- gating hooks ---

// GatePowerAction asks every plugin whether a power action may proceed.
//
// The first refusal wins and the rest are not consulted, so a plugin cannot
// override an earlier plugin's decision. A plugin that panics or overruns has
// already been taken out of the dispatch path and is treated as abstaining,
// because failing closed here would mean one broken plugin could stop every
// server on the node from starting.
func GatePowerAction(s api.Server, action api.PowerAction) (bool, string) {
	for _, inst := range current.Load().powerGate {
		reg := inst.Registration()
		if reg.BeforePowerAction == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforePowerAction", inst.gateTimeout, func() {
			d = reg.BeforePowerAction(s, action)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// GateServerInstall asks every plugin whether an install or reinstall may
// proceed.
func GateServerInstall(s api.Server) (bool, string) {
	for _, inst := range current.Load().installGate {
		reg := inst.Registration()
		if reg.BeforeServerInstall == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforeServerInstall", inst.gateTimeout, func() {
			d = reg.BeforeServerInstall(s)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// GateServerDelete asks every plugin whether a server may be removed from the
// node.
func GateServerDelete(s api.Server) (bool, string) {
	for _, inst := range current.Load().deleteGate {
		reg := inst.Registration()
		if reg.BeforeServerDelete == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforeServerDelete", inst.gateTimeout, func() {
			d = reg.BeforeServerDelete(s)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// GateServerTransfer asks every plugin whether a server may be transferred
// away.
func GateServerTransfer(s api.Server) (bool, string) {
	for _, inst := range current.Load().transferGate {
		reg := inst.Registration()
		if reg.BeforeServerTransfer == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforeServerTransfer", inst.gateTimeout, func() {
			d = reg.BeforeServerTransfer(s)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// HasFileHooks reports whether any plugin gates filesystem operations.
//
// The file paths call this before building a server snapshot, the same way the
// console path does, so a node with no file plugins pays one atomic load per
// operation rather than a snapshot per operation.
func HasFileHooks() bool {
	return len(current.Load().fileGate) > 0
}

// HasFileObservers reports whether any plugin watches completed filesystem
// operations.
func HasFileObservers() bool {
	return len(current.Load().fileObserve) > 0
}

// GateFileAction offers a user's filesystem operation to every plugin that
// asked to see one.
//
// It reports whether the operation may proceed, whether a plugin has already
// taken care of it, and the reason if it was refused. A plugin taking the
// operation over stops the walk: once one plugin has moved a file into a
// recycle bin, offering the same delete to the next plugin would have it act on
// a path that is no longer there.
func GateFileAction(e api.FileEvent) (allow bool, handled bool, reason string) {
	for _, inst := range current.Load().fileGate {
		reg := inst.Registration()
		if reg.BeforeFileAction == nil {
			continue
		}

		var d api.FileDirective
		if !inst.invoke("BeforeFileAction", inst.gateTimeout, func() {
			d = reg.BeforeFileAction(e)
		}) {
			continue
		}

		if d.Deny {
			return false, false, denialReason(inst, api.Decision{Reason: d.Reason})
		}
		if d.Handled {
			return true, true, ""
		}
	}
	return true, false, ""
}

// denialReason attributes a refusal to the plugin that made it, so the user
// waiting on the action is told which plugin stopped them rather than being
// left with an unexplained failure.
func denialReason(inst *Instance, d api.Decision) string {
	name := inst.Manifest().Name
	if name == "" {
		name = inst.ID()
	}
	if d.Reason == "" {
		return name + " blocked this action"
	}
	return name + ": " + d.Reason
}

// --- observing hooks ---

// Lifecycle tells every interested plugin that something happened to a server.
// It does not wait for them.
func Lifecycle(l api.Lifecycle) {
	for _, inst := range current.Load().lifecycle {
		reg := inst.Registration()
		if reg.OnServerLifecycle == nil {
			continue
		}
		hook := reg.OnServerLifecycle
		inst.invokeAsync("OnServerLifecycle", func() { hook(l) })
	}
}

// FileAction tells every interested plugin that a filesystem operation
// succeeded.
func FileAction(e api.FileEvent) {
	for _, inst := range current.Load().fileObserve {
		reg := inst.Registration()
		if reg.OnFileAction == nil {
			continue
		}
		hook := reg.OnFileAction
		inst.invokeAsync("OnFileAction", func() { hook(e) })
	}
}

// --- console ---

// HasConsoleHooks reports whether any plugin wants to see console output.
//
// The console path calls this before assembling a line so that a node with no
// console plugins does not pay to build an api.Server snapshot per line, which
// would otherwise be the most expensive part of having the subsystem enabled.
func HasConsoleHooks() bool {
	return len(current.Load().console) > 0
}

// ConsoleLine runs a line of server output past every plugin that asked to see
// it, returning the line to deliver and whether to deliver it at all.
//
// Plugins see the line in load order, each seeing what the previous one
// returned, so two plugins rewriting the same line compose rather than
// conflict. A drop is final: later plugins are not consulted, because a line
// one plugin suppressed should not be resurrected by the next.
//
// This runs synchronously, which is what lets a plugin redact a line before
// anyone sees it. It is also why the hook is the one place a slow plugin is
// most likely to be noticed.
func ConsoleLine(s api.Server, line string) (string, bool) {
	set := current.Load()
	if len(set.console) == 0 {
		return line, true
	}

	for _, inst := range set.console {
		reg := inst.Registration()
		if reg.OnConsoleLine == nil {
			continue
		}

		var d api.ConsoleDirective
		if !inst.invoke("OnConsoleLine", inst.gateTimeout, func() {
			d = reg.OnConsoleLine(api.ConsoleLine{Server: s, Line: line})
		}) {
			continue
		}

		if d.Drop {
			return "", false
		}
		if d.Rewritten {
			line = d.Rewrite
		}
	}

	return line, true
}

// HasCommandHooks reports whether any plugin wants to inspect console commands.
func HasCommandHooks() bool {
	return len(current.Load().command) > 0
}

// Command runs a console command past every plugin that asked to see it,
// returning the command to send and whether to send it.
//
// As with console output, plugins compose in load order and the first refusal
// is final.
func Command(c api.Command) (string, bool, string) {
	set := current.Load()
	if len(set.command) == 0 {
		return c.Command, true, ""
	}

	for _, inst := range set.command {
		reg := inst.Registration()
		if reg.OnCommand == nil {
			continue
		}

		var d api.CommandDirective
		if !inst.invoke("OnCommand", inst.gateTimeout, func() {
			d = reg.OnCommand(c)
		}) {
			continue
		}

		if d.Deny {
			reason := d.Reason
			if reason == "" {
				reason = "blocked"
			}
			return "", false, denialReason(inst, api.Decision{Reason: reason})
		}
		if d.Rewritten {
			c.Command = d.Rewrite
		}
	}

	return c.Command, true, ""
}

// --- container ---

// HasContainerHooks reports whether any plugin wants to shape containers.
func HasContainerHooks() bool {
	return len(current.Load().container) > 0
}

// MutateContainer collects every plugin's changes to a container Wings is about
// to create, merged into one patch in load order.
//
// Merging here rather than applying each patch in turn means the Docker
// environment has a single thing to validate and apply, and a single place
// reapplies the security settings a plugin is not allowed to weaken.
func MutateContainer(spec api.ContainerSpec) api.ContainerPatch {
	set := current.Load()
	if len(set.container) == 0 {
		return api.ContainerPatch{}
	}

	merged := api.ContainerPatch{}

	for _, inst := range set.container {
		reg := inst.Registration()
		if reg.MutateContainer == nil {
			continue
		}

		var p api.ContainerPatch
		if !inst.invoke("MutateContainer", inst.gateTimeout, func() {
			p = reg.MutateContainer(spec)
		}) {
			continue
		}

		mergePatch(&merged, p)

		// Later plugins see what earlier ones asked for, so a plugin can
		// react to another's labels rather than overwriting them blind.
		applyPatchToSpec(&spec, p)
	}

	return merged
}

// mergePatch folds src into dst. Maps merge with src winning, lists append, and
// scalars take the last non-empty value.
func mergePatch(dst *api.ContainerPatch, src api.ContainerPatch) {
	if src.Image != "" {
		dst.Image = src.Image
	}

	if len(src.Labels) > 0 {
		if dst.Labels == nil {
			dst.Labels = map[string]string{}
		}
		for k, v := range src.Labels {
			dst.Labels[k] = v
		}
	}

	if len(src.Env) > 0 {
		if dst.Env == nil {
			dst.Env = map[string]string{}
		}
		for k, v := range src.Env {
			dst.Env[k] = v
		}
	}

	if len(src.Sysctls) > 0 {
		if dst.Sysctls == nil {
			dst.Sysctls = map[string]string{}
		}
		for k, v := range src.Sysctls {
			dst.Sysctls[k] = v
		}
	}

	dst.Mounts = append(dst.Mounts, src.Mounts...)
	dst.Devices = append(dst.Devices, src.Devices...)
	dst.ExtraHosts = append(dst.ExtraHosts, src.ExtraHosts...)

	// The largest request wins rather than the last, so plugin order does not
	// decide whether a server gets the shared memory it needs.
	if src.ShmSizeBytes > dst.ShmSizeBytes {
		dst.ShmSizeBytes = src.ShmSizeBytes
	}
}

// applyPatchToSpec reflects a patch back into the spec shown to later plugins.
func applyPatchToSpec(spec *api.ContainerSpec, p api.ContainerPatch) {
	if p.Image != "" {
		spec.Image = p.Image
	}
	if len(p.Labels) > 0 {
		if spec.Labels == nil {
			spec.Labels = map[string]string{}
		}
		for k, v := range p.Labels {
			spec.Labels[k] = v
		}
	}
	if len(p.Env) > 0 {
		if spec.Env == nil {
			spec.Env = map[string]string{}
		}
		for k, v := range p.Env {
			spec.Env[k] = v
		}
	}
	spec.Mounts = append(spec.Mounts, p.Mounts...)
}

// --- websocket ---

// HasWebsocketHooks reports whether any plugin handles unrecognised websocket
// events.
func HasWebsocketHooks() bool {
	return len(current.Load().websocket) > 0
}

// WebsocketMessage offers an unrecognised console websocket message to every
// plugin that asked for one, gathering the replies to send back.
func WebsocketMessage(msg api.WebsocketMessage) []api.WebsocketReply {
	set := current.Load()
	if len(set.websocket) == 0 {
		return nil
	}

	var replies []api.WebsocketReply

	for _, inst := range set.websocket {
		reg := inst.Registration()
		if reg.OnWebsocketMessage == nil {
			continue
		}

		var out []api.WebsocketReply
		if !inst.invoke("OnWebsocketMessage", inst.gateTimeout, func() {
			out = reg.OnWebsocketMessage(msg)
		}) {
			continue
		}
		replies = append(replies, out...)
	}

	return replies
}

// ServerSnapshot resolves a server by uuid for a caller that has the id but not
// the server, which is the position the Docker environment is in when it builds
// a container.
//
// It returns false when there is no manager, no bridge, or no such server, so a
// caller can fall back to whatever it already knows rather than handling three
// separate failure modes.
func ServerSnapshot(uuid string) (api.Server, bool) {
	m := Active()
	if m == nil || m.bridge == nil {
		return api.Server{}, false
	}
	return m.bridge.GetServer(uuid)
}

// --- startup ---

// HasStartupHooks reports whether any plugin shapes server startup.
func HasStartupHooks() bool {
	return len(current.Load().startup) > 0
}

// MutateStartup collects every plugin's changes to the command and environment
// a server is about to boot with, merged in load order.
//
// Later plugins see what earlier ones asked for, so a plugin can build on
// another's flags rather than overwriting them blind.
func MutateStartup(s api.Server, startup api.Startup) api.StartupPatch {
	set := current.Load()
	if len(set.startup) == 0 {
		return api.StartupPatch{}
	}

	merged := api.StartupPatch{}

	for _, inst := range set.startup {
		reg := inst.Registration()
		if reg.MutateStartup == nil {
			continue
		}

		var p api.StartupPatch
		if !inst.invoke("MutateStartup", inst.gateTimeout, func() {
			p = reg.MutateStartup(s, startup)
		}) {
			continue
		}

		if p.Command != "" {
			merged.Command = p.Command
			startup.Command = p.Command
		}

		for k, v := range p.Environment {
			if merged.Environment == nil {
				merged.Environment = map[string]string{}
			}
			merged.Environment[k] = v

			if startup.Environment == nil {
				startup.Environment = map[string]string{}
			}
			startup.Environment[k] = v
		}
	}

	return merged
}

// --- crash ---

// GateCrashRestart asks every plugin whether a crashed server should be
// restarted.
//
// A plugin that panics or overruns is treated as abstaining, so a broken plugin
// cannot leave every crashed server on the node switched off.
func GateCrashRestart(info api.CrashInfo) (bool, string) {
	for _, inst := range current.Load().crashGate {
		reg := inst.Registration()
		if reg.BeforeCrashRestart == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforeCrashRestart", inst.gateTimeout, func() {
			d = reg.BeforeCrashRestart(info)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// --- stats ---

// HasStatsHooks reports whether any plugin watches resource usage.
//
// Stats are sampled for every running server on a fixed interval, so the
// caller checks this before building a server snapshot it would otherwise
// throw away.
func HasStatsHooks() bool {
	return len(current.Load().stats) > 0
}

// Stats delivers a resource sample to every interested plugin without waiting.
func Stats(s api.Server, stats api.Stats) {
	for _, inst := range current.Load().stats {
		reg := inst.Registration()
		if reg.OnStats == nil {
			continue
		}
		hook := reg.OnStats
		inst.invokeAsync("OnStats", func() { hook(s, stats) })
	}
}

// --- backups ---

// GateBackup asks every plugin whether a backup may be taken.
func GateBackup(req api.BackupRequest) (bool, string) {
	for _, inst := range current.Load().backupGate {
		reg := inst.Registration()
		if reg.BeforeBackup == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("BeforeBackup", inst.gateTimeout, func() {
			d = reg.BeforeBackup(req)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// BackupCompleted tells every interested plugin a backup finished.
func BackupCompleted(outcome api.BackupOutcome) {
	for _, inst := range current.Load().backupDone {
		reg := inst.Registration()
		if reg.OnBackupCompleted == nil {
			continue
		}
		hook := reg.OnBackupCompleted
		inst.invokeAsync("OnBackupCompleted", func() { hook(outcome) })
	}
}

// --- sftp ---

// HasSftpAuthHooks reports whether any plugin vets SFTP logins.
func HasSftpAuthHooks() bool {
	return len(current.Load().sftpAuth) > 0
}

// GateSftpAuth asks every plugin whether an SFTP session may start, after the
// Panel has already accepted the credentials.
func GateSftpAuth(auth api.SftpAuth) (bool, string) {
	for _, inst := range current.Load().sftpAuth {
		reg := inst.Registration()
		if reg.OnSftpAuth == nil {
			continue
		}

		var d api.Decision
		if !inst.invoke("OnSftpAuth", inst.gateTimeout, func() {
			d = reg.OnSftpAuth(auth)
		}) {
			continue
		}
		if !d.Allow {
			return false, denialReason(inst, d)
		}
	}
	return true, ""
}

// --- install output ---

// HasInstallOutputHooks reports whether any plugin watches install output.
func HasInstallOutputHooks() bool {
	return len(current.Load().installOut) > 0
}

// InstallOutput delivers one line of installation output to every interested
// plugin without waiting.
func InstallOutput(s api.Server, line string) {
	for _, inst := range current.Load().installOut {
		reg := inst.Registration()
		if reg.OnInstallOutput == nil {
			continue
		}
		hook := reg.OnInstallOutput
		inst.invokeAsync("OnInstallOutput", func() {
			hook(api.InstallLine{Server: s, Line: line})
		})
	}
}

// --- diagnostics ---

// Diagnostics collects what every plugin wants included in the node's
// diagnostics report, keyed by plugin id.
//
// A plugin that fails to answer is reported as such rather than omitted, since
// a plugin that cannot describe itself is exactly what someone reading a
// diagnostics report wants to know about.
func Diagnostics() map[string]map[string]string {
	set := current.Load()
	if len(set.diagnostics) == 0 {
		return nil
	}

	out := make(map[string]map[string]string, len(set.diagnostics))

	for _, inst := range set.diagnostics {
		reg := inst.Registration()
		if reg.Diagnostics == nil {
			continue
		}

		var fields map[string]string
		if !inst.invoke("Diagnostics", inst.gateTimeout, func() {
			fields = reg.Diagnostics()
		}) {
			out[inst.ID()] = map[string]string{"error": "the plugin failed to report diagnostics"}
			continue
		}

		out[inst.ID()] = fields
	}

	return out
}

// HasWebsocketLifecycleHooks reports whether any plugin watches console
// connections opening or closing.
func HasWebsocketLifecycleHooks() bool {
	set := current.Load()
	return len(set.wsOpen) > 0 || len(set.wsClose) > 0
}

// WebsocketOpened tells interested plugins a console connection was opened.
func WebsocketOpened(conn api.WebsocketConnection) {
	for _, inst := range current.Load().wsOpen {
		reg := inst.Registration()
		if reg.OnWebsocketOpen == nil {
			continue
		}
		hook := reg.OnWebsocketOpen
		inst.invokeAsync("OnWebsocketOpen", func() { hook(conn) })
	}
}

// WebsocketClosed tells interested plugins a console connection went away.
func WebsocketClosed(conn api.WebsocketConnection) {
	for _, inst := range current.Load().wsClose {
		reg := inst.Registration()
		if reg.OnWebsocketClose == nil {
			continue
		}
		hook := reg.OnWebsocketClose
		inst.invokeAsync("OnWebsocketClose", func() { hook(conn) })
	}
}

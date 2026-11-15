package api

import "time"

// Registration is what a plugin's New function returns. It is the complete list
// of everything a plugin can hook into, and every field is optional: leave a
// hook nil and Wings never calls it.
//
// Hooks come in two kinds. A gating hook returns a [Decision] and runs before
// Wings does the thing, so it can refuse it. An observing hook returns nothing
// and runs after, so it cannot change the outcome. When several enabled plugins
// hook the same gate, they run in the load order the operator set and the first
// denial wins, so a later plugin cannot override an earlier refusal.
//
// Hooks must return promptly. Wings gives a gating hook a deadline, after which
// it treats the hook as having abstained and logs it, because a plugin that
// hangs must not be able to wedge a power action. Do slow work in a [Job] or a
// goroutine, not in a gate.
type Registration struct {
	// Init runs once when the plugin is enabled, before any other hook. It is
	// the only place a plugin is handed its [Host]. Returning an error marks
	// the plugin errored and leaves it disabled.
	Init func(Host) error

	// Shutdown runs when the plugin is disabled or Wings is stopping. Flush
	// anything you would be sad to lose, then return; Wings does not wait
	// forever.
	Shutdown func() error

	// Settings declares the plugin's configuration surface: the fields an
	// operator sees on the Panel and the keys [Settings] reads back.
	//
	// Declared here rather than in the manifest, so that everything the plugin
	// does is registered in one place alongside its routes and jobs. Wings
	// remembers the schema when the plugin loads, which is what lets the Panel
	// render a form for a plugin that is switched off.
	Settings []Setting

	// SettingsChanged runs after an operator saves new settings for the plugin
	// on the Panel. The plugin stays enabled either way, but returning an
	// error is recorded against it so the operator can see the new settings
	// were rejected.
	SettingsChanged func(Settings) error

	// --- power and lifecycle ---

	// BeforePowerAction can refuse a power action. It covers start, stop,
	// restart and kill, so switch on the action. Refusing a kill is allowed
	// but usually wrong: it is the escape hatch an operator reaches for when a
	// server is already stuck.
	BeforePowerAction func(Server, PowerAction) Decision

	// BeforeServerInstall can refuse an install or reinstall, which is the
	// hook to use to hold back a reinstall that would wipe files a plugin
	// cares about.
	BeforeServerInstall func(Server) Decision

	// BeforeServerDelete can refuse deleting a server from the node.
	BeforeServerDelete func(Server) Decision

	// BeforeServerTransfer can refuse a transfer of this server to another
	// node.
	BeforeServerTransfer func(Server) Decision

	// OnServerLifecycle observes everything that happens to a server: created
	// and deleted, install started, completed and failed, state changes,
	// crashes, transfers and finished restores. Switch on [Lifecycle].Event.
	// A finished backup comes through OnBackupCompleted instead, which can
	// carry the adapter and checksum.
	//
	// This is the hook for reacting to a server coming up or going down.
	OnServerLifecycle func(Lifecycle)

	// --- console and commands ---

	// OnConsoleLine sees each line of console output before it reaches the
	// Panel, and can rewrite or drop it. Returning the zero [ConsoleDirective]
	// leaves the line alone.
	//
	// This is a hot path: it runs for every line every server prints. Keep it
	// cheap, and prefer matching on a prefix over compiling a pattern per
	// call. Wings only delivers lines that survived console throttling.
	OnConsoleLine func(ConsoleLine) ConsoleDirective

	// OnCommand sees each console command before it reaches the server process
	// and can rewrite or refuse it. Returning the zero [CommandDirective]
	// lets the command through unchanged.
	OnCommand func(Command) CommandDirective

	// --- files ---

	// BeforeFileAction runs before a filesystem operation a user asked for,
	// and can allow it, refuse it, or carry it out itself. Only user-initiated
	// operations reach it, never the writes Wings performs for its own
	// bookkeeping.
	//
	// Taking the operation over is what [FileHandled] is for, and it is the
	// hook a recycle bin is built on: a plugin moves the file somewhere else
	// and reports the delete as handled, so the user sees their delete succeed
	// while the file still exists.
	BeforeFileAction func(FileEvent) FileDirective

	// OnFileAction observes a filesystem operation after it succeeded.
	OnFileAction func(FileEvent)

	// --- container ---

	// MutateContainer adjusts the container Wings is about to create for a
	// server. It runs on every boot, not only the first, and the patch it
	// returns is merged over what Wings built.
	//
	// Wings reapplies its own security settings after every plugin has had a
	// turn, so a patch cannot weaken container isolation. See [ContainerPatch]
	// for what that rules out.
	MutateContainer func(ContainerSpec) ContainerPatch

	// MutateStartup adjusts the command and environment a server is about to
	// boot with.
	//
	// This runs on every boot, which is the difference that matters between it
	// and MutateContainer: a container is only built once and then reused, so
	// a change made there does not follow a server's configuration, while a
	// change made here applies every time the process starts. It is the hook
	// for adding JVM flags, pinning a variable, or rewriting a startup command
	// the egg cannot express.
	MutateStartup func(Server, Startup) StartupPatch

	// BeforeCrashRestart decides whether Wings restarts a server that stopped
	// unexpectedly. Denying leaves it offline.
	//
	// Wings already declines to restart a server that crashed again too soon,
	// so this is for policy it cannot know: a maintenance window, a server
	// that should page someone instead, or a crash whose logs say restarting
	// will not help.
	BeforeCrashRestart func(CrashInfo) Decision

	// OnStats observes a server's resource usage, sampled on the same interval
	// Wings reports it to the Panel.
	//
	// This fires for every running server for as long as it runs, so it is the
	// second hottest hook after console output. Keep it cheap, and do not call
	// back into the node from it on every sample.
	OnStats func(Server, Stats)

	// BeforeBackup can refuse a backup before any files are read.
	BeforeBackup func(BackupRequest) Decision

	// OnBackupCompleted observes a finished backup, successful or not.
	OnBackupCompleted func(BackupOutcome)

	// OnSftpAuth can refuse an SFTP session after the Panel has accepted the
	// credentials.
	//
	// The Panel has already checked who they are and what they may do, so this
	// is for a policy the Panel has no way to express, such as which addresses
	// may connect. Refusing here fails the login.
	OnSftpAuth func(SftpAuth) Decision

	// OnInstallOutput observes a server's installation script output, line by
	// line, the way OnConsoleLine observes a running server. Unlike console
	// output it cannot be rewritten or dropped: the install log is the record
	// of what the script actually did.
	OnInstallOutput func(InstallLine)

	// Diagnostics contributes a section to the node's diagnostics report, as a
	// set of labelled values.
	//
	// Whatever is returned is included when an administrator collects
	// diagnostics, so put in what someone debugging this plugin would want and
	// nothing they should not see. The report is meant to be shareable, so a
	// plugin must not return credentials here.
	Diagnostics func() map[string]string

	// --- extensions ---

	// ConfigParsers registers egg configuration file formats, keyed by the
	// format name an egg refers to. Registering a name Wings already handles,
	// such as "yaml" or "properties", overrides the built-in for servers on
	// this node, which is occasionally what you want and usually not.
	//
	// A parser receives the file as it is on disk along with the replacements
	// the egg asked for, and returns the rewritten file.
	ConfigParsers map[string]func(ConfigFile) ([]byte, error)

	// Backup registers a backup adapter under a name the Panel can then ask
	// for, alongside the built-in "wings" and "s3".
	Backup *BackupAdapter

	// Routes registers HTTP endpoints. Each is mounted under
	// /api/plugins/<plugin-id>/http, or under
	// /api/servers/<server>/plugins/<plugin-id>/http when server scoped, and
	// inherits the same authentication every other Wings endpoint uses, so the
	// Panel can call them with the node token.
	//
	// A plugin declaring Path "/stats" is therefore reached at
	// /api/plugins/<plugin-id>/http/stats. The "/http" segment keeps plugin
	// routes clear of the management endpoints, so a route named "enable"
	// cannot shadow the one that enables the plugin.
	Routes []Route

	// OnWebsocketOpen observes a console connection being opened, and
	// OnWebsocketClose the same connection going away. Together they are what
	// a plugin needs to show who is watching a server, or to audit console
	// access.
	OnWebsocketOpen  func(WebsocketConnection)
	OnWebsocketClose func(WebsocketConnection)

	// OnWebsocketMessage handles console websocket messages Wings does not
	// recognise, which is how a plugin's Panel-side UI talks to its Wings side
	// over the socket the user already has open.
	OnWebsocketMessage func(WebsocketMessage) []WebsocketReply

	// Jobs registers recurring work on the node's scheduler.
	Jobs []Job
}

// FileDirective is the outcome of a file gate. The zero value allows the
// operation, so a hook that only cares about deletes can return it for
// everything else. Build one with [AllowFile], [DenyFile] or [FileHandled].
type FileDirective struct {
	// Deny refuses the operation, and Reason is shown to the user who asked
	// for it.
	Deny   bool
	Reason string

	// Handled says the plugin has already carried out the operation, or has
	// deliberately done something else instead, and Wings should not perform
	// it. The user is told the operation succeeded.
	//
	// A plugin setting this takes on the whole responsibility for the
	// operation: if it reports a delete as handled and does not actually move
	// or remove the file, the file stays exactly where it was and the user
	// will be told it is gone.
	Handled bool
}

// AllowFile lets the operation proceed.
func AllowFile() FileDirective { return FileDirective{} }

// DenyFile refuses the operation and tells the user why.
func DenyFile(reason string) FileDirective {
	return FileDirective{Deny: true, Reason: reason}
}

// FileHandled reports that the plugin performed the operation itself, so Wings
// should skip it and treat it as successful.
func FileHandled() FileDirective { return FileDirective{Handled: true} }

// ConsoleDirective tells Wings what to do with a line of console output. The
// zero value leaves the line alone, so a hook that only watches output can
// return it. Build one with [KeepLine], [RewriteLine] or [DropLine].
type ConsoleDirective struct {
	// Rewrite is the replacement line, used only when Rewritten is set. An
	// empty Rewrite with Rewritten set delivers an empty line, which is
	// different from dropping it.
	Rewrite   string
	Rewritten bool

	// Drop suppresses the line: nobody watching the console sees it. It still
	// reaches the server's own log file on disk, because that is the record of
	// what the process actually printed.
	Drop bool
}

// KeepLine delivers the line unchanged.
func KeepLine() ConsoleDirective { return ConsoleDirective{} }

// RewriteLine replaces the line with s.
func RewriteLine(s string) ConsoleDirective {
	return ConsoleDirective{Rewrite: s, Rewritten: true}
}

// DropLine suppresses the line.
func DropLine() ConsoleDirective { return ConsoleDirective{Drop: true} }

// CommandDirective tells Wings what to do with a console command. The zero
// value lets it through unchanged. Build one with [AllowCommand],
// [RewriteCommand] or [DenyCommand].
type CommandDirective struct {
	// Rewrite is the replacement command, used only when Rewritten is set.
	Rewrite   string
	Rewritten bool

	// Deny refuses the command, and Reason is shown to the user who sent it.
	Deny   bool
	Reason string
}

// AllowCommand lets the command through unchanged.
func AllowCommand() CommandDirective { return CommandDirective{} }

// RewriteCommand replaces the command with s.
func RewriteCommand(s string) CommandDirective {
	return CommandDirective{Rewrite: s, Rewritten: true}
}

// DenyCommand refuses the command and tells the user why.
func DenyCommand(reason string) CommandDirective {
	return CommandDirective{Deny: true, Reason: reason}
}

// BackupAdapter is a backup destination a plugin provides, such as another
// object store or a snapshotting filesystem.
//
// The adapter is responsible for where the data lives; Wings still owns walking
// the server's files, honouring the ignore list and reporting the result to the
// Panel. Create is handed a reader over the archive Wings produced, so an
// adapter that only needs somewhere to put bytes does not have to know anything
// about archives.
type BackupAdapter struct {
	// Name is what the Panel asks for when it wants a backup on this adapter.
	// It must not be "wings" or "s3".
	Name string

	// Create stores a new backup and reports its size and checksum. Wings has
	// already written the archive to the node's temporary directory; the path
	// is in [BackupPayload].Path.
	Create func(Backup, BackupPayload) (BackupResult, error)

	// Restore puts a backup's contents back into the server's data directory.
	// It is given a writer-side callback per file so a restore can stream
	// rather than stage the whole archive.
	Restore func(Backup, RestoreWriter) error

	// Delete removes a stored backup. It must succeed when the backup is
	// already gone, so a retried delete does not wedge the Panel.
	Delete func(Backup) error

	// Size reports a stored backup's size in bytes. Optional: Wings falls back
	// to what Create reported when this is nil.
	Size func(Backup) (int64, error)
}

// BackupPayload is the archive Wings built, handed to a [BackupAdapter] Create
// hook.
type BackupPayload struct {
	// Path is the archive on the node's local disk. It is deleted after Create
	// returns, so copy or upload it rather than keeping the path.
	Path string

	// SizeBytes is the archive size.
	SizeBytes int64

	// Checksum is the archive's SHA1, already computed by Wings.
	Checksum string
}

// RestoreWriter is how a [BackupAdapter] Restore hook puts files back. Calling
// it writes one file into the server's data directory, with the same path
// checks and disk accounting a user's upload gets.
type RestoreWriter interface {
	// Write restores a single file. Path is relative to the server data
	// directory.
	Write(path string, content []byte) error

	// Progress reports how far along the restore is, which the Panel shows to
	// the user. Both counts are in bytes.
	Progress(done, total int64)
}

// Route is an HTTP endpoint a plugin registers.
type Route struct {
	// Method is the HTTP method, such as "GET" or "POST".
	Method string

	// Path is relative to the plugin's mount point and must begin with a
	// slash. Gin's ":param" and "*wildcard" syntax works here.
	Path string

	// ServerScoped mounts the route under a server rather than the node, at
	// /api/servers/:server/plugins/<plugin-id><path>, and populates
	// [Request].Server. Wings resolves the server and returns 404 itself, so
	// the handler never sees a request for a server that is not here.
	ServerScoped bool

	// Handler answers the request. A panic inside it becomes a 500 and marks
	// the plugin errored rather than taking the daemon down.
	Handler func(Request) Response
}

// Job is recurring work on the node's scheduler.
type Job struct {
	// Name identifies the job in logs. It is scoped to the plugin, so it only
	// has to be unique within one plugin.
	Name string

	// Interval is how often Run is called. Wings enforces a floor of one
	// second, and the first run happens one interval after the plugin is
	// enabled rather than immediately.
	Interval time.Duration

	// Run does the work. Runs never overlap: if one is still going when the
	// next is due, Wings skips that tick and logs it. Returning an error logs
	// it against the plugin without disabling it.
	Run func() error
}

// WebsocketReply is a message sent back over a server's console websocket in
// response to a [WebsocketMessage].
type WebsocketReply struct {
	// Event is the event name the client will see. Prefix it with the plugin's
	// id so it cannot be mistaken for a Wings event.
	Event string

	// Args are the arguments delivered to the client.
	Args []string

	// Broadcast sends the reply to every socket open on the server rather than
	// only the one that sent the message.
	Broadcast bool
}

// SettingType is the kind of input the Panel renders for a declared setting.
type SettingType string

const (
	SettingString  SettingType = "string"
	SettingText    SettingType = "text"
	SettingNumber  SettingType = "number"
	SettingBoolean SettingType = "boolean"
	SettingSelect  SettingType = "select"
	SettingSecret  SettingType = "secret"
)

// Setting is one configuration value a plugin accepts.
//
// Settings are declared here, in the plugin's own code, next to the routes and
// jobs it registers, rather than in its manifest. The manifest describes what
// the plugin is; this describes what it does, and a setting the plugin reads
// belongs with the code that reads it.
//
// Wings records the declared schema when the plugin loads, so the Panel can
// still render a form for a plugin that is currently disabled.
type Setting struct {
	// Key is how the plugin reads the value back through [Settings].
	Key string

	// Label is the field name shown to the operator. Falls back to the key.
	Label string

	// Description is help text shown under the field.
	Description string

	// Type decides the input rendered. An empty Type is treated as a string.
	Type SettingType

	// Default is used until an operator saves something else.
	Default any

	// Required marks a field the Panel will not let an operator leave empty.
	// Wings does not enforce it, so a plugin should still cope with a missing
	// value.
	Required bool

	// Options are the choices for a select, and are ignored otherwise.
	Options []SettingOption
}

// SettingOption is one choice for a select setting.
type SettingOption struct {
	Value any
	Label string
}

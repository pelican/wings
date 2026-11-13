package api

// Version is the plugin API contract version implemented by this package.
//
// A manifest may declare an api_version. If it declares one higher than this,
// Wings refuses to load the plugin instead of failing halfway through. Bump
// this only when an existing field changes meaning or disappears; adding a new
// optional hook to Registration is backwards compatible and does not need it.
const Version = 1

// Decision is the result of a gating hook. The zero value denies, so a hook
// that falls through every branch without returning fails closed. Use [Allow]
// and [Deny] rather than building one by hand.
type Decision struct {
	// Allow reports whether the action may proceed.
	Allow bool

	// Reason explains a denial. It is surfaced to the user who triggered the
	// action and written to the server console, so write it for them rather
	// than for a log file.
	Reason string
}

// Allow permits the action.
func Allow() Decision { return Decision{Allow: true} }

// Deny blocks the action and reports reason back to the caller.
func Deny(reason string) Decision { return Decision{Allow: false, Reason: reason} }

// PowerAction is a power state transition requested for a server.
type PowerAction string

const (
	PowerStart   PowerAction = "start"
	PowerStop    PowerAction = "stop"
	PowerRestart PowerAction = "restart"
	PowerKill    PowerAction = "kill"
)

// Process states a server can be in. These match the states Wings reports to
// the Panel.
const (
	StateOffline  = "offline"
	StateStarting = "starting"
	StateRunning  = "running"
	StateStopping = "stopping"
)

// Allocation is a single IP and port pair assigned to a server.
type Allocation struct {
	IP   string
	Port int
}

// Limits are the resource limits the Panel assigned to a server. Memory, swap
// and disk are in mebibytes, matching what the Panel sends.
type Limits struct {
	MemoryMiB int64
	SwapMiB   int64
	DiskMiB   int64
	IOWeight  uint16

	// CPUPercent is relative to a single core, so 200 means two full cores.
	// Zero means unlimited.
	CPUPercent int64

	// Threads pins the container to specific CPU threads, in Docker's cpuset
	// syntax. Empty means unpinned.
	Threads string

	// OOMKiller reports whether the kernel OOM killer is left enabled for the
	// container.
	OOMKiller bool
}

// Mount is a bind mount attached to a server's container.
type Mount struct {
	Source   string
	Target   string
	ReadOnly bool

	// Default marks the mounts Wings creates itself, such as the server data
	// directory. Plugins should leave these alone.
	Default bool
}

// Server is a snapshot of one server at the moment a hook fired. It is a copy:
// changing it has no effect on the daemon. Act through [Host] instead.
type Server struct {
	// UUID is the server's identifier on the Panel and in Wings, and the name
	// of its Docker container.
	UUID string

	// ID is the Panel's numeric database id for the server.
	ID int

	Name        string
	Description string

	// State is one of the State constants in this package.
	State string

	Suspended bool

	// Invocation is the fully parsed startup command, after variable
	// substitution.
	Invocation string

	// Image is the Docker image the server runs on.
	Image string

	// EggID is the UUID of the Panel egg this server was created from. Use it
	// to scope a plugin to one game.
	EggID string

	// Allocation is the server's primary IP and port, the one exposed as
	// SERVER_IP and SERVER_PORT.
	Allocation Allocation

	// Allocations is every allocation assigned to the server, including the
	// primary one.
	Allocations []Allocation

	Limits Limits

	// Labels are the container labels configured for the server on the Panel.
	Labels map[string]string

	// Environment is the server's environment variables, as they are passed to
	// the container.
	Environment map[string]string

	Mounts []Mount

	// FileDenylist is the set of paths the egg forbids all access to. A plugin
	// granting file access to anyone must honour it.
	FileDenylist []string

	// Installing, Restoring and Transferring report that the daemon is already
	// doing something lengthy to this server.
	//
	// A plugin should check these before acting: writing files into a server
	// that is mid-install races the installer, and a power action during a
	// transfer is refused anyway. The State field says what the process is
	// doing, which is a different question from what the daemon is.
	Installing   bool
	Restoring    bool
	Transferring bool
}

// ConsoleLine is one line of output from a server process.
type ConsoleLine struct {
	Server Server

	// Line is the output, with ANSI escapes left intact. Wings has already
	// decided this line is worth delivering, so throttled output never
	// reaches a hook.
	Line string
}

// Command is a console command on its way to a server process.
type Command struct {
	Server Server

	// Command is the command text, without a trailing newline.
	Command string

	// User is the Panel user who sent it, or empty when Wings itself did, such
	// as the stop command sent during a graceful shutdown.
	User string
}

// FileAction is the kind of filesystem operation a [FileEvent] describes.
type FileAction string

const (
	FileRead       FileAction = "read"
	FileWrite      FileAction = "write"
	FileDelete     FileAction = "delete"
	FileRename     FileAction = "rename"
	FileCopy       FileAction = "copy"
	FileChmod      FileAction = "chmod"
	FileCompress   FileAction = "compress"
	FileDecompress FileAction = "decompress"
	FileCreateDir  FileAction = "create-directory"
	FileUpload     FileAction = "upload"
	FileDownload   FileAction = "download"
	FilePullRemote FileAction = "pull-remote"
)

// FileEvent describes a filesystem operation a user asked for, before it runs.
//
// Only operations that originate from a user reach this hook: the Panel file
// manager, SFTP, uploads and downloads. Writes Wings performs for its own
// reasons, such as rendering egg configuration files on boot or unpacking a
// backup during a restore, are not reported, because a plugin blocking those
// would break the daemon rather than protect it.
type FileEvent struct {
	Server Server
	Action FileAction

	// Path is relative to the server's data directory, always beginning with a
	// slash.
	Path string

	// Target is the destination for a rename or copy, and empty otherwise.
	Target string

	// Size is the byte count for a write or upload, and -1 when not known
	// ahead of time.
	Size int64

	// Directory reports whether Path is a directory. A plugin mirroring paths
	// somewhere else needs this to know whether to recreate a folder or a
	// file, and it is not always cheap to stat for it afterwards.
	Directory bool

	// User is the Panel user who triggered the operation, or empty for SFTP
	// and signed-URL access where no user is attached.
	User string

	// IP is the address the request came from.
	IP string
}

// LifecycleEvent is a point in a server's life that a plugin can observe.
type LifecycleEvent string

const (
	ServerCreated          LifecycleEvent = "created"
	ServerDeleted          LifecycleEvent = "deleted"
	ServerInstallStarted   LifecycleEvent = "install-started"
	ServerInstallCompleted LifecycleEvent = "install-completed"
	ServerInstallFailed    LifecycleEvent = "install-failed"
	ServerStateChanged     LifecycleEvent = "state-changed"
	ServerCrashed          LifecycleEvent = "crashed"
	ServerTransferStarted  LifecycleEvent = "transfer-started"
	ServerTransferComplete LifecycleEvent = "transfer-completed"

	// ServerReconfigured reports that the Panel pushed new settings for a
	// server: limits changed, the egg swapped, it was suspended or resumed.
	// The snapshot carries the new state; a plugin that cares what changed
	// should keep its own copy of what it last saw.
	ServerReconfigured LifecycleEvent = "reconfigured"

	// ServerBackupRestored reports a finished restore. There is deliberately
	// no matching event for a finished backup: OnBackupCompleted carries the
	// adapter, size and checksum, which a lifecycle event has nowhere to put.
	ServerBackupRestored LifecycleEvent = "backup-restored"
)

// Lifecycle is an observation of something that happened to a server. It is
// delivered after the fact and cannot be vetoed.
type Lifecycle struct {
	Server Server
	Event  LifecycleEvent

	// PreviousState and NewState are set for ServerStateChanged.
	PreviousState string
	NewState      string

	// ExitCode and OOMKilled are set for ServerCrashed.
	ExitCode  int
	OOMKilled bool

	// Error carries the failure message for the failed variants.
	Error string
}

// ContainerSpec is the container Wings is about to create, in the shape a
// plugin is allowed to influence. It is passed to a mutation hook along with
// the server, and the hook returns a [ContainerPatch].
type ContainerSpec struct {
	Server Server

	Image  string
	Labels map[string]string
	Env    map[string]string
	Mounts []Mount
}

// ContainerPatch is what a container mutation hook asks Wings to change. Every
// field is additive or empty-means-no-change, so a hook only states what it
// cares about and never has to reproduce the rest of the container.
//
// Wings applies patches from each plugin in load order and always reapplies its
// own security settings afterwards. A plugin cannot drop the no-new-privileges
// flag, re-add a dropped capability, make the root filesystem writable, or
// mount a path the node has not allowed, because those are the invariants that
// keep one server from reaching another.
type ContainerPatch struct {
	// Image replaces the container image when non-empty.
	Image string

	// Labels are merged over the existing labels.
	Labels map[string]string

	// Env entries are merged over the existing environment.
	Env map[string]string

	// Mounts are appended. Each source must already be inside the node's
	// allowed_mounts, or Wings drops it and logs a warning.
	Mounts []Mount

	// Sysctls are applied to the container.
	Sysctls map[string]string

	// Devices are host devices to expose, in Docker's
	// "/dev/host:/dev/container:rwm" syntax. Ignored unless the node config
	// permits plugin devices.
	Devices []string

	// ExtraHosts are appended to the container's /etc/hosts as
	// "hostname:address".
	ExtraHosts []string

	// ShmSizeBytes overrides the container's /dev/shm size when greater than
	// zero.
	ShmSizeBytes int64
}

// BackupKind identifies a backup adapter a plugin provides. Wings ships "wings"
// for local archives and "s3"; a plugin declaring a backup adapter registers a
// third name, which the Panel can then request.
type BackupKind string

// Backup identifies one backup for an adapter hook.
type Backup struct {
	Server Server

	// UUID is the backup's identifier on the Panel.
	UUID string

	// Ignore is the ignore file content, in gitignore syntax, listing what to
	// leave out of the archive.
	Ignore string
}

// BackupResult is what a create hook reports back so the Panel can record the
// backup.
type BackupResult struct {
	// SizeBytes is the size of the stored archive.
	SizeBytes int64

	// Checksum is the archive's checksum, and ChecksumType names the algorithm,
	// normally "sha1".
	Checksum     string
	ChecksumType string
}

// ConfigFile is an egg configuration file a parser hook is asked to rewrite.
type ConfigFile struct {
	Server Server

	// Path is the file, relative to the server data directory.
	Path string

	// Content is the file as it exists now. A parser returns the rewritten
	// bytes.
	Content []byte

	// Replacements are the substitutions the egg asked for, as a mapping of
	// dotted key path to the value to set.
	Replacements map[string]string
}

// Request is an HTTP request delivered to a route a plugin registered.
type Request struct {
	Method string

	// Path is the portion of the URL after the plugin's own prefix, so a
	// plugin registering "/stats" sees "/" here.
	Path string

	Query  map[string][]string
	Header map[string][]string
	Body   []byte

	// Server is set when the route was registered under a server, and is the
	// zero value for node level routes.
	Server Server

	// ServerScoped reports whether Server is meaningful.
	ServerScoped bool
}

// Response is what a plugin returns from an HTTP route. A zero Status is sent
// as 200.
type Response struct {
	Status int
	Header map[string][]string
	Body   []byte
}

// JSON builds a response carrying a JSON body.
func JSON(status int, body []byte) Response {
	return Response{
		Status: status,
		Header: map[string][]string{"Content-Type": {"application/json"}},
		Body:   body,
	}
}

// Text builds a response carrying a plain text body.
func Text(status int, body string) Response {
	return Response{
		Status: status,
		Header: map[string][]string{"Content-Type": {"text/plain; charset=utf-8"}},
		Body:   []byte(body),
	}
}

// WebsocketMessage is a message received on a server's console websocket that
// Wings itself does not recognise, handed to a plugin so the Panel side of that
// plugin can talk to the Wings side over the connection the user already has
// open.
type WebsocketMessage struct {
	Server Server

	// Event is the message's event name. Wings only delivers events it does
	// not handle, so a plugin cannot shadow a built-in one.
	Event string

	// Args are the message arguments as sent by the client.
	Args []string

	// User is the Panel user on the other end of the socket.
	User string

	// Permissions are the permissions that user holds for this server, as
	// granted by the token the socket was opened with.
	Permissions []string
}

// DirEntry describes one entry returned by [Host] directory listing.
type DirEntry struct {
	Name      string
	Size      int64
	Mode      uint32
	Directory bool
	Symlink   bool
	MimeType  string

	// ModifiedUnix is the modification time in seconds since the epoch.
	ModifiedUnix int64
}

// Node describes the Wings instance the plugin is running on.
type Node struct {
	// UUID is the node's identifier on the Panel.
	UUID string

	// Version is the running Wings version.
	Version string

	// PanelURL is the Panel this node is attached to.
	PanelURL string

	// Architecture and OS describe the host, such as "amd64" and "linux".
	Architecture string
	OS           string

	CPUThreads  int
	MemoryBytes int64
}

// Startup is the command and environment a server is about to boot with.
//
// This is the parsed form, after the Panel's variables have been substituted,
// which is what the container actually receives. A plugin adjusting it is
// changing what the process runs, not what the Panel has on file.
type Startup struct {
	// Command is the fully parsed startup command.
	Command string

	// Environment is every variable the container will be given, including the
	// ones Wings sets itself such as SERVER_IP, SERVER_PORT and SERVER_MEMORY.
	Environment map[string]string
}

// StartupPatch is what a startup hook asks Wings to change. The zero value
// changes nothing.
//
// Unlike a container patch, this is applied on every boot rather than only when
// the container is created, which makes it the right place for anything that
// has to follow the server's current configuration.
type StartupPatch struct {
	// Command replaces the startup command when non-empty.
	Command string

	// Environment entries are merged over what Wings built. Setting a variable
	// to an empty string sets it empty; there is no way to remove one, because
	// a server missing a variable its egg expects fails in confusing ways.
	Environment map[string]string
}

// Stats is a sample of a server's resource usage, taken on the same interval
// Wings reports usage to the Panel.
type Stats struct {
	// MemoryBytes is current usage and MemoryLimitBytes the ceiling, which
	// includes the overhead Wings adds on top of the Panel's limit.
	MemoryBytes      uint64
	MemoryLimitBytes uint64

	// CPUPercent is usage against the whole host, ignoring the server's own
	// limit, so 200 means two full cores on any machine.
	CPUPercent float64

	// DiskBytes is the server's current disk usage.
	DiskBytes int64

	// Network counters are cumulative since the container started. Derive a
	// rate from the difference between two samples rather than reading these
	// as throughput.
	NetworkRxBytes uint64
	NetworkTxBytes uint64

	// UptimeMillis is how long the container has been running.
	UptimeMillis int64
}

// CrashInfo describes a server that has just stopped unexpectedly, offered to a
// plugin before Wings decides whether to restart it.
type CrashInfo struct {
	Server Server

	ExitCode  int
	OOMKilled bool

	// LastCrashUnix is when this server last crashed, in seconds since the
	// epoch, or zero if this is the first. Use it to tell a one-off from a
	// server stuck in a boot loop.
	LastCrashUnix int64

	// Logs are the last lines the process printed before it went, which is
	// usually where the reason is.
	Logs []string
}

// BackupRequest is a backup about to be taken.
type BackupRequest struct {
	Server Server

	// UUID is the backup's identifier on the Panel.
	UUID string

	// Adapter is where it will be stored: "wings", "s3", or a name a plugin
	// registered.
	Adapter string

	// Ignore is the ignore file content, in gitignore syntax.
	Ignore string
}

// BackupOutcome is a finished backup, successful or not.
type BackupOutcome struct {
	Server  Server
	UUID    string
	Adapter string

	Successful bool

	// SizeBytes and Checksum describe the stored archive, and are only
	// meaningful when Successful is set.
	SizeBytes int64
	Checksum  string

	// Error is why it failed.
	Error string
}

// SftpAuth is an SFTP login attempt, offered to a plugin after the Panel has
// accepted the credentials and before the session is allowed to start.
//
// The Panel has already decided the credentials are valid and which server they
// belong to, so this is the place to apply a policy the Panel does not know
// about, such as restricting where a user may connect from.
type SftpAuth struct {
	// User is the Panel user's UUID.
	User string

	// Username is what they typed, which is the Panel username and server
	// joined by a dot.
	Username string

	// IP is the address the connection came from.
	IP string

	// ServerUUID is the server the credentials resolved to.
	ServerUUID string

	// PublicKey reports whether they authenticated with a key rather than a
	// password.
	PublicKey bool

	// Permissions are the permissions the Panel granted for this session.
	Permissions []string
}

// InstallLine is one line of output from a server's installation script.
type InstallLine struct {
	Server Server
	Line   string
}

// ExecOptions bounds a command run inside a server's container.
type ExecOptions struct {
	// TimeoutSeconds stops waiting for the command. Defaults to 30.
	//
	// The process itself is not killed when this elapses, because Docker
	// offers no way to signal an exec. It is left running and its output
	// discarded, so a command that might not terminate is a poor fit for this.
	TimeoutSeconds int

	// MaxOutputBytes caps stdout and stderr together. Defaults to 256 KiB.
	MaxOutputBytes int64

	// WorkingDir runs the command somewhere other than the container's
	// default directory.
	WorkingDir string
}

// ExecResult is what a command left behind.
type ExecResult struct {
	Stdout string
	Stderr string

	// ExitCode is the command's own exit status.
	ExitCode int

	// Truncated reports that output hit the cap, so what is here is a prefix
	// rather than the whole of it.
	Truncated bool
}

// WebsocketConnection describes a console connection opening or closing.
type WebsocketConnection struct {
	Server Server

	// User is the Panel user on the other end.
	User string

	// IP is where they connected from.
	IP string

	// Permissions are what the token granted them for this server.
	Permissions []string
}

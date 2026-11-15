// Package external adapts a manifest-described plugin subprocess into a native
// capability provider by speaking JSON-RPC over the process's stdio. Processes
// are spawned lazily on first use and respawned (with a backoff) after a crash.
package external

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/apex/log"

	"github.com/pelican/wings/plugin"
	"github.com/pelican/wings/plugin/jsonrpc"
)

const (
	initTimeout    = 5 * time.Second
	respawnBackoff = 3 * time.Second
)

// Client manages a single external plugin process. It is safe for concurrent
// use; the process is (re)spawned on demand.
type Client struct {
	manifest plugin.Manifest
	handler  jsonrpc.Handler // inbound host-callback handler; nil rejects every call

	mu        sync.Mutex
	conn      *jsonrpc.Conn
	lastSpawn time.Time
}

// NewClient returns a client for a discovered manifest. handler receives inbound
// host-callback requests issued by the plugin.
func NewClient(m plugin.Manifest, handler jsonrpc.Handler) *Client {
	return &Client{manifest: m, handler: handler}
}

type initParams struct {
	Host    string `json:"host"`
	Version int    `json:"version"`
}

type initResult struct {
	Name       string `json:"name"`
	Capability string `json:"capability"`
}

// ensureSession returns a live connection, spawning and initializing the process
// if necessary. A minimum backoff between spawns avoids hammering a plugin that
// crashes on startup.
func (c *Client) ensureSession(ctx context.Context) (*jsonrpc.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn != nil {
		select {
		case <-c.conn.Done():
			c.conn = nil // process died; fall through to respawn
		default:
			return c.conn, nil
		}
	}
	if !c.lastSpawn.IsZero() && time.Since(c.lastSpawn) < respawnBackoff {
		return nil, fmt.Errorf("plugin %q: in respawn backoff", c.manifest.Name)
	}
	c.lastSpawn = time.Now()

	cmd, err := c.command()
	if err != nil {
		return nil, err
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("plugin %q: start: %w", c.manifest.Name, err)
	}
	stderrDone := make(chan struct{})
	go func() {
		pipeStderr(c.manifest.Name, stderr)
		close(stderrDone)
	}()

	conn := jsonrpc.NewConn(stdout, stdin, c.handler, processCloser{cmd: cmd, stderrDone: stderrDone})

	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	var res initResult
	if err := conn.Call(ictx, "initialize", initParams{Host: "wings", Version: 1}, &res); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("plugin %q: initialize: %w", c.manifest.Name, err)
	}
	c.conn = conn
	log.WithField("plugin", c.manifest.Name).Info("external plugin initialized")
	return conn, nil
}

// command builds the plugin's process: in its own directory, with its data
// directory ready, and as the user plugins run as (plugin.ExternalProcessUser).
// When Wings is root that is system.user, and the plugin's own files must be
// read-only to it.
func (c *Client) command() (*exec.Cmd, error) {
	u, err := plugin.ExternalProcessUser()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: %w", c.manifest.Name, err)
	}
	argv := c.manifest.Entrypoint
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = c.manifest.Dir()
	cmd.Env = append(pluginEnv(os.Environ()), "WINGS_PLUGIN=1")
	dir := c.manifest.DataDir()
	if dir != "" {
		if err := plugin.PrepareDataDir(dir, u); err != nil {
			return nil, fmt.Errorf("plugin %q: preparing data directory: %w", c.manifest.Name, err)
		}
		cmd.Env = append(cmd.Env, "WINGS_PLUGIN_DATA="+dir)
	}
	if !u.Switch {
		return cmd, nil
	}
	if err := plugin.CheckReadOnlyTo(c.manifest.Dir(), u); err != nil {
		return nil, fmt.Errorf("plugin %q: %w", c.manifest.Name, err)
	}
	if err := plugin.CheckReachableBy(c.manifest.Dir(), u); err != nil {
		return nil, fmt.Errorf("plugin %q: %w", c.manifest.Name, err)
	}
	uid, gid, err := u.IDs()
	if err != nil {
		return nil, fmt.Errorf("plugin %q: %w", c.manifest.Name, err)
	}
	// No supplementary groups: root's would otherwise carry over.
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: uid, Gid: gid, Groups: []uint32{},
	}}
	// Root's HOME isn't the plugin's; its data directory is the one place it
	// can write.
	home := dir
	if home == "" {
		home = "/"
	}
	cmd.Env = setEnv(cmd.Env, "HOME", home)
	cmd.Env = setEnv(cmd.Env, "USER", u.Name)
	cmd.Env = setEnv(cmd.Env, "LOGNAME", u.Name)
	return cmd, nil
}

// pluginEnv keeps the part of Wings' environment a plugin needs to run:
// PATH, locale, time zone, TLS certificate locations and proxy settings.
// Everything else stays with Wings, above all WINGS_TOKEN (the node's
// credential for its own HTTP API) and registry tokens referenced as ${NAME}.
func pluginEnv(environ []string) []string {
	keep := map[string]bool{
		"PATH": true, "LANG": true, "LANGUAGE": true, "TZ": true,
		"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
		"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
		"http_proxy": true, "https_proxy": true, "no_proxy": true,
	}
	out := make([]string, 0, len(keep))
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "LC_") {
			out = append(out, kv)
		}
	}
	return out
}

// setEnv sets key=value in env, replacing any earlier value.
func setEnv(env []string, key, value string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

// EventListener returns a plugin.EventListener backed by this client. Events are
// delivered to the plugin as JSON-RPC notifications.
func (c *Client) EventListener() plugin.EventListener { return eventListener{c} }

func (c *Client) emit(ctx context.Context, e plugin.Event) error {
	conn, err := c.ensureSession(ctx)
	if err != nil {
		return err
	}
	return conn.Notify("event", e)
}

type eventListener struct{ c *Client }

func (s eventListener) Name() string { return s.c.manifest.Name }
func (s eventListener) Events() []string {
	return plugin.EffectiveEvents(s.c.manifest)
}
func (s eventListener) OnEvent(ctx context.Context, e plugin.Event) error { return s.c.emit(ctx, e) }

// stderrDrainTimeout bounds how long Close waits for the stderr reader after
// killing the plugin; a child process the plugin started can keep the pipe open.
const stderrDrainTimeout = 2 * time.Second

type processCloser struct {
	cmd        *exec.Cmd
	stderrDone <-chan struct{}
}

func (p processCloser) Close() error {
	if p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	// Wait closes the stderr pipe, and os/exec says not to call it before all
	// reads from the pipe have completed: whatever the plugin wrote last (often
	// why it crashed) would be lost. Let the reader finish first.
	if p.stderrDone != nil {
		select {
		case <-p.stderrDone:
		case <-time.After(stderrDrainTimeout):
		}
	}
	return p.cmd.Wait()
}

func pipeStderr(name string, r io.Reader) {
	l := log.WithField("plugin", name)
	// A plugin's stderr is its log, not a warning from Wings.
	pipeLines(r, maxStderrLine, func(line string) { l.Info(line) })
}

// maxStderrLine is the longest stderr line logged in full.
const maxStderrLine = 8 << 10

// pipeLines reads r until EOF and calls emit once per line. A line longer than
// max is cut to max bytes, marked, and the rest of it skipped. A bufio.Scanner
// would stop at its first too-long line instead, and then nothing would drain
// the pipe: the plugin's next stderr writes would block once the pipe filled.
func pipeLines(r io.Reader, max int, emit func(string)) {
	br := bufio.NewReaderSize(r, max)
	for {
		line, err := br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			emit(string(line) + " … (line truncated)")
			for err == bufio.ErrBufferFull {
				_, err = br.ReadSlice('\n')
			}
			if err != nil {
				return
			}
			continue
		}
		if len(line) > 0 {
			emit(strings.TrimRight(string(line), "\r\n"))
		}
		if err != nil {
			return
		}
	}
}

// LoadEnabled discovers manifests in dir and registers a provider for each
// enabled plugin according to its capability. Nothing is spawned until first
// use. api backs the host-callback handler each plugin receives, restricted to
// the capabilities that plugin was granted.
func LoadEnabled(dir string, api plugin.HostAPI) error {
	manifests, issues, err := plugin.DiscoverDetailed(dir)
	for _, issue := range issues {
		log.WithField("manifest", issue.Manifest).
			WithField("error", issue.Err).
			Warn("external plugin discovery issue")
	}

	trust, terr := plugin.LoadTrustStore()
	if terr != nil {
		log.WithField("error", terr).Warn("could not load plugin trust store; treating external plugins as untrusted")
	}

	for _, m := range manifests {
		if !plugin.Enabled(m.Name) {
			continue
		}
		// Workspace-Trust gate: an external plugin only loads once the operator
		// has explicitly approved its security-relevant manifest fields.
		if trust == nil || !trust.IsTrusted(m) {
			log.WithField("plugin", m.Name).
				Warn("external plugin is not trusted; run `wings plugins trust` to approve — skipping")
			continue
		}
		handler := plugin.HostHandler(api, plugin.GrantsFor(m))
		client := NewClient(m, handler)
		switch m.Capability {
		case plugin.CapabilityEvents:
			plugin.RegisterEventListener(client.EventListener())
		default:
			log.WithField("plugin", m.Name).
				WithField("capability", string(m.Capability)).
				Warn("unknown plugin capability; skipping")
			continue
		}
		log.WithField("plugin", m.Name).Debug("registered external plugin")
	}
	return err
}

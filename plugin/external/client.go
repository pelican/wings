// Package external adapts a manifest-described plugin subprocess into a native
// capability provider by speaking JSON-RPC over the process's stdio. Processes
// are spawned lazily on first use and respawned (with a backoff) after a crash.
package external

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	// verify, if set, runs before every process start; an error keeps the
	// process from starting. Reconcile uses it to check the plugin's files
	// still match its approval.
	verify func() error

	mu        sync.Mutex
	conn      *jsonrpc.Conn
	lastSpawn time.Time
	closed    bool
}

// errClosed is returned for a client that Close has stopped.
var errClosed = errors.New("plugin stopped")

// Close stops the plugin process, if it is running, and keeps the client from
// starting it again. Events still queued for the plugin are dropped.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
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

	if c.closed {
		return nil, fmt.Errorf("plugin %q: %w", c.manifest.Name, errClosed)
	}
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
	if c.verify != nil {
		if err := c.verify(); err != nil {
			return nil, fmt.Errorf("plugin %q: not starting it: %w", c.manifest.Name, err)
		}
	}

	argv := c.manifest.Entrypoint
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = c.manifest.Dir()
	cmd.Env = append(os.Environ(), "WINGS_PLUGIN=1")
	if dir := c.manifest.DataDir(); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("plugin %q: creating data directory: %w", c.manifest.Name, err)
		}
		cmd.Env = append(cmd.Env, "WINGS_PLUGIN_DATA="+dir)
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

// EventListener returns a plugin.EventListener backed by this client. Events are
// delivered to the plugin as JSON-RPC notifications.
func (c *Client) EventListener() plugin.EventListener { return eventListener{c} }

func (c *Client) emit(ctx context.Context, e plugin.Event) error {
	conn, err := c.ensureSession(ctx)
	if errors.Is(err, errClosed) {
		return nil // stopped on purpose; nothing to report
	}
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

var _ plugin.ExternalListener = eventListener{}

// External marks this listener as an external plugin (plugin.ExternalListener).
func (eventListener) External() bool { return true }

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
	pipeLines(r, maxStderrLine, func(line string) { l.Warn(line) })
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
// enabled, trusted plugin according to its capability. Nothing is spawned
// until first use. api backs the host-callback handler each plugin receives,
// restricted to the capabilities that plugin was granted. It is Reconcile with
// nothing loaded yet.
func LoadEnabled(dir string, api plugin.HostAPI) error {
	return Reconcile(dir, api)
}

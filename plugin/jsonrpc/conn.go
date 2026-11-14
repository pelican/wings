// Package jsonrpc implements a minimal bidirectional JSON-RPC 2.0 connection
// over a byte stream (typically a plugin subprocess's stdio). Messages are
// newline-delimited JSON objects. Both peers may issue requests and
// notifications, which is what lets the host call a plugin and a plugin call
// back into the host over the same connection.
package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// ErrClosed is returned for calls made on or after the connection closes.
var ErrClosed = errors.New("jsonrpc: connection closed")

// ErrWriteTimeout is returned when a message could not be written within
// WriteTimeout. The connection is closed when that happens.
var ErrWriteTimeout = errors.New("jsonrpc: write timed out; connection closed")

// WriteTimeout bounds how long writing one message may take, including waiting
// for earlier writes. A peer that stops reading would otherwise block every
// writer forever once the pipe fills. When it is exceeded the connection is
// closed, which for a plugin kills the process and unblocks the stuck write.
// Each connection reads it once, when it is created.
var WriteTimeout = 10 * time.Second

// NotificationQueueSize bounds the inbound notifications waiting for the
// handler. They are handled one at a time, in the order they arrived; when the
// handler falls this far behind, newer ones are dropped (see Dropped) instead
// of piling up as goroutines. Each connection reads it once, when it is
// created.
var NotificationQueueSize = 256

// Handler processes an inbound request or notification from the peer. For a
// notification (no response expected) the returned values are ignored. A nil
// Handler rejects inbound requests with a method-not-found error.
type Handler func(ctx context.Context, method string, params json.RawMessage) (any, error)

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("jsonrpc: %d %s", e.Code, e.Message) }

// message is any JSON-RPC frame. ID is kept raw because JSON-RPC 2.0 allows a
// string or a number, and a peer written in another language (JavaScript
// especially) commonly uses strings; it is echoed back byte-for-byte.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Conn is a JSON-RPC connection over a reader/writer pair.
type Conn struct {
	w       *bufio.Writer
	enc     *json.Encoder
	wmu     sync.Mutex
	handler Handler

	nextID  atomic.Uint64
	pending sync.Map // string(id) -> chan message

	closeOnce    sync.Once
	closed       chan struct{}
	closer       io.Closer
	writeTimeout time.Duration

	notes   chan message // inbound notifications, in arrival order
	dropped atomic.Uint64
}

// NewConn starts a connection reading from r and writing to w. handler may be
// nil. If closer is non-nil it is closed when the connection terminates (e.g. to
// reap a subprocess). The read loop runs in a background goroutine.
func NewConn(r io.Reader, w io.Writer, handler Handler, closer io.Closer) *Conn {
	bw := bufio.NewWriter(w)
	c := &Conn{
		w:       bw,
		enc:     json.NewEncoder(bw),
		handler: handler,
		closed:  make(chan struct{}),
		closer:  closer,

		writeTimeout: WriteTimeout,
		notes:        make(chan message, NotificationQueueSize),
	}
	go c.readLoop(r)
	go c.notificationLoop()
	return c
}

// Dropped reports how many inbound notifications were dropped because the
// handler had NotificationQueueSize of them still waiting.
func (c *Conn) Dropped() uint64 { return c.dropped.Load() }

// notificationLoop hands queued notifications to the handler one at a time.
// They are not handled on the read loop itself: a handler that makes a Call
// (a plugin asking Wings for something while handling an event) needs the read
// loop free to receive the response.
func (c *Conn) notificationLoop() {
	for m := range c.notes {
		c.serveNotification(m)
	}
}

func (c *Conn) readLoop(r io.Reader) {
	dec := json.NewDecoder(bufio.NewReader(r))
	for {
		var m message
		if err := dec.Decode(&m); err != nil {
			close(c.notes) // the read loop is the only sender
			c.close()
			return
		}
		hasID := len(m.ID) > 0 && string(m.ID) != "null"
		switch {
		case m.Method != "" && hasID:
			go c.serveRequest(m)
		case m.Method != "":
			select {
			case c.notes <- m:
			default:
				c.dropped.Add(1)
			}
		case hasID:
			if ch, ok := c.pending.LoadAndDelete(string(m.ID)); ok {
				ch.(chan message) <- m
			}
		}
	}
}

func (c *Conn) serveRequest(m message) {
	var (
		result any
		err    error
	)
	if c.handler == nil {
		err = &Error{Code: -32601, Message: "method not found"}
	} else {
		result, err = c.safeHandle(m.Method, m.Params)
	}

	resp := message{ID: m.ID}
	switch {
	case err != nil:
		var e *Error
		if errors.As(err, &e) {
			resp.Error = e
		} else {
			resp.Error = &Error{Code: -32000, Message: err.Error()}
		}
	default:
		raw, mErr := json.Marshal(result)
		if mErr != nil {
			resp.Error = &Error{Code: -32603, Message: mErr.Error()}
		} else {
			resp.Result = raw
		}
	}
	_ = c.write(resp)
}

func (c *Conn) serveNotification(m message) {
	if c.handler == nil {
		return
	}
	// A panic in a notification handler has no response to carry it; swallow it
	// so a faulty handler cannot take down the connection's read loop goroutine.
	defer func() { _ = recover() }()
	_, _ = c.handler(context.Background(), m.Method, m.Params)
}

// safeHandle invokes the handler with panic recovery. A panicking handler must
// not crash the process; it becomes an internal JSON-RPC error so the caller
// still gets a response and the connection stays alive.
func (c *Conn) safeHandle(method string, params json.RawMessage) (result any, err error) {
	defer func() {
		if r := recover(); r != nil {
			result = nil
			err = &Error{Code: -32603, Message: fmt.Sprintf("handler panic in %q", method)}
		}
	}()
	return c.handler(context.Background(), method, params)
}

func (c *Conn) write(m message) error {
	m.JSONRPC = "2.0"
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	done := make(chan error, 1)
	go func() {
		c.wmu.Lock()
		defer c.wmu.Unlock()
		select {
		case <-c.closed:
			done <- ErrClosed
			return
		default:
		}
		if err := c.enc.Encode(m); err != nil {
			done <- err
			return
		}
		done <- c.w.Flush()
	}()
	timer := time.NewTimer(c.writeTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-c.closed:
		return ErrClosed
	case <-timer.C:
		c.close()
		return ErrWriteTimeout
	}
}

// Call issues a request and waits for the response or ctx cancellation. result
// may be nil to discard the payload.
func (c *Conn) Call(ctx context.Context, method string, params any, result any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	id := strconv.FormatUint(c.nextID.Add(1), 10)
	ch := make(chan message, 1)
	c.pending.Store(id, ch)
	defer c.pending.Delete(id)

	if err := c.write(message{ID: json.RawMessage(id), Method: method, Params: raw}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return ErrClosed
	case resp := <-ch:
		if resp.Error != nil {
			return resp.Error
		}
		if result != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, result)
		}
		return nil
	}
}

// Notify sends a notification for which no response is expected.
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(message{Method: method, Params: raw})
}

// Done returns a channel closed when the connection terminates.
func (c *Conn) Done() <-chan struct{} { return c.closed }

// Close terminates the connection and reaps the underlying closer.
func (c *Conn) Close() error { c.close(); return nil }

func (c *Conn) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		if c.closer != nil {
			_ = c.closer.Close()
		}
	})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	return json.Marshal(params)
}

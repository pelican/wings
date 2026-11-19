package docker

import (
	"context"
	"net"
	"testing"
	"time"

	"emperror.dev/errors"
	"github.com/moby/moby/client"

	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/system"
)

// attachedEnvironment returns an environment attached to one end of a pipe,
// returning the other end, which receives the process input.
func attachedEnvironment(t *testing.T) (*Environment, net.Conn) {
	t.Helper()
	local, remote := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = remote.Close()
	})
	e := &Environment{meta: &Metadata{}, st: system.NewAtomicString(environment.ProcessRunningState)}
	e.SetStream(&client.HijackedResponse{Conn: local})
	return e, remote
}

// Commands may not contain control characters, other than tabs.
func TestSendCommandRejectsControlCharacters(t *testing.T) {
	e, input := attachedEnvironment(t)
	received := make(chan string, 1)
	go func() {
		b := make([]byte, 64)
		n, _ := input.Read(b)
		received <- string(b[:n])
	}()

	for _, c := range []string{"\x10\x11", "say \x10", "\x03", "\x1b[A", "\x7f"} {
		if err := e.SendCommand(c); err == nil {
			t.Errorf("expected command %q to be rejected", c)
		}
	}
	if err := e.SendCommand("say\thello world"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-received:
		if got != "say\thello world\n" {
			t.Fatalf("expected only the valid command to be sent, got %q", got)
		}
	case <-time.After(time.Second * 2):
		t.Fatal("expected the valid command to be sent")
	}
}

// Sending a command times out if the process does not read its input.
func TestSendCommandDoesNotBlockForever(t *testing.T) {
	previous := commandWriteTimeout
	commandWriteTimeout = time.Millisecond * 200
	t.Cleanup(func() { commandWriteTimeout = previous })

	e, _ := attachedEnvironment(t)
	done := make(chan error, 1)
	go func() { done <- e.SendCommand("say hello") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected an error when the command could not be written")
		}
	case <-time.After(time.Second * 3):
		t.Fatal("expected sending the command to time out")
	}
}

// A command that times out, whether waiting for the send lock or on the write
// itself, must report context.DeadlineExceeded. WaitForStop runs its forced
// SIGKILL fallback only for that error, so without this identity a stuck stop
// command would leave the container running in the stopping state.
func TestSendCommandTimeoutIsDeadlineExceeded(t *testing.T) {
	previous := commandWriteTimeout
	commandWriteTimeout = time.Millisecond * 100
	t.Cleanup(func() { commandWriteTimeout = previous })

	t.Run("blocked write", func(t *testing.T) {
		// Nothing reads the pipe, so the write blocks until its deadline.
		e, _ := attachedEnvironment(t)
		err := e.SendCommand("stop")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a deadline-exceeded error, got %v", err)
		}
	})

	t.Run("contended send lock", func(t *testing.T) {
		// Hold the send lock for longer than the timeout so the command cannot
		// acquire it and times out while waiting to send.
		e, _ := attachedEnvironment(t)
		e.sendMu.Lock()
		defer e.sendMu.Unlock()

		err := e.SendCommand("stop")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expected a deadline-exceeded error, got %v", err)
		}
	})
}

// A command holding several lines stops the server if any of its lines is the
// stop command, since the process reads each line as its own command.
func TestContainsStopCommand(t *testing.T) {
	cases := map[string]bool{
		"stop":             true,
		"x\nstop":          true,
		"x\r\nstop":        true,
		"stop\rx":          true,
		"say hi\n\nstop\n": true,
		"stopx":            false,
		"x stop":           false,
		" stop":            false,
		"say stop\nsay hi": false,
		"":                 false,
	}
	for c, want := range cases {
		if got := containsStopCommand(c, "stop"); got != want {
			t.Errorf("containsStopCommand(%q) = %v, want %v", c, got, want)
		}
	}
	if containsStopCommand("x\n", "") {
		t.Error("an empty stop command must never match")
	}
}

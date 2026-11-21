package server

import (
	"sync"
	"testing"
	"time"

	"github.com/pelican/wings/environment"
	"github.com/pelican/wings/remote"
)

// blockingStateEnvironment holds every call to State until it is released.
type blockingStateEnvironment struct {
	environment.ProcessEnvironment
	release chan struct{}
}

func (e *blockingStateEnvironment) State() string {
	<-e.release
	return environment.ProcessRunningState
}

// recordingStateEnvironment reports a fixed state and records every state the
// server tries to set.
type recordingStateEnvironment struct {
	environment.ProcessEnvironment
	mu     sync.Mutex
	state  string
	states []string
}

func (e *recordingStateEnvironment) State() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state
}

func (e *recordingStateEnvironment) SetState(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.states = append(e.states, s)
}

// Each line of console output is checked before the next one is read, instead
// of in a new goroutine per line.
func TestConsoleOutputIsCheckedAsItIsRead(t *testing.T) {
	setTestConfig(t)

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	env := &blockingStateEnvironment{release: make(chan struct{})}
	s.Environment = env

	done := make(chan struct{})
	go func() {
		s.processConsoleOutputEvent([]byte("line"))
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("expected the line to be checked before the next one is read")
	case <-time.After(100 * time.Millisecond):
	}

	close(env.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("expected the line to be processed once it could be checked")
	}
}

// A running server printing its own stop command must not be able to mark
// itself as offline, which would stop its disk limit from being enforced
// (GHSA-6pfj-2xp3-j4mm).
func TestConsoleOutputCannotMarkServerOffline(t *testing.T) {
	setTestConfig(t)

	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	env := &recordingStateEnvironment{state: environment.ProcessRunningState}
	s.Environment = env
	s.procConfig = &remote.ProcessConfiguration{
		Stop: remote.ProcessStopConfiguration{Type: remote.ProcessStopCommand, Value: "stop"},
	}

	for i := 0; i < 3; i++ {
		s.onConsoleOutput([]byte("stop"))
	}

	env.mu.Lock()
	defer env.mu.Unlock()
	if len(env.states) != 0 {
		t.Fatalf("expected console output not to change the server state, got %v", env.states)
	}
}

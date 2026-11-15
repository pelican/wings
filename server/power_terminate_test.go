package server

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pelican/wings/environment"
)

// terminateTestEnvironment reports a configurable process state and records
// termination requests.
type terminateTestEnvironment struct {
	environment.ProcessEnvironment
	state      atomic.Value
	terminated chan struct{}
}

func (e *terminateTestEnvironment) State() string { return e.state.Load().(string) }

func (e *terminateTestEnvironment) Terminate(context.Context, string) error {
	e.terminated <- struct{}{}
	return nil
}

func newTerminateTestServer(t *testing.T, state string) (*Server, *terminateTestEnvironment) {
	t.Helper()
	s, err := New(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.CtxCancel)
	env := &terminateTestEnvironment{terminated: make(chan struct{}, 1)}
	env.state.Store(state)
	s.Environment = env
	return s, env
}

func setTerminateBootWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := terminateBootWait
	terminateBootWait = d
	t.Cleanup(func() { terminateBootWait = prev })
}

// A kill sent while a start or restart is booting the server waits for the boot
// to finish instead of racing it, then terminates the process it started.
func TestTerminateWaitsForBootingServer(t *testing.T) {
	for _, state := range []string{environment.ProcessOfflineState, environment.ProcessStartingState} {
		t.Run(state, func(t *testing.T) {
			setTerminateBootWait(t, 5*time.Second)
			s, env := newTerminateTestServer(t, state)

			// Another power action (the start or restart) holds the lock.
			if err := s.powerLock.Acquire(); err != nil {
				t.Fatal(err)
			}

			done := make(chan error, 1)
			go func() { done <- s.HandlePowerAction(PowerActionTerminate) }()

			select {
			case <-env.terminated:
				t.Fatal("terminated while the server was still booting")
			case <-time.After(150 * time.Millisecond):
			}

			// The boot finishes and the server is now running.
			env.state.Store(environment.ProcessRunningState)
			s.powerLock.Release()

			select {
			case <-env.terminated:
			case <-time.After(2 * time.Second):
				t.Fatal("the kill was not processed after the boot finished")
			}
			if err := <-done; err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if s.ExecutingPowerAction() {
				t.Error("expected the power lock to be released after terminating")
			}
		})
	}
}

// A boot that never finishes does not prevent the server from being killed.
func TestTerminateFallsBackWhenBootDoesNotFinish(t *testing.T) {
	setTerminateBootWait(t, 100*time.Millisecond)
	s, env := newTerminateTestServer(t, environment.ProcessStartingState)
	if err := s.powerLock.Acquire(); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	if err := s.HandlePowerAction(PowerActionTerminate); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	select {
	case <-env.terminated:
	default:
		t.Fatal("expected the server to be terminated after the wait expired")
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("expected terminate to wait for the boot, returned after %s", elapsed)
	}
}

// A kill during a stuck stop keeps bypassing the power lock immediately.
func TestTerminateBypassesLockWhenNotBooting(t *testing.T) {
	for _, state := range []string{environment.ProcessRunningState, environment.ProcessStoppingState} {
		t.Run(state, func(t *testing.T) {
			setTerminateBootWait(t, 5*time.Second)
			s, env := newTerminateTestServer(t, state)
			if err := s.powerLock.Acquire(); err != nil {
				t.Fatal(err)
			}

			start := time.Now()
			if err := s.HandlePowerAction(PowerActionTerminate); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			select {
			case <-env.terminated:
			default:
				t.Fatal("expected the server to be terminated")
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Errorf("expected terminate to bypass the lock, took %s", elapsed)
			}
		})
	}
}
